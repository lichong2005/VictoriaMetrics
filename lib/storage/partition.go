package storage

import (
	"fmt"
	"io/ioutil"
	"math/bits"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/VictoriaMetrics/VictoriaMetrics/lib/bytesutil"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/encoding"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/fs"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/logger"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/memory"
	"golang.org/x/sys/unix"
)

// The maximum number of rows in a small part.
//
// Small part merges cannot be interrupted during server stop, so this value
// must be small enough to complete a merge
// of `maxRowsPerSmallPart * defaultPartsToMerge` rows in a reasonable amount
// of time (up to a a minute).
//
// Additionally, this number limits the maximum size of small parts storage.
// Production simultation shows that the required size of the storage
// may be estimated as:
//
//     maxRowsPerSmallPart * 2 * defaultPartsToMerge * mergeWorkers
//
const maxRowsPerSmallPart = 300e6

// The maximum number of rows per big part.
//
// This number limits the maximum time required for building big part.
// This time shouldn't exceed a few days.
const maxRowsPerBigPart = 1e12

// The maximum number of small parts in the partition.
const maxSmallPartsPerPartition = 256

// Default number of parts to merge at once.
//
// This number has been obtained empirically - it gives the lowest possible overhead.
// See appendPartsToMerge tests for details.
const defaultPartsToMerge = 15

// The final number of parts to merge at once.
//
// It must be smaller than defaultPartsToMerge.
// Lower value improves select performance at the cost of increased
// write amplification.
const finalPartsToMerge = 3

// getMaxRowsPerPartition returns the maximum number of rows that haven't been converted into parts yet.
func getMaxRawRowsPerPartition() int {
	maxRawRowsPerPartitionOnce.Do(func() {
		n := memory.Allowed() / 256 / int(unsafe.Sizeof(rawRow{}))
		if n < 1e4 {
			n = 1e4
		}
		if n > 500e3 {
			n = 500e3
		}
		maxRawRowsPerPartition = n
	})
	return maxRawRowsPerPartition
}

var (
	maxRawRowsPerPartition     int
	maxRawRowsPerPartitionOnce sync.Once
)

// The interval for flushing (converting) recent raw rows into parts,
// so they become visible to search.
const rawRowsFlushInterval = time.Second

// The interval for flushing inmemory parts to persistent storage,
// so they survive process crash.
const inmemoryPartsFlushInterval = 5 * time.Second

// partition represents a partition.
type partition struct {
	smallPartsPath string
	bigPartsPath   string

	// The callack that returns deleted metric ids which must be skipped during merge.
	getDeletedMetricIDs func() map[uint64]struct{}

	// Name is the name of the partition in the form YYYY_MM.
	name string

	// The time range for the partition. Usually this is a whole month.
	tr TimeRange

	// partsLock protects smallParts and bigParts.
	partsLock sync.Mutex

	// Contains all the inmemoryPart plus file-based parts
	// with small number of items (up to maxRowsCountPerSmallPart).
	smallParts []*partWrapper

	// Contains file-based parts with big number of items.
	bigParts []*partWrapper

	// rawRowsLock protects rawRows.
	rawRowsLock sync.Mutex

	// rawRows contains recently added rows that haven't been converted into parts yet.
	//
	// rawRows aren't used in search for performance reasons.
	rawRows []rawRow

	// rawRowsLastFlushTime is the last time rawRows are flushed.
	rawRowsLastFlushTime time.Time

	mergeIdx uint64

	snapshotLock sync.RWMutex

	stopCh chan struct{}

	smallPartsMergerWG     sync.WaitGroup
	bigPartsMergerWG       sync.WaitGroup
	rawRowsFlusherWG       sync.WaitGroup
	inmemoryPartsFlusherWG sync.WaitGroup

	activeBigMerges   uint64
	activeSmallMerges uint64
	bigMergesCount    uint64
	smallMergesCount  uint64
	bigRowsMerged     uint64
	smallRowsMerged   uint64
	bigRowsDeleted    uint64
	smallRowsDeleted  uint64

	smallAssistedMerges uint64
}

// partWrapper is a wrapper for the part.
type partWrapper struct {
	// The part itself.
	p *part

	// non-nil if the part is inmemoryPart.
	mp *inmemoryPart

	// The number of references to the part.
	refCount uint64

	// Whether the part is in merge now.
	isInMerge bool
}

func (pw *partWrapper) incRef() {
	atomic.AddUint64(&pw.refCount, 1)
}

func (pw *partWrapper) decRef() {
	n := atomic.AddUint64(&pw.refCount, ^uint64(0))
	if int64(n) < 0 {
		logger.Panicf("BUG: pw.refCount must be bigger than 0; got %d", int64(n))
	}
	if n > 0 {
		return
	}

	if pw.mp != nil {
		putInmemoryPart(pw.mp)
		pw.mp = nil
	}
	pw.p.MustClose()
	pw.p = nil
}

// createPartition creates new partition for the given timestamp and the given paths
// to small and big partitions.
func createPartition(timestamp int64, smallPartitionsPath, bigPartitionsPath string, getDeletedMetricIDs func() map[uint64]struct{}) (*partition, error) {
	name := timestampToPartitionName(timestamp)
	smallPartsPath := filepath.Clean(smallPartitionsPath) + "/" + name
	bigPartsPath := filepath.Clean(bigPartitionsPath) + "/" + name
	logger.Infof("creating a partition %q with smallPartsPath=%q, bigPartsPath=%q", name, smallPartsPath, bigPartsPath)

	if err := createPartitionDirs(smallPartsPath); err != nil {
		return nil, fmt.Errorf("cannot create directories for small parts %q: %s", smallPartsPath, err)
	}
	if err := createPartitionDirs(bigPartsPath); err != nil {
		return nil, fmt.Errorf("cannot create directories for big parts %q: %s", bigPartsPath, err)
	}

	pt := newPartition(name, smallPartsPath, bigPartsPath, getDeletedMetricIDs)
	pt.tr.fromPartitionTimestamp(timestamp)
	pt.startMergeWorkers()
	pt.startRawRowsFlusher()
	pt.startInmemoryPartsFlusher()

	logger.Infof("partition %q has been created", name)

	return pt, nil
}

// Drop drops all the data on the storage for the given pt.
//
// The pt must be detached from table before calling pt.Drop.
func (pt *partition) Drop() {
	logger.Infof("dropping partition %q at smallPartsPath=%q, bigPartsPath=%q", pt.name, pt.smallPartsPath, pt.bigPartsPath)

	if err := os.RemoveAll(pt.smallPartsPath); err != nil {
		logger.Panicf("FATAL: cannot remove small parts directory %q: %s", pt.smallPartsPath, err)
	}
	if err := os.RemoveAll(pt.bigPartsPath); err != nil {
		logger.Panicf("FATAL: cannot remove big parts directory %q: %s", pt.bigPartsPath, err)
	}

	logger.Infof("partition %q has been dropped", pt.name)
}

// openPartition opens the existing partition from the given paths.
func openPartition(smallPartsPath, bigPartsPath string, getDeletedMetricIDs func() map[uint64]struct{}) (*partition, error) {
	smallPartsPath = filepath.Clean(smallPartsPath)
	bigPartsPath = filepath.Clean(bigPartsPath)

	n := strings.LastIndexByte(smallPartsPath, '/')
	if n < 0 {
		return nil, fmt.Errorf("cannot find partition name from smallPartsPath %q; must be in the form /path/to/smallparts/YYYY_MM", smallPartsPath)
	}
	name := smallPartsPath[n+1:]

	if !strings.HasSuffix(bigPartsPath, "/"+name) {
		return nil, fmt.Errorf("patititon name in bigPartsPath %q doesn't match smallPartsPath %q; want %q", bigPartsPath, smallPartsPath, name)
	}

	smallParts, err := openParts(smallPartsPath, bigPartsPath, smallPartsPath)
	if err != nil {
		return nil, fmt.Errorf("cannot open small parts from %q: %s", smallPartsPath, err)
	}
	bigParts, err := openParts(smallPartsPath, bigPartsPath, bigPartsPath)
	if err != nil {
		mustCloseParts(smallParts)
		return nil, fmt.Errorf("cannot open big parts from %q: %s", bigPartsPath, err)
	}

	pt := newPartition(name, smallPartsPath, bigPartsPath, getDeletedMetricIDs)
	pt.smallParts = smallParts
	pt.bigParts = bigParts
	if err := pt.tr.fromPartitionName(name); err != nil {
		return nil, fmt.Errorf("cannot obtain partition time range from smallPartsPath %q: %s", smallPartsPath, err)
	}
	pt.startMergeWorkers()
	pt.startRawRowsFlusher()
	pt.startInmemoryPartsFlusher()

	return pt, nil
}

func newPartition(name, smallPartsPath, bigPartsPath string, getDeletedMetricIDs func() map[uint64]struct{}) *partition {
	return &partition{
		name:           name,
		smallPartsPath: smallPartsPath,
		bigPartsPath:   bigPartsPath,

		getDeletedMetricIDs: getDeletedMetricIDs,

		rawRows: getRawRowsMaxSize().rows,

		mergeIdx: uint64(time.Now().UnixNano()),
		stopCh:   make(chan struct{}),
	}
}

// partitionMetrics contains essential metrics for the partition.
type partitionMetrics struct {
	PendingRows uint64

	BigIndexBlocksCacheSize     uint64
	BigIndexBlocksCacheRequests uint64
	BigIndexBlocksCacheMisses   uint64

	SmallIndexBlocksCacheSize     uint64
	SmallIndexBlocksCacheRequests uint64
	SmallIndexBlocksCacheMisses   uint64

	BigRowsCount   uint64
	SmallRowsCount uint64

	BigBlocksCount   uint64
	SmallBlocksCount uint64

	BigPartsCount   uint64
	SmallPartsCount uint64

	ActiveBigMerges   uint64
	ActiveSmallMerges uint64

	BigMergesCount   uint64
	SmallMergesCount uint64

	BigRowsMerged   uint64
	SmallRowsMerged uint64

	BigRowsDeleted   uint64
	SmallRowsDeleted uint64

	BigPartsRefCount   uint64
	SmallPartsRefCount uint64

	SmallAssistedMerges uint64
}

// UpdateMetrics updates m with metrics from pt.
func (pt *partition) UpdateMetrics(m *partitionMetrics) {
	pt.rawRowsLock.Lock()
	m.PendingRows += uint64(len(pt.rawRows))
	m.SmallRowsCount += uint64(len(pt.rawRows))
	pt.rawRowsLock.Unlock()

	pt.partsLock.Lock()

	for _, pw := range pt.bigParts {
		p := pw.p

		m.BigIndexBlocksCacheSize += p.ibCache.Len()
		m.BigIndexBlocksCacheRequests += p.ibCache.Requests()
		m.BigIndexBlocksCacheMisses += p.ibCache.Misses()
		m.BigRowsCount += p.ph.RowsCount
		m.BigBlocksCount += p.ph.BlocksCount
		m.BigPartsRefCount += atomic.LoadUint64(&pw.refCount)
	}

	for _, pw := range pt.smallParts {
		p := pw.p

		m.SmallIndexBlocksCacheSize += p.ibCache.Len()
		m.SmallIndexBlocksCacheRequests += p.ibCache.Requests()
		m.SmallIndexBlocksCacheMisses += p.ibCache.Misses()
		m.SmallRowsCount += p.ph.RowsCount
		m.SmallBlocksCount += p.ph.BlocksCount
		m.SmallPartsRefCount += atomic.LoadUint64(&pw.refCount)
	}

	m.BigPartsCount += uint64(len(pt.bigParts))
	m.SmallPartsCount += uint64(len(pt.smallParts))

	pt.partsLock.Unlock()

	atomic.AddUint64(&m.BigIndexBlocksCacheRequests, atomic.LoadUint64(&bigIndexBlockCacheRequests))
	atomic.AddUint64(&m.BigIndexBlocksCacheMisses, atomic.LoadUint64(&bigIndexBlockCacheMisses))

	atomic.AddUint64(&m.SmallIndexBlocksCacheRequests, atomic.LoadUint64(&smallIndexBlockCacheRequests))
	atomic.AddUint64(&m.SmallIndexBlocksCacheMisses, atomic.LoadUint64(&smallIndexBlockCacheMisses))

	m.ActiveBigMerges += atomic.LoadUint64(&pt.activeBigMerges)
	m.ActiveSmallMerges += atomic.LoadUint64(&pt.activeSmallMerges)

	m.BigMergesCount += atomic.LoadUint64(&pt.bigMergesCount)
	m.SmallMergesCount += atomic.LoadUint64(&pt.smallMergesCount)

	m.BigRowsMerged += atomic.LoadUint64(&pt.bigRowsMerged)
	m.SmallRowsMerged += atomic.LoadUint64(&pt.smallRowsMerged)

	m.BigRowsDeleted += atomic.LoadUint64(&pt.bigRowsDeleted)
	m.SmallRowsDeleted += atomic.LoadUint64(&pt.smallRowsDeleted)

	m.SmallAssistedMerges += atomic.LoadUint64(&pt.smallAssistedMerges)
}

// AddRows adds the given rows to the partition pt.
//
// All the rows must fit the partition by timestamp range
// and must have valid PrecisionBits.
func (pt *partition) AddRows(rows []rawRow) {
	if len(rows) == 0 {
		return
	}

	// Validate all the rows.
	for i := range rows {
		r := &rows[i]
		if !pt.HasTimestamp(r.Timestamp) {
			logger.Panicf("BUG: row %+v has Timestamp outside partition %q range %+v", r, pt.smallPartsPath, &pt.tr)
		}
		if err := encoding.CheckPrecisionBits(r.PrecisionBits); err != nil {
			logger.Panicf("BUG: row %+v has invalid PrecisionBits: %s", r, err)
		}
	}

	// Try adding rows.
	var rrs []*rawRows
	pt.rawRowsLock.Lock()
	for {
		capacity := cap(pt.rawRows) - len(pt.rawRows)
		if capacity >= len(rows) {
			// Fast path - rows fit capacity.
			pt.rawRows = append(pt.rawRows, rows...)
			break
		}

		// Slow path - rows don't fit capacity.
		// Fill rawRows to capacity and convert it to a part.
		pt.rawRows = append(pt.rawRows, rows[:capacity]...)
		rows = rows[capacity:]
		rr := getRawRowsMaxSize()
		pt.rawRows, rr.rows = rr.rows, pt.rawRows
		rrs = append(rrs, rr)
		pt.rawRowsLastFlushTime = time.Now()
	}
	pt.rawRowsLock.Unlock()

	for _, rr := range rrs {
		pt.addRowsPart(rr.rows)
		putRawRows(rr)
	}
}

type rawRows struct {
	rows []rawRow
}

func getRawRowsMaxSize() *rawRows {
	size := getMaxRawRowsPerPartition()
	return getRawRowsWithSize(size)
}

func getRawRowsWithSize(size int) *rawRows {
	p, sizeRounded := getRawRowsPool(size)
	v := p.Get()
	if v == nil {
		return &rawRows{
			rows: make([]rawRow, 0, sizeRounded),
		}
	}
	return v.(*rawRows)
}

func putRawRows(rr *rawRows) {
	rr.rows = rr.rows[:0]
	size := cap(rr.rows)
	p, _ := getRawRowsPool(size)
	p.Put(rr)
}

func getRawRowsPool(size int) (*sync.Pool, int) {
	size--
	if size < 0 {
		size = 0
	}
	bucketIdx := 64 - bits.LeadingZeros64(uint64(size))
	if bucketIdx >= len(rawRowsPools) {
		bucketIdx = len(rawRowsPools) - 1
	}
	p := &rawRowsPools[bucketIdx]
	sizeRounded := 1 << uint(bucketIdx)
	return p, sizeRounded
}

var rawRowsPools [19]sync.Pool

func (pt *partition) addRowsPart(rows []rawRow) {
	if len(rows) == 0 {
		return
	}

	mp := getInmemoryPart()
	mp.InitFromRows(rows)

	// Make sure the part may be added.
	if mp.ph.MinTimestamp > mp.ph.MaxTimestamp {
		logger.Panicf("BUG: the part %q cannot be added to partition %q because its MinTimestamp exceeds MaxTimestamp; %d vs %d",
			&mp.ph, pt.smallPartsPath, mp.ph.MinTimestamp, mp.ph.MaxTimestamp)
	}
	if mp.ph.MinTimestamp < pt.tr.MinTimestamp {
		logger.Panicf("BUG: the part %q cannot be added to partition %q because of too small MinTimestamp; got %d; want at least %d",
			&mp.ph, pt.smallPartsPath, mp.ph.MinTimestamp, pt.tr.MinTimestamp)
	}
	if mp.ph.MaxTimestamp > pt.tr.MaxTimestamp {
		logger.Panicf("BUG: the part %q cannot be added to partition %q because of too big MaxTimestamp; got %d; want at least %d",
			&mp.ph, pt.smallPartsPath, mp.ph.MaxTimestamp, pt.tr.MaxTimestamp)
	}

	p, err := mp.NewPart()
	if err != nil {
		logger.Panicf("BUG: cannot create part from %q: %s", &mp.ph, err)
	}

	pw := &partWrapper{
		p:        p,
		mp:       mp,
		refCount: 1,
	}

	pt.partsLock.Lock()
	pt.smallParts = append(pt.smallParts, pw)
	ok := len(pt.smallParts) <= maxSmallPartsPerPartition
	pt.partsLock.Unlock()
	if ok {
		return
	}

	// The added part exceeds available limit. Help merging parts.
	err = pt.mergeSmallParts(false)
	if err == nil {
		atomic.AddUint64(&pt.smallAssistedMerges, 1)
		return
	}
	if err == errNothingToMerge || err == errForciblyStopped {
		return
	}
	logger.Panicf("FATAL: cannot merge small parts: %s", err)
}

// HasTimestamp returns true if the pt contains the given timestamp.
func (pt *partition) HasTimestamp(timestamp int64) bool {
	return timestamp >= pt.tr.MinTimestamp && timestamp <= pt.tr.MaxTimestamp
}

// GetParts appends parts snapshot to dst and returns it.
//
// The appended parts must be released with PutParts.
func (pt *partition) GetParts(dst []*partWrapper) []*partWrapper {
	pt.partsLock.Lock()
	for _, pw := range pt.smallParts {
		pw.incRef()
	}
	dst = append(dst, pt.smallParts...)
	for _, pw := range pt.bigParts {
		pw.incRef()
	}
	dst = append(dst, pt.bigParts...)
	pt.partsLock.Unlock()

	return dst
}

// PutParts releases the given pws obtained via GetParts.
func (pt *partition) PutParts(pws []*partWrapper) {
	for _, pw := range pws {
		pw.decRef()
	}
}

// MustClose closes the pt, so the app may safely exit.
//
// The pt must be detached from table before calling pt.MustClose.
func (pt *partition) MustClose() {
	close(pt.stopCh)

	logger.Infof("waiting for inmemory parts flusher to stop on %q...", pt.smallPartsPath)
	startTime := time.Now()
	pt.inmemoryPartsFlusherWG.Wait()
	logger.Infof("inmemory parts flusher stopped in %s on %q", time.Since(startTime), pt.smallPartsPath)

	logger.Infof("waiting for raw rows flusher to stop on %q...", pt.smallPartsPath)
	startTime = time.Now()
	pt.rawRowsFlusherWG.Wait()
	logger.Infof("raw rows flusher stopped in %s on %q", time.Since(startTime), pt.smallPartsPath)

	logger.Infof("waiting for small part mergers to stop on %q...", pt.smallPartsPath)
	startTime = time.Now()
	pt.smallPartsMergerWG.Wait()
	logger.Infof("small part mergers stopped in %s on %q", time.Since(startTime), pt.smallPartsPath)

	logger.Infof("waiting for big part mergers to stop on %q...", pt.bigPartsPath)
	startTime = time.Now()
	pt.bigPartsMergerWG.Wait()
	logger.Infof("big part mergers stopped in %s on %q", time.Since(startTime), pt.bigPartsPath)

	logger.Infof("flushing inmemory parts to files on %q...", pt.smallPartsPath)
	startTime = time.Now()

	// Flush raw rows the last time before exit.
	pt.flushRawRows(nil, true)

	// Flush inmemory parts to disk.
	var pws []*partWrapper
	pt.partsLock.Lock()
	for _, pw := range pt.smallParts {
		if pw.mp == nil {
			continue
		}
		if pw.isInMerge {
			logger.Panicf("BUG: the inmemory part %q mustn't be in merge after stopping small parts merger in the partition %q", &pw.mp.ph, pt.smallPartsPath)
		}
		pw.isInMerge = true
		pws = append(pws, pw)
	}
	pt.partsLock.Unlock()

	if err := pt.mergePartsOptimal(pws); err != nil {
		logger.Panicf("FATAL: cannot flush %d inmemory parts to files on %q: %s", len(pws), pt.smallPartsPath, err)
	}
	logger.Infof("%d inmemory parts have been flushed to files in %s on %q", len(pws), time.Since(startTime), pt.smallPartsPath)

	// Remove references to smallParts from the pt, so they may be eventually closed
	// after all the seraches are done.
	pt.partsLock.Lock()
	smallParts := pt.smallParts
	pt.smallParts = nil
	pt.partsLock.Unlock()

	for _, pw := range smallParts {
		pw.decRef()
	}

	// Remove references to bigParts from the pt, so they may be eventually closed
	// after all the searches are done.
	pt.partsLock.Lock()
	bigParts := pt.bigParts
	pt.bigParts = nil
	pt.partsLock.Unlock()

	for _, pw := range bigParts {
		pw.decRef()
	}
}

func (pt *partition) startRawRowsFlusher() {
	pt.rawRowsFlusherWG.Add(1)
	go func() {
		pt.rawRowsFlusher()
		pt.rawRowsFlusherWG.Done()
	}()
}

func (pt *partition) rawRowsFlusher() {
	var rawRows []rawRow
	t := time.NewTimer(rawRowsFlushInterval)
	for {
		select {
		case <-pt.stopCh:
			return
		case <-t.C:
			t.Reset(rawRowsFlushInterval)
		}

		rawRows = pt.flushRawRows(rawRows[:0], false)
	}
}

func (pt *partition) flushRawRows(newRawRows []rawRow, isFinal bool) []rawRow {
	oldRawRows := newRawRows[:0]
	mustFlush := false
	currentTime := time.Now()

	pt.rawRowsLock.Lock()
	if isFinal || currentTime.Sub(pt.rawRowsLastFlushTime) > rawRowsFlushInterval {
		mustFlush = true
		oldRawRows = pt.rawRows
		pt.rawRows = newRawRows[:0]
		pt.rawRowsLastFlushTime = currentTime
	}
	pt.rawRowsLock.Unlock()

	if mustFlush {
		pt.addRowsPart(oldRawRows)
	}
	return oldRawRows
}

func (pt *partition) startInmemoryPartsFlusher() {
	pt.inmemoryPartsFlusherWG.Add(1)
	go func() {
		pt.inmemoryPartsFlusher()
		pt.inmemoryPartsFlusherWG.Done()
	}()
}

func (pt *partition) inmemoryPartsFlusher() {
	t := time.NewTimer(inmemoryPartsFlushInterval)
	var pwsBuf []*partWrapper
	var err error
	for {
		select {
		case <-pt.stopCh:
			return
		case <-t.C:
			t.Reset(inmemoryPartsFlushInterval)
		}

		pwsBuf, err = pt.flushInmemoryParts(pwsBuf[:0], false)
		if err != nil {
			logger.Panicf("FATAL: cannot flush inmemory parts: %s", err)
		}
	}
}

func (pt *partition) flushInmemoryParts(dstPws []*partWrapper, force bool) ([]*partWrapper, error) {
	currentTime := time.Now()

	// Inmemory parts may present only in small parts.
	pt.partsLock.Lock()
	for _, pw := range pt.smallParts {
		if pw.mp == nil || pw.isInMerge {
			continue
		}
		if force || currentTime.Sub(pw.mp.creationTime) >= inmemoryPartsFlushInterval {
			pw.isInMerge = true
			dstPws = append(dstPws, pw)
		}
	}
	pt.partsLock.Unlock()

	if err := pt.mergePartsOptimal(dstPws); err != nil {
		return dstPws, fmt.Errorf("cannot merge %d inmemory parts: %s", len(dstPws), err)
	}
	return dstPws, nil
}

func (pt *partition) mergePartsOptimal(pws []*partWrapper) error {
	for len(pws) > defaultPartsToMerge {
		if err := pt.mergeParts(pws[:defaultPartsToMerge], nil); err != nil {
			return fmt.Errorf("cannot merge %d parts: %s", defaultPartsToMerge, err)
		}
		pws = pws[defaultPartsToMerge:]
	}
	if len(pws) > 0 {
		if err := pt.mergeParts(pws, nil); err != nil {
			return fmt.Errorf("cannot merge %d parts: %s", len(pws), err)
		}
	}
	return nil
}

var mergeWorkers = func() int {
	n := runtime.GOMAXPROCS(-1) / 2
	if n <= 0 {
		n = 1
	}
	return n
}()

func (pt *partition) startMergeWorkers() {
	for i := 0; i < mergeWorkers; i++ {
		pt.smallPartsMergerWG.Add(1)
		go func() {
			pt.smallPartsMerger()
			pt.smallPartsMergerWG.Done()
		}()
	}

	for i := 0; i < mergeWorkers; i++ {
		pt.bigPartsMergerWG.Add(1)
		go func() {
			pt.bigPartsMerger()
			pt.bigPartsMergerWG.Done()
		}()
	}
}

func (pt *partition) bigPartsMerger() {
	if err := pt.partsMerger(pt.mergeBigParts); err != nil {
		logger.Panicf("FATAL: unrecoverable error when merging big parts in the partition %q: %s", pt.bigPartsPath, err)
	}
}

func (pt *partition) smallPartsMerger() {
	if err := pt.partsMerger(pt.mergeSmallParts); err != nil {
		logger.Panicf("FATAL: unrecoverable error when merging small parts in the partition %q: %s", pt.smallPartsPath, err)
	}
}

const (
	minMergeSleepTime = time.Millisecond
	maxMergeSleepTime = time.Second
)

func (pt *partition) partsMerger(mergerFunc func(isFinal bool) error) error {
	sleepTime := minMergeSleepTime
	var lastMergeTime time.Time
	isFinal := false
	t := time.NewTimer(sleepTime)
	for {
		err := mergerFunc(isFinal)
		if err == nil {
			// Try merging additional parts.
			sleepTime = minMergeSleepTime
			lastMergeTime = time.Now()
			isFinal = false
			continue
		}
		if err == errForciblyStopped {
			// The merger has been stopped.
			return nil
		}
		if err != errNothingToMerge {
			return err
		}
		if time.Since(lastMergeTime) > 10*time.Second {
			// We have free time for merging into bigger parts.
			// This should improve select performance.
			lastMergeTime = time.Now()
			isFinal = true
			continue
		}

		// Nothing to merge. Sleep for a while and try again.
		sleepTime *= 2
		if sleepTime > maxMergeSleepTime {
			sleepTime = maxMergeSleepTime
		}
		select {
		case <-pt.stopCh:
			return nil
		case <-t.C:
			t.Reset(sleepTime)
		}
	}
}

func (pt *partition) maxOutPartRows() uint64 {
	freeSpace := mustGetFreeDiskSpace(pt.bigPartsPath)

	// Calculate the maximum number of rows in the output merge part
	// by dividing the freeSpace by the number of concurrent
	// mergeWorkers for big parts.
	// This assumes each row is compressed into 1 byte. Production
	// simulation shows that each row usually occupies up to 0.5 bytes,
	// so this is quite safe assumption.
	return freeSpace / uint64(mergeWorkers)
}

func mustGetFreeDiskSpace(path string) uint64 {
	// Try obtaining the cache value at first.
	freeSpaceMapLock.Lock()
	defer freeSpaceMapLock.Unlock()

	e, ok := freeSpaceMap[path]
	if ok && time.Since(e.updateTime) < time.Second {
		// Fast path - the entry is fresh.
		return e.freeSpace
	}

	// Slow path.
	// Determine the amount of free space on bigPartsPath.
	d, err := os.Open(path)
	if err != nil {
		logger.Panicf("FATAL: cannot determine free disk space on %q: %s", path, err)
	}
	defer fs.MustClose(d)

	fd := d.Fd()
	var stat unix.Statfs_t
	if err := unix.Fstatfs(int(fd), &stat); err != nil {
		logger.Panicf("FATAL: cannot determine free disk space on %q: %s", path, err)
	}
	e.freeSpace = stat.Bavail * uint64(stat.Bsize)
	e.updateTime = time.Now()
	freeSpaceMap[path] = e
	return e.freeSpace
}

var (
	freeSpaceMap     = make(map[string]freeSpaceEntry)
	freeSpaceMapLock sync.Mutex
)

type freeSpaceEntry struct {
	updateTime time.Time
	freeSpace  uint64
}

func (pt *partition) mergeBigParts(isFinal bool) error {
	maxRows := pt.maxOutPartRows()
	if maxRows > maxRowsPerBigPart {
		maxRows = maxRowsPerBigPart
	}

	pt.partsLock.Lock()
	pws := getPartsToMerge(pt.bigParts, maxRows, isFinal)
	pt.partsLock.Unlock()

	if len(pws) == 0 {
		return errNothingToMerge
	}

	atomic.AddUint64(&pt.bigMergesCount, 1)
	atomic.AddUint64(&pt.activeBigMerges, 1)
	err := pt.mergeParts(pws, pt.stopCh)
	atomic.AddUint64(&pt.activeBigMerges, ^uint64(0))

	return err
}

func (pt *partition) mergeSmallParts(isFinal bool) error {
	maxRows := uint64(maxRowsPerSmallPart * defaultPartsToMerge)

	pt.partsLock.Lock()
	pws := getPartsToMerge(pt.smallParts, maxRows, isFinal)
	pt.partsLock.Unlock()

	if len(pws) == 0 {
		return errNothingToMerge
	}

	atomic.AddUint64(&pt.smallMergesCount, 1)
	atomic.AddUint64(&pt.activeSmallMerges, 1)
	err := pt.mergeParts(pws, pt.stopCh)
	atomic.AddUint64(&pt.activeSmallMerges, ^uint64(0))

	return err
}

var errNothingToMerge = fmt.Errorf("nothing to merge")

func (pt *partition) mergeParts(pws []*partWrapper, stopCh <-chan struct{}) error {
	if len(pws) == 0 {
		// Nothing to merge.
		return errNothingToMerge
	}

	defer func() {
		// Remove isInMerge flag from pws.
		pt.partsLock.Lock()
		for _, pw := range pws {
			if !pw.isInMerge {
				logger.Panicf("BUG: missing isInMerge flag on the part %q", pw.p.path)
			}
			pw.isInMerge = false
		}
		pt.partsLock.Unlock()
	}()

	startTime := time.Now()

	// Prepare BlockStreamReaders for source parts.
	bsrs := make([]*blockStreamReader, 0, len(pws))
	defer func() {
		for _, bsr := range bsrs {
			putBlockStreamReader(bsr)
		}
	}()
	for _, pw := range pws {
		bsr := getBlockStreamReader()
		if pw.mp != nil {
			bsr.InitFromInmemoryPart(pw.mp)
		} else {
			if err := bsr.InitFromFilePart(pw.p.path); err != nil {
				return fmt.Errorf("cannot open source part for merging: %s", err)
			}
		}
		bsrs = append(bsrs, bsr)
	}

	outRowsCount := uint64(0)
	for _, pw := range pws {
		outRowsCount += pw.p.ph.RowsCount
	}
	isBigPart := outRowsCount > maxRowsPerSmallPart
	nocache := isBigPart

	// Prepare BlockStreamWriter for destination part.
	ptPath := pt.smallPartsPath
	if isBigPart {
		ptPath = pt.bigPartsPath
	}
	ptPath = filepath.Clean(ptPath)
	mergeIdx := pt.nextMergeIdx()
	tmpPartPath := fmt.Sprintf("%s/tmp/%016X", ptPath, mergeIdx)
	bsw := getBlockStreamWriter()
	compressLevel := getCompressLevelForRowsCount(outRowsCount)
	if err := bsw.InitFromFilePart(tmpPartPath, nocache, compressLevel); err != nil {
		return fmt.Errorf("cannot create destination part %q: %s", tmpPartPath, err)
	}

	// Merge parts.
	var ph partHeader
	rowsMerged := &pt.smallRowsMerged
	rowsDeleted := &pt.smallRowsDeleted
	if isBigPart {
		rowsMerged = &pt.bigRowsMerged
		rowsDeleted = &pt.bigRowsDeleted
	}
	dmis := pt.getDeletedMetricIDs()
	err := mergeBlockStreams(&ph, bsw, bsrs, stopCh, rowsMerged, dmis, rowsDeleted)
	putBlockStreamWriter(bsw)
	if err != nil {
		if err == errForciblyStopped {
			return err
		}
		return fmt.Errorf("error when merging parts to %q: %s", tmpPartPath, err)
	}

	// Close bsrs.
	for _, bsr := range bsrs {
		putBlockStreamReader(bsr)
	}
	bsrs = nil

	// Create a transaction for atomic deleting old parts and moving
	// new part to its destination place.
	var bb bytesutil.ByteBuffer
	for _, pw := range pws {
		if pw.mp == nil {
			fmt.Fprintf(&bb, "%s\n", pw.p.path)
		}
	}
	dstPartPath := ""
	if ph.RowsCount > 0 {
		// The destination part may have no rows if they are deleted
		// during the merge due to dmis.
		dstPartPath = ph.Path(ptPath, mergeIdx)
	}
	fmt.Fprintf(&bb, "%s -> %s\n", tmpPartPath, dstPartPath)
	txnPath := fmt.Sprintf("%s/txn/%016X", ptPath, mergeIdx)
	if err := fs.WriteFile(txnPath, bb.B); err != nil {
		return fmt.Errorf("cannot create transaction file %q: %s", txnPath, err)
	}

	// Run the created transaction.
	if err := runTransaction(&pt.snapshotLock, pt.smallPartsPath, pt.bigPartsPath, txnPath); err != nil {
		return fmt.Errorf("cannot execute transaction %q: %s", txnPath, err)
	}

	var newPW *partWrapper
	if len(dstPartPath) > 0 {
		// Open the merged part if it is non-empty.
		newP, err := openFilePart(dstPartPath)
		if err != nil {
			return fmt.Errorf("cannot open merged part %q: %s", dstPartPath, err)
		}
		newPW = &partWrapper{
			p:        newP,
			refCount: 1,
		}
	}

	// Atomically remove old parts and add new part.
	m := make(map[*partWrapper]bool, len(pws))
	for _, pw := range pws {
		m[pw] = true
	}
	if len(m) != len(pws) {
		logger.Panicf("BUG: %d duplicate parts found in the merge of %d parts", len(pws)-len(m), len(pws))
	}
	removedSmallParts := 0
	removedBigParts := 0
	pt.partsLock.Lock()
	pt.smallParts, removedSmallParts = removeParts(pt.smallParts, m)
	pt.bigParts, removedBigParts = removeParts(pt.bigParts, m)
	if newPW != nil {
		if isBigPart {
			pt.bigParts = append(pt.bigParts, newPW)
		} else {
			pt.smallParts = append(pt.smallParts, newPW)
		}
	}
	pt.partsLock.Unlock()
	if removedSmallParts+removedBigParts != len(m) {
		logger.Panicf("BUG: unexpected number of parts removed; got %d, want %d", removedSmallParts+removedBigParts, len(m))
	}

	// Remove partition references from old parts.
	for _, pw := range pws {
		pw.decRef()
	}

	d := time.Since(startTime)
	if d > 10*time.Second {
		logger.Infof("merged %d rows in %s at %d rows/sec to %q", outRowsCount, d, int(float64(outRowsCount)/d.Seconds()), dstPartPath)
	}

	return nil
}

func getCompressLevelForRowsCount(rowsCount uint64) int {
	if rowsCount <= 1<<19 {
		return 1
	}
	if rowsCount <= 1<<22 {
		return 2
	}
	if rowsCount <= 1<<25 {
		return 3
	}
	if rowsCount <= 1<<28 {
		return 4
	}
	return 5
}

func (pt *partition) nextMergeIdx() uint64 {
	return atomic.AddUint64(&pt.mergeIdx, 1)
}

func removeParts(pws []*partWrapper, partsToRemove map[*partWrapper]bool) ([]*partWrapper, int) {
	removedParts := 0
	dst := pws[:0]
	for _, pw := range pws {
		if partsToRemove[pw] {
			removedParts++
			continue
		}
		dst = append(dst, pw)
	}
	return dst, removedParts
}

// getPartsToMerge returns optimal parts to merge from pws.
//
// The returned rows will contain less than maxRows rows.
func getPartsToMerge(pws []*partWrapper, maxRows uint64, isFinal bool) []*partWrapper {
	pwsRemaining := make([]*partWrapper, 0, len(pws))
	for _, pw := range pws {
		if !pw.isInMerge {
			pwsRemaining = append(pwsRemaining, pw)
		}
	}
	maxPartsToMerge := defaultPartsToMerge
	var pms []*partWrapper
	if isFinal {
		for len(pms) == 0 && maxPartsToMerge >= finalPartsToMerge {
			pms = appendPartsToMerge(pms[:0], pwsRemaining, maxPartsToMerge, maxRows)
			maxPartsToMerge--
		}
	} else {
		pms = appendPartsToMerge(pms[:0], pwsRemaining, maxPartsToMerge, maxRows)
	}
	for _, pw := range pms {
		if pw.isInMerge {
			logger.Panicf("BUG: partWrapper.isInMerge cannot be set")
		}
		pw.isInMerge = true
	}
	return pms
}

// appendPartsToMerge finds optimal parts to merge from src, appends
// them to dst and returns the result.
func appendPartsToMerge(dst, src []*partWrapper, maxPartsToMerge int, maxRows uint64) []*partWrapper {
	if len(src) < 2 {
		// There is no need in merging zero or one part :)
		return dst
	}
	if maxPartsToMerge < 2 {
		logger.Panicf("BUG: maxPartsToMerge cannot be smaller than 2; got %d", maxPartsToMerge)
	}

	// Filter out too big parts.
	// This should reduce N for O(n^2) algorithm below.
	maxInPartRows := maxRows / 2
	tmp := make([]*partWrapper, 0, len(src))
	for _, pw := range src {
		if pw.p.ph.RowsCount > maxInPartRows {
			continue
		}
		tmp = append(tmp, pw)
	}
	src = tmp

	// Sort src parts by rows count and backwards timestamp.
	// This should improve adjanced points' locality in the merged parts.
	sort.Slice(src, func(i, j int) bool {
		a := &src[i].p.ph
		b := &src[j].p.ph
		if a.RowsCount < b.RowsCount {
			return true
		}
		if a.RowsCount > b.RowsCount {
			return false
		}
		return a.MinTimestamp > b.MinTimestamp
	})

	n := maxPartsToMerge
	if len(src) < n {
		n = len(src)
	}

	// Exhaustive search for parts giving the lowest write amplification
	// when merged.
	var pws []*partWrapper
	maxM := float64(0)
	for i := 2; i <= n; i++ {
		for j := 0; j <= len(src)-i; j++ {
			rowsSum := uint64(0)
			for _, pw := range src[j : j+i] {
				rowsSum += pw.p.ph.RowsCount
			}
			if rowsSum > maxRows {
				continue
			}
			m := float64(rowsSum) / float64(src[j+i-1].p.ph.RowsCount)
			if m < maxM {
				continue
			}
			maxM = m
			pws = src[j : j+i]
		}
	}

	minM := float64(maxPartsToMerge / 2)
	if minM < 2 {
		minM = 2
	}
	if maxM < minM {
		// There is no sense in merging parts with too small m.
		return dst
	}

	return append(dst, pws...)
}

func openParts(pathPrefix1, pathPrefix2, path string) ([]*partWrapper, error) {
	// Verify that the directory for the parts exists.
	d, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("cannot open directory %q: %s", path, err)
	}
	defer fs.MustClose(d)

	// Run remaining transactions and cleanup /txn and /tmp directories.
	// Snapshots cannot be created yet, so use fakeSnapshotLock.
	var fakeSnapshotLock sync.RWMutex
	if err := runTransactions(&fakeSnapshotLock, pathPrefix1, pathPrefix2, path); err != nil {
		return nil, fmt.Errorf("cannot run transactions from %q: %s", path, err)
	}

	txnDir := path + "/txn"
	if err := os.RemoveAll(txnDir); err != nil {
		return nil, fmt.Errorf("cannot delete transaction directory %q: %s", txnDir, err)
	}
	tmpDir := path + "/tmp"
	if err := os.RemoveAll(tmpDir); err != nil {
		return nil, fmt.Errorf("cannot remove temporary directory %q: %s", tmpDir, err)
	}
	if err := createPartitionDirs(path); err != nil {
		return nil, fmt.Errorf("cannot create directories for partition %q: %s", path, err)
	}

	// Open parts.
	fis, err := d.Readdir(-1)
	if err != nil {
		return nil, fmt.Errorf("cannot read directory %q: %s", d.Name(), err)
	}
	var pws []*partWrapper
	for _, fi := range fis {
		if !fs.IsDirOrSymlink(fi) {
			// Skip non-directories.
			continue
		}
		fn := fi.Name()
		if fn == "tmp" || fn == "txn" || fn == "snapshots" {
			// "snapshots" dir is skipped for backwards compatibility. Now it is unused.
			// Skip special dirs.
			continue
		}
		partPath := path + "/" + fn
		startTime := time.Now()
		p, err := openFilePart(partPath)
		if err != nil {
			mustCloseParts(pws)
			return nil, fmt.Errorf("cannot open part %q: %s", partPath, err)
		}
		d := time.Since(startTime)
		logger.Infof("opened part %q in %s", partPath, d)

		pw := &partWrapper{
			p:        p,
			refCount: 1,
		}
		pws = append(pws, pw)
	}

	return pws, nil
}

func mustCloseParts(pws []*partWrapper) {
	for _, pw := range pws {
		if pw.refCount != 1 {
			logger.Panicf("BUG: unexpected refCount when closing part %q: %d; want 1", &pw.p.ph, pw.refCount)
		}
		pw.p.MustClose()
	}
}

// CreateSnapshotAt creates pt snapshot at the given smallPath and bigPath dirs.
//
// Snapshot is created using linux hard links, so it is usually created
// very quickly.
func (pt *partition) CreateSnapshotAt(smallPath, bigPath string) error {
	logger.Infof("creating partition snapshot of %q and %q...", pt.smallPartsPath, pt.bigPartsPath)
	startTime := time.Now()

	// Flush inmemory data to disk.
	pt.flushRawRows(nil, true)
	if _, err := pt.flushInmemoryParts(nil, true); err != nil {
		return fmt.Errorf("cannot flush inmemory parts: %s", err)
	}

	// The snapshot must be created under the lock in order to prevent from
	// concurrent modifications via runTransaction.
	pt.snapshotLock.Lock()
	defer pt.snapshotLock.Unlock()

	if err := pt.createSnapshot(pt.smallPartsPath, smallPath); err != nil {
		return fmt.Errorf("cannot create snapshot for %q: %s", pt.smallPartsPath, err)
	}
	if err := pt.createSnapshot(pt.bigPartsPath, bigPath); err != nil {
		return fmt.Errorf("cannot create snapshot for %q: %s", pt.bigPartsPath, err)
	}

	logger.Infof("created partition snapshot of %q and %q at %q and %q in %s", pt.smallPartsPath, pt.bigPartsPath, smallPath, bigPath, time.Since(startTime))
	return nil
}

func (pt *partition) createSnapshot(srcDir, dstDir string) error {
	if err := fs.MkdirAllFailIfExist(dstDir); err != nil {
		return fmt.Errorf("cannot create snapshot dir %q: %s", dstDir, err)
	}

	d, err := os.Open(srcDir)
	if err != nil {
		return fmt.Errorf("cannot open difrectory: %s", err)
	}
	defer fs.MustClose(d)

	fis, err := d.Readdir(-1)
	if err != nil {
		return fmt.Errorf("cannot read directory: %s", err)
	}
	for _, fi := range fis {
		if !fs.IsDirOrSymlink(fi) {
			// Skip non-directories.
			continue
		}
		fn := fi.Name()
		if fn == "tmp" || fn == "txn" {
			// Skip special dirs.
			continue
		}
		srcPartPath := srcDir + "/" + fn
		dstPartPath := dstDir + "/" + fn
		if err := fs.HardLinkFiles(srcPartPath, dstPartPath); err != nil {
			return fmt.Errorf("cannot create hard links from %q to %q: %s", srcPartPath, dstPartPath, err)
		}
	}

	fs.SyncPath(dstDir)
	fs.SyncPath(filepath.Dir(dstDir))

	return nil
}

func runTransactions(txnLock *sync.RWMutex, pathPrefix1, pathPrefix2, path string) error {
	txnDir := path + "/txn"
	d, err := os.Open(txnDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("cannot open %q: %s", txnDir, err)
	}
	defer fs.MustClose(d)

	fis, err := d.Readdir(-1)
	if err != nil {
		return fmt.Errorf("cannot read directory %q: %s", d.Name(), err)
	}

	// Sort transaction files by id.
	sort.Slice(fis, func(i, j int) bool {
		return fis[i].Name() < fis[j].Name()
	})

	for _, fi := range fis {
		txnPath := txnDir + "/" + fi.Name()
		if err := runTransaction(txnLock, pathPrefix1, pathPrefix2, txnPath); err != nil {
			return fmt.Errorf("cannot run transaction from %q: %s", txnPath, err)
		}
	}
	return nil
}

func runTransaction(txnLock *sync.RWMutex, pathPrefix1, pathPrefix2, txnPath string) error {
	// The transaction must be run under read lock in order to provide
	// consistent snapshots with partition.CreateSnapshot().
	txnLock.RLock()
	defer txnLock.RUnlock()

	data, err := ioutil.ReadFile(txnPath)
	if err != nil {
		return fmt.Errorf("cannot read transaction file: %s", err)
	}
	if len(data) > 0 && data[len(data)-1] == '\n' {
		data = data[:len(data)-1]
	}
	paths := strings.Split(string(data), "\n")

	if len(paths) == 0 {
		return fmt.Errorf("empty transaction")
	}
	rmPaths := paths[:len(paths)-1]
	mvPaths := strings.Split(paths[len(paths)-1], " -> ")
	if len(mvPaths) != 2 {
		return fmt.Errorf("invalid last line in the transaction file: got %q; must contain `srcPath -> dstPath`", paths[len(paths)-1])
	}

	// Remove old paths. It is OK if certain paths don't exist.
	for _, path := range rmPaths {
		path, err := validatePath(pathPrefix1, pathPrefix2, path)
		if err != nil {
			return fmt.Errorf("invalid path to remove: %s", err)
		}
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("cannot remove %q: %s", path, err)
		}
	}

	// Move the new part to new directory.
	srcPath := mvPaths[0]
	dstPath := mvPaths[1]
	srcPath, err = validatePath(pathPrefix1, pathPrefix2, srcPath)
	if err != nil {
		return fmt.Errorf("invalid source path to rename: %s", err)
	}
	if len(dstPath) > 0 {
		// Move srcPath to dstPath.
		dstPath, err = validatePath(pathPrefix1, pathPrefix2, dstPath)
		if err != nil {
			return fmt.Errorf("invalid destination path to rename: %s", err)
		}
		if fs.IsPathExist(srcPath) {
			if err := os.Rename(srcPath, dstPath); err != nil {
				return fmt.Errorf("cannot rename %q to %q: %s", srcPath, dstPath, err)
			}
		} else {
			// Verify dstPath exists.
			if !fs.IsPathExist(dstPath) {
				return fmt.Errorf("cannot find both source and destination paths: %q -> %q", srcPath, dstPath)
			}
		}
	} else {
		// Just remove srcPath.
		if err := os.RemoveAll(srcPath); err != nil {
			return fmt.Errorf("cannot remove %q: %s", srcPath, err)
		}
	}

	// Flush pathPrefix* directory metadata to the underying storage.
	fs.SyncPath(pathPrefix1)
	fs.SyncPath(pathPrefix2)

	// Remove the transaction file.
	if err := os.Remove(txnPath); err != nil {
		return fmt.Errorf("cannot remove transaction file: %s", err)
	}

	return nil
}

func validatePath(pathPrefix1, pathPrefix2, path string) (string, error) {
	var err error

	pathPrefix1, err = filepath.Abs(pathPrefix1)
	if err != nil {
		return path, fmt.Errorf("cannot determine absolute path for pathPrefix1=%q: %s", pathPrefix1, err)
	}
	pathPrefix2, err = filepath.Abs(pathPrefix2)
	if err != nil {
		return path, fmt.Errorf("cannot determine absolute path for pathPrefix2=%q: %s", pathPrefix2, err)
	}

	path, err = filepath.Abs(path)
	if err != nil {
		return path, fmt.Errorf("cannot determine absolute path for %q: %s", path, err)
	}
	if !strings.HasPrefix(path, pathPrefix1+"/") && !strings.HasPrefix(path, pathPrefix2+"/") {
		return path, fmt.Errorf("invalid path %q; must start with either %q or %q", path, pathPrefix1+"/", pathPrefix2+"/")
	}
	return path, nil
}

func createPartitionDirs(path string) error {
	path = filepath.Clean(path)
	txnPath := path + "/txn"
	if err := fs.MkdirAllFailIfExist(txnPath); err != nil {
		return fmt.Errorf("cannot create txn directory %q: %s", txnPath, err)
	}
	tmpPath := path + "/tmp"
	if err := fs.MkdirAllFailIfExist(tmpPath); err != nil {
		return fmt.Errorf("cannot create tmp directory %q: %s", tmpPath, err)
	}
	fs.SyncPath(path)
	return nil
}