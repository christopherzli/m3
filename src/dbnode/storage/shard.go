// Copyright (c) 2020 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package storage

import (
	"container/list"
	"errors"
	"fmt"
	"io"
	"math"
	"sync"
	"time"

	"github.com/m3db/m3/src/dbnode/generated/proto/pagetoken"
	"github.com/m3db/m3/src/dbnode/namespace"
	"github.com/m3db/m3/src/dbnode/persist"
	"github.com/m3db/m3/src/dbnode/persist/fs"
	"github.com/m3db/m3/src/dbnode/retention"
	"github.com/m3db/m3/src/dbnode/runtime"
	"github.com/m3db/m3/src/dbnode/storage/block"
	"github.com/m3db/m3/src/dbnode/storage/bootstrap"
	"github.com/m3db/m3/src/dbnode/storage/bootstrap/result"
	"github.com/m3db/m3/src/dbnode/storage/index"
	"github.com/m3db/m3/src/dbnode/storage/index/convert"
	"github.com/m3db/m3/src/dbnode/storage/repair"
	"github.com/m3db/m3/src/dbnode/storage/series"
	"github.com/m3db/m3/src/dbnode/tracepoint"
	"github.com/m3db/m3/src/dbnode/ts"
	"github.com/m3db/m3/src/dbnode/ts/writes"
	"github.com/m3db/m3/src/dbnode/x/xio"
	"github.com/m3db/m3/src/m3ninx/doc"
	"github.com/m3db/m3/src/x/checked"
	"github.com/m3db/m3/src/x/clock"
	"github.com/m3db/m3/src/x/context"
	xerrors "github.com/m3db/m3/src/x/errors"
	"github.com/m3db/m3/src/x/ident"
	"github.com/m3db/m3/src/x/instrument"
	xresource "github.com/m3db/m3/src/x/resource"
	xtime "github.com/m3db/m3/src/x/time"

	"github.com/gogo/protobuf/proto"
	"github.com/opentracing/opentracing-go/log"
	"github.com/uber-go/tally"
	"go.uber.org/zap"
)

const (
	shardIterateBatchPercent = 0.01
	shardIterateBatchMinSize = 16
	metricLabelName          = "__name__"
)

var (
	errShardEntryNotFound         = errors.New("shard entry not found")
	errShardNotOpen               = errors.New("shard is not open")
	errShardAlreadyTicking        = errors.New("shard is already ticking")
	errShardClosingTickTerminated = errors.New("shard is closing, terminating tick")
	errShardInvalidPageToken      = errors.New("shard could not unmarshal page token")
	errShardIsNotBootstrapped     = errors.New("shard is not bootstrapped")
	errShardAlreadyBootstrapped   = errors.New("shard is already bootstrapped")
	errFlushStateIsNotInitialized = errors.New("shard flush state is not initialized")
	errTriedToLoadNilSeries       = errors.New("tried to load nil series into shard")

	// ErrDatabaseLoadLimitHit is the error returned when the database load limit
	// is hit or exceeded.
	ErrDatabaseLoadLimitHit = errors.New("error loading series, database load limit hit")

	emptyDoc = doc.Metadata{}
)

type filesetsFn func(
	filePathPrefix string,
	namespace ident.ID,
	shardID uint32,
) (fs.FileSetFilesSlice, error)

type filesetPathsBeforeFn func(
	filePathPrefix string,
	namespace ident.ID,
	shardID uint32,
	t xtime.UnixNano,
) ([]string, error)

type tickPolicy int

const (
	tickPolicyRegular tickPolicy = iota
	tickPolicyCloseShard
)

type dbShardState int

const (
	dbShardStateOpen dbShardState = iota
	dbShardStateClosing
)

type dbShard struct {
	sync.RWMutex
	block.DatabaseBlockRetriever
	opts                     Options
	seriesOpts               series.Options
	nowFn                    clock.NowFn
	state                    dbShardState
	namespace                namespace.Metadata
	seriesBlockRetriever     series.QueryableBlockRetriever
	seriesOnRetrieveBlock    block.OnRetrieveBlock
	namespaceReaderMgr       databaseNamespaceReaderManager
	increasingIndex          increasingIndex
	seriesPool               series.DatabaseSeriesPool
	reverseIndex             NamespaceIndex
	insertQueue              *dbShardInsertQueue
	lookup                   *shardMap
	list                     *list.List
	bootstrapState           BootstrapState
	newMergerFn              fs.NewMergerFn
	newFSMergeWithMemFn      newFSMergeWithMemFn
	filesetsFn               filesetsFn
	filesetPathsBeforeFn     filesetPathsBeforeFn
	deleteFilesFn            deleteFilesFn
	snapshotFilesFn          snapshotFilesFn
	newReaderFn              fs.NewReaderFn
	sleepFn                  func(time.Duration)
	identifierPool           ident.Pool
	contextPool              context.Pool
	flushState               shardFlushState
	tickWg                   *sync.WaitGroup
	runtimeOptsListenClosers []xresource.SimpleCloser
	currRuntimeOptions       dbShardRuntimeOptions
	logger                   *zap.Logger
	metrics                  dbShardMetrics
	tileAggregator           TileAggregator
	ticking                  bool
	shard                    uint32
	coldWritesEnabled        bool
	indexEnabled             bool

	entryMetrics *EntryMetrics
}

// NB(r): dbShardRuntimeOptions does not contain its own
// mutex as some of the variables are needed each write
// which already at least acquires read lock from the shard
// mutex, so to keep the lock acquisitions to a minimum
// these are protected under the same shard mutex.
type dbShardRuntimeOptions struct {
	writeNewSeriesAsync      bool
	tickSleepSeriesBatchSize int
	tickSleepPerSeries       time.Duration
}

type dbShardMetrics struct {
	create                              tally.Counter
	close                               tally.Counter
	closeStart                          tally.Counter
	closeLatency                        tally.Timer
	seriesTicked                        tally.Gauge
	insertAsyncInsertErrors             tally.Counter
	insertAsyncWriteInternalErrors      tally.Counter
	insertAsyncWriteInvalidParamsErrors tally.Counter
	insertAsyncIndexErrors              tally.Counter
	snapshotTotalLatency                tally.Timer
	snapshotPrepareLatency              tally.Timer
	snapshotMergeByBucketLatency        tally.Timer
	snapshotMergeAcrossBucketsLatency   tally.Timer
	snapshotChecksumLatency             tally.Timer
	snapshotPersistLatency              tally.Timer
	snapshotCloseLatency                tally.Timer

	purgeUnexpectedRefCount tally.Counter
}

func newDatabaseShardMetrics(shardID uint32, scope tally.Scope) dbShardMetrics {
	const insertErrorName = "insert-async.errors"
	snapshotScope := scope.SubScope("snapshot")
	return dbShardMetrics{
		create:       scope.Counter("create"),
		close:        scope.Counter("close"),
		closeStart:   scope.Counter("close-start"),
		closeLatency: scope.Timer("close-latency"),
		seriesTicked: scope.Tagged(map[string]string{
			"shard": fmt.Sprintf("%d", shardID),
		}).Gauge("series-ticked"),
		insertAsyncInsertErrors: scope.Tagged(map[string]string{
			"error_type":    "insert-series",
			"suberror_type": "shard-entry-insert-error",
		}).Counter(insertErrorName),
		insertAsyncWriteInternalErrors: scope.Tagged(map[string]string{
			"error_type":    "write-value",
			"suberror_type": "internal-error",
		}).Counter(insertErrorName),
		insertAsyncWriteInvalidParamsErrors: scope.Tagged(map[string]string{
			"error_type":    "write-value",
			"suberror_type": "invalid-params-error",
		}).Counter(insertErrorName),
		insertAsyncIndexErrors: scope.Tagged(map[string]string{
			"error_type":    "reverse-index",
			"suberror_type": "write-batch-error",
		}).Counter(insertErrorName),
		snapshotTotalLatency:              snapshotScope.Timer("total-latency"),
		snapshotPrepareLatency:            snapshotScope.Timer("prepare-latency"),
		snapshotMergeByBucketLatency:      snapshotScope.Timer("merge-by-bucket-latency"),
		snapshotMergeAcrossBucketsLatency: snapshotScope.Timer("merge-across-buckets-latency"),
		snapshotChecksumLatency:           snapshotScope.Timer("checksum-latency"),
		snapshotPersistLatency:            snapshotScope.Timer("persist-latency"),
		snapshotCloseLatency:              snapshotScope.Timer("close-latency"),
		purgeUnexpectedRefCount:           scope.Counter("purge-unexpected-ref-count"),
	}
}

type dbShardEntryWorkFn func(entry *Entry) bool

type dbShardEntryBatchWorkFn func(entries []*Entry) bool

type shardListElement *list.Element

type shardFlushState struct {
	sync.RWMutex
	statesByTime map[xtime.UnixNano]fileOpState
	initialized  bool
}

func newShardFlushState() shardFlushState {
	return shardFlushState{
		statesByTime: make(map[xtime.UnixNano]fileOpState),
	}
}

func newDatabaseShard(
	namespaceMetadata namespace.Metadata,
	shard uint32,
	blockRetriever block.DatabaseBlockRetriever,
	namespaceReaderMgr databaseNamespaceReaderManager,
	increasingIndex increasingIndex,
	reverseIndex NamespaceIndex,
	needsBootstrap bool,
	opts Options,
	seriesOpts series.Options,
) databaseShard {
	scope := opts.InstrumentOptions().MetricsScope().
		SubScope("dbshard")

	s := &dbShard{
		opts:                 opts,
		seriesOpts:           seriesOpts,
		nowFn:                opts.ClockOptions().NowFn(),
		state:                dbShardStateOpen,
		namespace:            namespaceMetadata,
		shard:                shard,
		namespaceReaderMgr:   namespaceReaderMgr,
		increasingIndex:      increasingIndex,
		seriesPool:           opts.DatabaseSeriesPool(),
		reverseIndex:         reverseIndex,
		lookup:               newShardMap(shardMapOptions{}),
		list:                 list.New(),
		newMergerFn:          fs.NewMerger,
		newFSMergeWithMemFn:  newFSMergeWithMem,
		filesetsFn:           fs.DataFiles,
		filesetPathsBeforeFn: fs.DataFileSetsBefore,
		deleteFilesFn:        fs.DeleteFiles,
		snapshotFilesFn:      fs.SnapshotFiles,
		sleepFn:              time.Sleep,
		newReaderFn:          fs.NewReader,
		identifierPool:       opts.IdentifierPool(),
		contextPool:          opts.ContextPool(),
		flushState:           newShardFlushState(),
		tickWg:               &sync.WaitGroup{},
		coldWritesEnabled:    namespaceMetadata.Options().ColdWritesEnabled(),
		indexEnabled:         namespaceMetadata.Options().IndexOptions().Enabled(),
		logger:               opts.InstrumentOptions().Logger(),
		metrics:              newDatabaseShardMetrics(shard, scope),
		tileAggregator:       opts.TileAggregator(),
		entryMetrics:         NewEntryMetrics(scope.SubScope("entries")),
	}
	s.insertQueue = newDatabaseShardInsertQueue(s.insertSeriesBatch,
		s.nowFn, opts.CoreFn(), scope, opts.InstrumentOptions().Logger())

	registerRuntimeOptionsListener := func(listener runtime.OptionsListener) {
		elem := opts.RuntimeOptionsManager().RegisterListener(listener)
		s.runtimeOptsListenClosers = append(s.runtimeOptsListenClosers, elem)
	}
	registerRuntimeOptionsListener(s)
	registerRuntimeOptionsListener(s.insertQueue)

	// Start the insert queue after registering runtime options listeners
	// that may immediately fire with values
	s.insertQueue.Start()

	if !needsBootstrap {
		s.bootstrapState = Bootstrapped
	}

	if blockRetriever != nil {
		s.setBlockRetriever(blockRetriever)
	}

	s.metrics.create.Inc(1)

	return s
}

func (s *dbShard) setBlockRetriever(retriever block.DatabaseBlockRetriever) {
	// If using the block retriever then set the block retriever field
	// and set the series block retriever as the shard itself and
	// the on retrieve block callback as the shard itself as well
	s.DatabaseBlockRetriever = retriever
	s.seriesBlockRetriever = s
	s.seriesOnRetrieveBlock = s
}

func (s *dbShard) SetRuntimeOptions(value runtime.Options) {
	s.Lock()
	s.currRuntimeOptions = dbShardRuntimeOptions{
		writeNewSeriesAsync:      value.WriteNewSeriesAsync(),
		tickSleepSeriesBatchSize: value.TickSeriesBatchSize(),
		tickSleepPerSeries:       value.TickPerSeriesSleepDuration(),
	}
	s.Unlock()
}

func (s *dbShard) ID() uint32 {
	return s.shard
}

func (s *dbShard) NumSeries() int64 {
	s.RLock()
	n := s.list.Len()
	s.RUnlock()
	return int64(n)
}

// Stream implements series.QueryableBlockRetriever
func (s *dbShard) Stream(
	ctx context.Context,
	id ident.ID,
	blockStart xtime.UnixNano,
	onRetrieve block.OnRetrieveBlock,
	nsCtx namespace.Context,
) (xio.BlockReader, error) {
	return s.DatabaseBlockRetriever.Stream(ctx, s.shard, id,
		blockStart, onRetrieve, nsCtx)
}

// IsBlockRetrievable implements series.QueryableBlockRetriever
func (s *dbShard) IsBlockRetrievable(blockStart xtime.UnixNano) (bool, error) {
	return s.hasWarmFlushed(blockStart)
}

func (s *dbShard) hasWarmFlushed(blockStart xtime.UnixNano) (bool, error) {
	flushState, err := s.FlushState(blockStart)
	if err != nil {
		return false, err
	}
	return s.warmStatusIsRetrievable(flushState.WarmStatus), nil
}

func (s *dbShard) warmStatusIsRetrievable(status warmStatus) bool {
	if !statusIsRetrievable(status.DataFlushed) {
		return false
	}

	// If the index is disabled, then we only are tracking data flushing.
	// Otherwise, warm status requires both data and index flushed.
	if !s.indexEnabled {
		return true
	}

	return statusIsRetrievable(status.IndexFlushed)
}

func statusIsRetrievable(status fileOpStatus) bool {
	switch status {
	case fileOpNotStarted, fileOpInProgress, fileOpFailed:
		return false
	case fileOpSuccess:
		return true
	}
	panic(fmt.Errorf("shard queried is retrievable with bad flush state %d",
		status))
}

// RetrievableBlockColdVersion implements series.QueryableBlockRetriever
func (s *dbShard) RetrievableBlockColdVersion(blockStart xtime.UnixNano) (int, error) {
	flushState, err := s.FlushState(blockStart)
	if err != nil {
		return -1, err
	}
	return flushState.ColdVersionFlushed, nil
}

// BlockStatesSnapshot implements series.QueryableBlockRetriever
func (s *dbShard) BlockStatesSnapshot() series.ShardBlockStateSnapshot {
	s.RLock()
	snapshots := s.blockStatesSnapshotWithRLock()
	s.RUnlock()

	return snapshots
}

func (s *dbShard) blockStatesSnapshotWithRLock() series.ShardBlockStateSnapshot {
	bootstrapped := s.bootstrapState == Bootstrapped
	if !bootstrapped {
		// Needs to be bootstrapped.
		return series.NewShardBlockStateSnapshot(false, series.BootstrappedBlockStateSnapshot{})
	}

	s.flushState.RLock()
	defer s.flushState.RUnlock()
	if !s.flushState.initialized {
		// Also needs to have the shard flush states initialized.
		return series.NewShardBlockStateSnapshot(false, series.BootstrappedBlockStateSnapshot{})
	}

	snapshot := make(map[xtime.UnixNano]series.BlockState, len(s.flushState.statesByTime))
	for time, state := range s.flushState.statesByTime {
		snapshot[time] = series.BlockState{
			WarmRetrievable: s.warmStatusIsRetrievable(state.WarmStatus),
			// Use ColdVersionRetrievable instead of ColdVersionFlushed since the snapshot
			// will be used to make eviction decisions and we don't want to evict data before
			// it is retrievable.
			ColdVersion: state.ColdVersionRetrievable,
		}
	}

	return series.NewShardBlockStateSnapshot(true, series.BootstrappedBlockStateSnapshot{
		Snapshot: snapshot,
	})
}

func (s *dbShard) OnRetrieveBlock(
	id ident.ID,
	tags ident.TagIterator,
	startTime xtime.UnixNano,
	segment ts.Segment,
	nsCtx namespace.Context,
) {
	s.RLock()
	entry, err := s.lookupEntryWithLock(id)
	if entry != nil {
		entry.IncrementReaderWriterCount()
		defer entry.DecrementReaderWriterCount()
	}
	s.RUnlock()

	if err != nil && err != errShardEntryNotFound {
		return // Likely closing
	}

	if entry != nil {
		entry.Series.OnRetrieveBlock(id, tags, startTime, segment, nsCtx)
		return
	}

	entry, err = s.newShardEntry(id, convert.NewTagsIterMetadataResolver(tags))
	if err != nil {
		// should never happen
		instrument.EmitAndLogInvariantViolation(s.opts.InstrumentOptions(),
			func(logger *zap.Logger) {
				logger.Error("unable to create shardEntry from retrieved block data",
					zap.Stringer("id", id),
					zap.Time("startTime", startTime.ToTime()),
					zap.Error(err))
			})
		return
	}

	// NB(r): Do not need to specify that needs to be indexed as series would
	// have been already been indexed when it was written
	copiedID := entry.Series.ID()
	copiedTagsIter := s.identifierPool.TagsIterator()
	copiedTagsIter.ResetFields(entry.Series.Metadata().Fields)
	s.insertQueue.Insert(dbShardInsert{
		entry: entry,
		opts: dbShardInsertAsyncOptions{
			// NB(r): Caching blocks should not be considered for
			// new series insert rate limit.
			skipRateLimit:            true,
			hasPendingRetrievedBlock: true,
			pendingRetrievedBlock: dbShardPendingRetrievedBlock{
				id:      copiedID,
				tags:    copiedTagsIter,
				start:   startTime,
				segment: segment,
				nsCtx:   nsCtx,
			},
		},
	})
}

func (s *dbShard) OnEvictedFromWiredList(id ident.ID, blockStart xtime.UnixNano) {
	s.RLock()
	entry, err := s.lookupEntryWithLock(id)
	s.RUnlock()

	if err != nil && err != errShardEntryNotFound {
		return // Shard is probably closing
	}

	if entry == nil {
		// Its counter-intuitive that this can ever occur because the series should
		// always exist if it has any active blocks, and if we've reached this point
		// then the WiredList had a reference to a block that should still be in the
		// series, and thus the series should exist. The reason this can occur is that
		// even though the WiredList controls the lifecycle of blocks retrieved from
		// disk, those blocks can still be removed from the series if they've completely
		// fallen out of the retention period. In that case, the series tick will still
		// remove the block, and then the shard tick can remove the series. At that point,
		// it's possible for the WiredList to have a reference to an expired block for a
		// series that is no longer in the shard.
		return
	}

	entry.Series.OnEvictedFromWiredList(id, blockStart)
}

func (s *dbShard) forEachShardEntry(entryFn dbShardEntryWorkFn) {
	s.forEachShardEntryBatch(func(currEntries []*Entry) bool {
		for _, entry := range currEntries {
			if continueForEach := entryFn(entry); !continueForEach {
				return false
			}
		}
		return true
	})
}

func iterateBatchSize(elemsLen int) int {
	if elemsLen < shardIterateBatchMinSize {
		return shardIterateBatchMinSize
	}
	t := math.Ceil(shardIterateBatchPercent * float64(elemsLen))
	return int(math.Max(shardIterateBatchMinSize, t))
}

func (s *dbShard) forEachShardEntryBatch(entriesBatchFn dbShardEntryBatchWorkFn) {
	// NB(r): consider using a lockless list for ticking.
	s.RLock()
	elemsLen := s.list.Len()
	s.RUnlock()

	batchSize := iterateBatchSize(elemsLen)
	decRefElem := func(e *list.Element) {
		if e == nil {
			return
		}
		e.Value.(*Entry).DecrementReaderWriterCount()
	}

	var (
		currEntries = make([]*Entry, 0, batchSize)
		first       = true
		nextElem    *list.Element
	)
	for nextElem != nil || first {
		s.RLock()
		// NB(prateek): release held reference on the next element pointer now
		// that we have the read lock and are guaranteed it cannot be changed
		// from under us.
		decRefElem(nextElem)

		// lazily pull from the head of the list at first
		if first {
			nextElem = s.list.Front()
			first = false
		}

		elem := nextElem
		for ticked := 0; ticked < batchSize && elem != nil; ticked++ {
			nextElem = elem.Next()
			entry := elem.Value.(*Entry)
			entry.IncrementReaderWriterCount()
			currEntries = append(currEntries, entry)
			elem = nextElem
		}

		// NB(prateek): inc a reference to the next element while we have a lock,
		// to guarantee the element pointer cannot be changed from under us.
		if nextElem != nil {
			nextElem.Value.(*Entry).IncrementReaderWriterCount()
		}
		s.RUnlock()

		continueExecution := entriesBatchFn(currEntries)
		for i := range currEntries {
			currEntries[i].DecrementReaderWriterCount()
			currEntries[i] = nil
		}
		currEntries = currEntries[:0]
		if !continueExecution {
			decRefElem(nextElem)
			return
		}
	}
}

func (s *dbShard) IsBootstrapped() bool {
	return s.BootstrapState() == Bootstrapped
}

func (s *dbShard) Close() error {
	s.Lock()
	if s.state != dbShardStateOpen {
		s.Unlock()
		return errShardNotOpen
	}
	s.state = dbShardStateClosing
	s.Unlock()

	s.insertQueue.Stop()

	for _, closer := range s.runtimeOptsListenClosers {
		closer.Close()
	}

	s.metrics.closeStart.Inc(1)
	stopwatch := s.metrics.closeLatency.Start()
	defer func() {
		s.metrics.close.Inc(1)
		stopwatch.Stop()
	}()

	// NB(prateek): wait till any existing ticks are finished. In the usual
	// case, no other ticks are running, and tickWg count is at 0, so the
	// call to Wait() will return immediately.
	// In the case when there is an existing Tick running, the count for
	// tickWg will be > 0, and we'll wait until it's reset to zero, which
	// will happen because earlier in this function we set the shard state
	// to dbShardStateClosing, which triggers an early termination of
	// any active ticks.
	s.tickWg.Wait()

	// NB(r): Asynchronously we purge expired series to ensure pressure on the
	// GC is not placed all at one time.  If the deadline is too low and still
	// causes the GC to impact performance when closing shards the deadline
	// should be increased.
	cancellable := context.NewNoOpCanncellable()
	_, err := s.tickAndExpire(cancellable, tickPolicyCloseShard, namespace.Context{}, TickOptions{TopMetricsToTrack: 0})
	return err
}

func (s *dbShard) Closed() bool {
	return s.isClosing()
}

func (s *dbShard) isClosing() bool {
	s.RLock()
	closing := s.isClosingWithLock()
	s.RUnlock()
	return closing
}

func (s *dbShard) isClosingWithLock() bool {
	return s.state == dbShardStateClosing
}

func (s *dbShard) Tick(
	c context.Cancellable,
	startTime xtime.UnixNano,
	nsCtx namespace.Context,
	tickOptions TickOptions,
) (tickResult, error) {
	s.removeAnyFlushStatesTooEarly(startTime)
	return s.tickAndExpire(c, tickPolicyRegular, nsCtx, tickOptions)
}

func (s *dbShard) tickAndExpire(
	c context.Cancellable,
	policy tickPolicy,
	nsCtx namespace.Context,
	tickOptions TickOptions,
) (tickResult, error) {
	s.Lock()
	// ensure only one tick can execute at a time
	if s.ticking {
		s.Unlock()
		// i.e. we were previously ticking
		return tickResult{}, errShardAlreadyTicking
	}

	// NB(prateek): we bail out early if the shard is closing,
	// unless it's the final tick issued during the Close(). This
	// final tick is required to release resources back to our pools.
	if policy != tickPolicyCloseShard && s.isClosingWithLock() {
		s.Unlock()
		return tickResult{}, errShardClosingTickTerminated
	}

	// enable Close() to track the lifecycle of the tick
	s.ticking = true
	s.tickWg.Add(1)
	s.Unlock()

	// reset ticking state
	defer func() {
		s.Lock()
		s.ticking = false
		s.tickWg.Done()
		s.Unlock()
		s.metrics.seriesTicked.Update(0.0) // reset external visibility
	}()

	var (
		r                             = &tickResult{}
		terminatedTickingDueToClosing bool
		i                             int
		slept                         time.Duration
		expired                       []*Entry
	)
	s.RLock()
	tickSleepBatch := s.currRuntimeOptions.tickSleepSeriesBatchSize
	tickSleepPerSeries := s.currRuntimeOptions.tickSleepPerSeries
	// Use blockStatesSnapshotWithRLock here to prevent nested read locks.
	// Nested read locks will cause deadlocks if there is write lock attempt in
	// between the nested read locks, since the write lock attempt will block
	// future read lock attempts.
	blockStates := s.blockStatesSnapshotWithRLock()
	s.RUnlock()
	topMetricsToTrack := tickOptions.TopMetricsToTrack
	maxMapLenForTracking := tickOptions.MaxMapLenForTracking
	shouldTrackTopMetrics := topMetricsToTrack > 0 && maxMapLenForTracking > 0

	if shouldTrackTopMetrics {
		// Make 'r' ready to track top metrics.
		s.logger.Debug("shard is ticking with top metrics tracking enabled", zap.Int("shard", int(s.ID())))
		r.trackTopMetrics()
	}

	// NB: no lock is held when the func is called. See the implementation of forEachShardEntryBatch().
	s.forEachShardEntryBatch(func(currEntries []*Entry) bool {
		// re-using `expired` to amortize allocs, still need to reset it
		// to be safe for re-use.
		for i := range expired {
			expired[i] = nil
		}
		expired = expired[:0]
		for _, entry := range currEntries {
			if i > 0 && i%tickSleepBatch == 0 {
				// NB(xichen): if the tick is cancelled, we bail out immediately.
				// The cancellation check is performed on every batch of entries
				// instead of every entry to reduce load.
				if c.IsCancelled() {
					return false
				}
				// NB(prateek): Also bail out early if the shard is closing,
				// unless it's the final tick issued during the Close(). This
				// final tick is required to release resources back to our pools.
				if policy != tickPolicyCloseShard && s.isClosing() {
					terminatedTickingDueToClosing = true
					return false
				}
				// Expose shard level Tick() progress externally.
				s.metrics.seriesTicked.Update(float64(i))
				// Throttle the tick
				sleepFor := time.Duration(tickSleepBatch) * tickSleepPerSeries
				s.sleepFn(sleepFor)
				slept += sleepFor
			}

			var (
				result series.TickResult
				err    error
			)
			switch policy {
			case tickPolicyRegular:
				result, err = entry.Series.Tick(blockStates, nsCtx)
			case tickPolicyCloseShard:
				err = series.ErrSeriesAllDatapointsExpired
			}
			if err == series.ErrSeriesAllDatapointsExpired {
				expired = append(expired, entry)
				r.expiredSeries++
			} else {
				r.activeSeries++
				if err != nil {
					r.errors++
				}
				if shouldTrackTopMetrics {
					// TODO: find a cheaper way to get the metric name. 'Get()' iterates on all labels.
					if metricNameBytes, ok := entry.Series.Metadata().Get([]byte(metricLabelName)); ok {
						nameHash, metric := newMetricCardinality(metricNameBytes, 1)
						// We don't handle hash collisions here, since we are not looking for precise top metrics/candinality.
						if currentMetric, ok := r.metricToCardinality[nameHash]; ok {
							currentMetric.cardinality++
						} else if len(r.metricToCardinality) < maxMapLenForTracking {
							r.metricToCardinality[nameHash] = metric
						}
					}
				}
			}
			r.activeBlocks += result.ActiveBlocks
			r.wiredBlocks += result.WiredBlocks
			r.unwiredBlocks += result.UnwiredBlocks
			r.pendingMergeBlocks += result.PendingMergeBlocks
			r.madeExpiredBlocks += result.MadeExpiredBlocks
			r.madeUnwiredBlocks += result.MadeUnwiredBlocks
			r.mergedOutOfOrderBlocks += result.MergedOutOfOrderBlocks
			r.evictedBuckets += result.EvictedBuckets
			r.truncateTopMetrics(topMetricsToTrack)
			i++
		}

		// Purge any series requiring purging.
		if len(expired) > 0 {
			r.purgedSeries += s.purgeExpiredSeries(expired)
			for i := range expired {
				expired[i] = nil
			}
			expired = expired[:0]
		}
		// Continue.
		return true
	})

	if terminatedTickingDueToClosing {
		s.logger.Debug("Returning empty tick result due to closing")
		return tickResult{}, errShardClosingTickTerminated
	}

	return *r, nil
}

// NB(prateek): purgeExpiredSeries requires that all entries passed to it have at least one reader/writer,
// i.e. have a readWriteCount of at least 1.
// Currently, this function is only called by the lambda inside `tickAndExpire`'s `forEachShardEntryBatch`
// call. This satisfies the contract of all entries it operating upon being guaranteed to have a
// readerWriterEntryCount of at least 1, by virtue of the implementation of `forEachShardEntryBatch`.
func (s *dbShard) purgeExpiredSeries(expiredEntries []*Entry) int {
	var closedSeries int = 0
	// Remove all expired series from lookup and list.
	s.Lock()
	for _, entry := range expiredEntries {
		// Only purge series after they've been GCed from the index, so that these happen and in order
		// and there is no raciness around GCing something from the index when the series has already
		// been removed from memory.
		if s.indexEnabled && !entry.IndexGarbageCollected.Load() {
			continue
		}

		series := entry.Series
		id := series.ID()
		elem, exists := s.lookup.Get(id)
		if !exists {
			continue
		}

		count := entry.ReaderWriterCount()
		// The contract requires all entries to have count >= 1.
		if count < 1 {
			s.metrics.purgeUnexpectedRefCount.Inc(1)
			instrument.EmitAndLogInvariantViolation(s.opts.InstrumentOptions(), func(l *zap.Logger) {
				l.Error("purgeExpiredSeries encountered invalid series read/write count",
					zap.Stringer("namespace", s.namespace.ID()),
					zap.Uint32("shard", s.ID()),
					zap.Stringer("series", series.ID()),
					zap.Int32("readerWriterCount", count))
			})
			continue
		}
		// If this series is currently being written to or read from, we don't
		// remove to ensure a consistent view of the series to other users.
		if count > 1 {
			continue
		}
		// If there have been datapoints written to the series since its
		// last empty check, we don't remove it.
		if !series.IsEmpty() {
			continue
		}

		// NB(xichen): if we get here, we are guaranteed that there can be
		// no more reads/writes to this series while the lock is held, so it's
		// safe to remove it.
		series.Close()
		s.list.Remove(elem)
		s.lookup.Delete(id)
		closedSeries++
	}
	s.Unlock()
	s.logger.Debug("purgeExpiredSeries purged series", zap.Int("purgedSeries", closedSeries))
	return closedSeries
}

func (s *dbShard) WriteTagged(
	ctx context.Context,
	id ident.ID,
	tagResolver convert.TagMetadataResolver,
	timestamp xtime.UnixNano,
	value float64,
	unit xtime.Unit,
	annotation []byte,
	wOpts series.WriteOptions,
) (SeriesWrite, error) {
	return s.writeAndIndex(ctx, id, tagResolver, timestamp,
		value, unit, annotation, wOpts, true)
}

func (s *dbShard) Write(
	ctx context.Context,
	id ident.ID,
	timestamp xtime.UnixNano,
	value float64,
	unit xtime.Unit,
	annotation []byte,
	wOpts series.WriteOptions,
) (SeriesWrite, error) {
	return s.writeAndIndex(ctx, id, convert.EmptyTagMetadataResolver, timestamp,
		value, unit, annotation, wOpts, false)
}

func (s *dbShard) writeAndIndex(
	ctx context.Context,
	id ident.ID,
	tagResolver convert.TagMetadataResolver,
	timestamp xtime.UnixNano,
	value float64,
	unit xtime.Unit,
	annotation []byte,
	wOpts series.WriteOptions,
	shouldReverseIndex bool,
) (SeriesWrite, error) {
	// Prepare write
	entry, opts, err := s.TryRetrieveSeriesAndIncrementReaderWriterCount(id)
	if err != nil {
		return SeriesWrite{}, err
	}

	writable := entry != nil

	// If no entry and we are not writing new series asynchronously.
	if !writable && !opts.WriteNewSeriesAsync {
		// Avoid double lookup by enqueueing insert immediately.
		result, err := s.insertSeriesAsyncBatched(id, tagResolver, dbShardInsertAsyncOptions{
			hasPendingIndexing: shouldReverseIndex,
			pendingIndex: dbShardPendingIndex{
				timestamp:  timestamp,
				enqueuedAt: s.nowFn(),
			},
		})
		if err != nil {
			return SeriesWrite{}, err
		}

		// Wait for the insert to be batched together and inserted
		result.wg.Wait()

		// Retrieve the inserted entry
		entry, err = s.writableSeries(id, tagResolver)
		if err != nil {
			return SeriesWrite{}, err
		}
		writable = true

		// NB(r): We just indexed this series if shouldReverseIndex was true
		shouldReverseIndex = false
	}

	var (
		commitLogSeriesID          ident.ID
		commitLogSeriesUniqueIndex uint64
		needsIndex                 bool
		pendingIndexInsert         writes.PendingIndexInsert
		// Err on the side of caution and always write to the commitlog if writing
		// async, since there is no information about whether the write succeeded
		// or not.
		wasWritten = true
	)
	if writable {
		// Perform write. No need to copy the annotation here because we're using it
		// synchronously and all downstream code will copy anthing they need to maintain
		// a reference to.
		wasWritten, _, err = entry.Series.Write(ctx, timestamp, value, unit, annotation, wOpts)
		// Load series metadata before decrementing the writer count
		// to ensure this metadata is snapshotted at a consistent state
		// NB(r): We explicitly do not place the series ID back into a
		// pool as high frequency users of series IDs such
		// as the commit log need to use the reference without the
		// overhead of ownership tracking. This makes taking a ref here safe.
		commitLogSeriesID = entry.Series.ID()
		commitLogSeriesUniqueIndex = entry.Index
		if err == nil && shouldReverseIndex {
			if entry.NeedsIndexUpdate(s.reverseIndex.BlockStartForWriteTime(timestamp)) {
				if !opts.WriteNewSeriesAsync {
					return SeriesWrite{}, fmt.Errorf("to index async need write new series to be enabled")
				}
				needsIndex = true
				pendingIndexInsert = s.pendingIndexInsert(entry, timestamp)
			}
		}
		// release the reference we got on entry from `writableSeries`
		entry.DecrementReaderWriterCount()
		if err != nil {
			return SeriesWrite{}, err
		}
	} else {
		// This is an asynchronous insert and write which means we need to clone the annotation
		// because its lifecycle in the commit log is independent of the calling function.
		var annotationClone checked.Bytes
		if len(annotation) != 0 {
			annotationClone = s.opts.BytesPool().Get(len(annotation))
			// IncRef here so we can write the bytes in, but don't DecRef because the queue is about
			// to take ownership and will DecRef when its done.
			annotationClone.IncRef()
			annotationClone.AppendAll(annotation)
		}

		result, err := s.insertSeriesAsyncBatched(id, tagResolver, dbShardInsertAsyncOptions{
			hasPendingWrite: true,
			pendingWrite: dbShardPendingWrite{
				timestamp:  timestamp,
				value:      value,
				unit:       unit,
				annotation: annotationClone,
				opts:       wOpts,
			},
		})
		if err != nil {
			return SeriesWrite{}, err
		}

		if shouldReverseIndex {
			if !opts.WriteNewSeriesAsync {
				return SeriesWrite{}, fmt.Errorf("to index async need write new series to be enabled")
			}
			needsIndex = true
			pendingIndexInsert = s.pendingIndexInsert(result.entry, timestamp)
		}

		// NB(r): Make sure to use the copied ID which will eventually
		// be set to the newly series inserted ID.
		// The `id` var here is volatile after the context is closed
		// and adding ownership tracking to use it in the commit log
		// (i.e. registering a dependency on the context) is too expensive.
		commitLogSeriesID = result.copiedID
		commitLogSeriesUniqueIndex = result.entry.Index
	}

	// Return metadata useful for writing to commit log and indexing.
	return SeriesWrite{
		Series: ts.Series{
			UniqueIndex: commitLogSeriesUniqueIndex,
			Namespace:   s.namespace.ID(),
			ID:          commitLogSeriesID,
			Shard:       s.shard,
		},
		WasWritten:         wasWritten,
		NeedsIndex:         needsIndex,
		PendingIndexInsert: pendingIndexInsert,
	}, nil
}

func (s *dbShard) SeriesRefResolver(
	id ident.ID,
	tags ident.TagIterator,
) (bootstrap.SeriesRefResolver, error) {
	// Try retrieve existing series.
	entry, err := s.retrieveWritableSeriesAndIncrementReaderWriterCount(id)
	if err != nil {
		return nil, err
	}

	if entry != nil {
		// The read/write ref is already incremented.
		return entry, nil
	}

	entry, err = s.newShardEntry(id, convert.NewTagsIterMetadataResolver(tags))
	if err != nil {
		return nil, err
	}

	// Increment ref count to avoid expiration of the new entry just after adding it to the queue.
	// It is possible that this entry does not end up as the one in the shard. Therefore, the resolver
	// for this specific entry is responsible for closing, and there should always be one resolver
	// responsible for the one that DOES end up in the shard.
	entry.IncrementReaderWriterCount()

	wg, err := s.insertQueue.Insert(dbShardInsert{
		entry: entry,
		opts: dbShardInsertAsyncOptions{
			// skipRateLimit for true since this method is used by bootstrapping
			// and should not be rate limited.
			skipRateLimit: true,
			// do not release entry ref during async write, because entry ref will be released when
			// ReleaseRef() is called on bootstrap.SeriesRefResolver.
			releaseEntryRef: false,
		},
	})
	if err != nil {
		return nil, err
	}

	// Series will wait for the result to be batched together and inserted.
	return NewSeriesResolver(
		wg,
		entry,
		s.retrieveWritableSeriesAndIncrementReaderWriterCount), nil
}

func (s *dbShard) ReadEncoded(
	ctx context.Context,
	id ident.ID,
	start, end xtime.UnixNano,
	nsCtx namespace.Context,
) (series.BlockReaderIter, error) {
	s.RLock()
	entry, err := s.lookupEntryWithLock(id)
	if entry != nil {
		// NB(r): Ensure readers have consistent view of this series, do
		// not expire the series while being read from.
		entry.IncrementReaderWriterCount()
		defer entry.DecrementReaderWriterCount()
	}
	s.RUnlock()

	if err == errShardEntryNotFound {
		switch s.opts.SeriesCachePolicy() {
		case series.CacheAll:
			// No-op, would be in memory if cached
			return nil, nil
		}
	} else if err != nil {
		return nil, err
	}

	if entry != nil {
		return entry.Series.ReadEncoded(ctx, start, end, nsCtx)
	}

	retriever := s.seriesBlockRetriever
	onRetrieve := s.seriesOnRetrieveBlock
	opts := s.seriesOpts
	reader := series.NewReaderUsingRetriever(id, retriever, onRetrieve, nil, opts)
	return reader.ReadEncoded(ctx, start, end, nsCtx)
}

// lookupEntryWithLock returns the entry for a given id while holding a read lock or a write lock.
func (s *dbShard) lookupEntryWithLock(id ident.ID) (*Entry, error) {
	if s.state != dbShardStateOpen {
		// NB(r): Return an invalid params error here so any upstream
		// callers will not retry this operation
		return nil, xerrors.NewInvalidParamsError(errShardNotOpen)
	}
	elem, exists := s.lookup.Get(id)
	if !exists {
		return nil, errShardEntryNotFound
	}
	return elem.Value.(*Entry), nil
}

func (s *dbShard) writableSeries(id ident.ID, tagResolver convert.TagMetadataResolver) (*Entry, error) {
	for {
		entry, err := s.retrieveWritableSeriesAndIncrementReaderWriterCount(id)
		if entry != nil {
			return entry, nil
		}
		if err != nil {
			return nil, err
		}

		// Not inserted, attempt a batched insert
		result, err := s.insertSeriesAsyncBatched(id, tagResolver, dbShardInsertAsyncOptions{})
		if err != nil {
			return nil, err
		}

		// Wait for the insert attempt
		result.wg.Wait()
	}
}

// WritableSeriesOptions defines writable series options.
type WritableSeriesOptions struct {
	// WriteNewSeriesAsync specifies if the series should be async written.
	WriteNewSeriesAsync bool
}

// TryRetrieveSeriesAndIncrementReaderWriterCount attempts to retrieve a writable series.
// This increments the reader/writer count and so should be decremented when the series
// is no longer held.
func (s *dbShard) TryRetrieveSeriesAndIncrementReaderWriterCount(id ident.ID) (
	*Entry,
	WritableSeriesOptions,
	error,
) {
	s.RLock()
	opts := WritableSeriesOptions{
		WriteNewSeriesAsync: s.currRuntimeOptions.writeNewSeriesAsync,
	}
	if entry, err := s.lookupEntryWithLock(id); err == nil {
		entry.IncrementReaderWriterCount()
		s.RUnlock()
		return entry, opts, nil
	} else if err != errShardEntryNotFound {
		s.RUnlock()
		return nil, opts, err
	}
	s.RUnlock()
	return nil, opts, nil
}

func (s *dbShard) retrieveWritableSeriesAndIncrementReaderWriterCount(id ident.ID) (*Entry, error) {
	entry, _, err := s.TryRetrieveSeriesAndIncrementReaderWriterCount(id)
	return entry, err
}

func (s *dbShard) newShardEntry(
	id ident.ID,
	tagResolver convert.TagMetadataResolver,
) (*Entry, error) {
	// NB(r): As documented in storage/series.DatabaseSeries the series IDs
	// and metadata are garbage collected, hence we cast the ID to a BytesID
	// that can't be finalized.
	// Since series are purged so infrequently the overhead of not releasing
	// back an ID and metadata to a pool is amortized over a long period of
	// time.
	// Also of note, when a series is indexed in multiple index segments it is
	// worth keeping the metadata around so it can be referenced to twice
	// without creating a new array of []doc.Field for all the tags twice.
	// Hence this stays on the storage/series.DatabaseSeries for when it needs
	// to be re-indexed.
	var (
		seriesMetadata doc.Metadata
		err            error
	)

	seriesMetadata, err = tagResolver.Resolve(id)
	if err != nil {
		return nil, err
	}

	// Use the same bytes as the series metadata for the ID.
	seriesID := ident.BytesID(seriesMetadata.ID)

	uniqueIndex := s.increasingIndex.nextIndex()
	newSeries := s.seriesPool.Get()
	newSeries.Reset(series.DatabaseSeriesOptions{
		ID:                     seriesID,
		Metadata:               seriesMetadata,
		UniqueIndex:            uniqueIndex,
		BlockRetriever:         s.seriesBlockRetriever,
		OnRetrieveBlock:        s.seriesOnRetrieveBlock,
		OnEvictedFromWiredList: s,
		Options:                s.seriesOpts,
	})
	return NewEntry(NewEntryOptions{
		Shard:        s,
		Series:       newSeries,
		Index:        uniqueIndex,
		IndexWriter:  s.reverseIndex,
		NowFn:        s.nowFn,
		EntryMetrics: s.entryMetrics,
	}), nil
}

type insertAsyncResult struct {
	wg       *sync.WaitGroup
	copiedID ident.ID
	// entry is not guaranteed to be the final entry
	// inserted into the shard map in case there is already
	// an existing entry waiting in the insert queue
	entry *Entry
}

func (s *dbShard) pendingIndexInsert(
	entry *Entry,
	timestamp xtime.UnixNano,
) writes.PendingIndexInsert {
	// inc a ref on the entry to ensure it's valid until the queue acts upon it.
	entry.OnIndexPrepare(s.reverseIndex.BlockStartForWriteTime(timestamp))
	return writes.PendingIndexInsert{
		Entry: index.WriteBatchEntry{
			Timestamp:     timestamp,
			OnIndexSeries: entry,
			EnqueuedAt:    s.nowFn(),
		},
		Document: entry.Series.Metadata(),
	}
}

func (s *dbShard) insertSeriesForIndexingAsyncBatched(
	entry *Entry,
	timestamp xtime.UnixNano,
	async bool,
) error {
	indexBlockStart := s.reverseIndex.BlockStartForWriteTime(timestamp)
	// inc a ref on the entry to ensure it's valid until the queue acts upon it.
	entry.OnIndexPrepare(indexBlockStart)
	wg, err := s.insertQueue.Insert(dbShardInsert{
		entry: entry,
		opts: dbShardInsertAsyncOptions{
			// NB(r): Just indexing, should not be considered for new
			// series insert rate limiting.
			skipRateLimit:      true,
			hasPendingIndexing: true,
			pendingIndex: dbShardPendingIndex{
				timestamp:  timestamp,
				enqueuedAt: s.nowFn(),
			},
			// indicate we already have inc'd the entry's ref count, so we can correctly
			// handle the ref counting semantics in `insertSeriesBatch`.
			releaseEntryRef: true,
		},
	})
	// i.e. unable to enqueue into shard insert queue
	if err != nil {
		entry.OnIndexFinalize(indexBlockStart) // release any reference's we've held for indexing
		return err
	}

	// if operating in async mode, we're done
	if async {
		return nil
	}

	// if indexing in sync mode, wait till we're done and ensure we have have indexed the entry
	wg.Wait()
	if !entry.IndexedForBlockStart(indexBlockStart) {
		// i.e. indexing failed
		return fmt.Errorf("internal error: unable to index series")
	}

	return nil
}

func (s *dbShard) insertSeriesAsyncBatched(
	id ident.ID,
	tagResolver convert.TagMetadataResolver,
	opts dbShardInsertAsyncOptions,
) (insertAsyncResult, error) {
	entry, err := s.newShardEntry(id, tagResolver)
	if err != nil {
		return insertAsyncResult{}, err
	}

	wg, err := s.insertQueue.Insert(dbShardInsert{
		entry: entry,
		opts:  opts,
	})
	return insertAsyncResult{
		wg: wg,
		// Make sure to return the copied ID from the new series.
		copiedID: entry.Series.ID(),
		entry:    entry,
	}, err
}

type insertSyncType uint8

// nolint: varcheck, unused
const (
	insertSync insertSyncType = iota
	insertSyncIncReaderWriterCount
)

type insertSyncOptions struct {
	insertType      insertSyncType
	hasPendingIndex bool
	pendingIndex    dbShardPendingIndex
}

func (s *dbShard) insertSeriesSync(
	id ident.ID,
	tagResolver convert.TagMetadataResolver,
	opts insertSyncOptions,
) (*Entry, error) {
	// NB(r): Create new shard entry outside of write lock to reduce
	// time using write lock.
	newEntry, err := s.newShardEntry(id, tagResolver)
	if err != nil {
		// should never happen
		instrument.EmitAndLogInvariantViolation(s.opts.InstrumentOptions(),
			func(logger *zap.Logger) {
				logger.Error("insertSeriesSync error creating shard entry",
					zap.String("id", id.String()),
					zap.Error(err))
			})
		return nil, err
	}

	s.Lock()
	unlocked := false
	defer func() {
		if !unlocked {
			s.Unlock()
		}
	}()

	existingEntry, err := s.lookupEntryWithLock(id)
	if err != nil && err != errShardEntryNotFound {
		// Shard not taking inserts likely.
		return nil, err
	}
	if existingEntry != nil {
		// Already inserted, likely a race.
		return existingEntry, nil
	}

	s.insertNewShardEntryWithLock(newEntry)

	// Track unlocking.
	unlocked = true
	s.Unlock()

	// Be sure to enqueue for indexing if requires a pending index.
	if opts.hasPendingIndex {
		if _, err := s.insertQueue.Insert(dbShardInsert{
			entry: newEntry,
			opts: dbShardInsertAsyncOptions{
				// NB(r): Just indexing, should not be considered for new
				// series insert rate limiting.
				skipRateLimit:      true,
				hasPendingIndexing: opts.hasPendingIndex,
				pendingIndex:       opts.pendingIndex,
			},
		}); err != nil {
			return nil, err
		}
	}

	// Check if we're making a modification to this entry, be sure
	// to increment the writer count so it's visible when we release
	// the lock.
	if opts.insertType == insertSyncIncReaderWriterCount {
		newEntry.IncrementReaderWriterCount()
	}

	return newEntry, nil
}

func (s *dbShard) insertNewShardEntryWithLock(entry *Entry) {
	// Set the lookup value, we use the copied ID and since it is GC'd
	// we explicitly set it with options to not copy the key and not to
	// finalize it.
	copiedID := entry.Series.ID()
	listElem := s.list.PushBack(entry)
	s.lookup.SetUnsafe(copiedID, listElem, shardMapSetUnsafeOptions{
		NoCopyKey:     true,
		NoFinalizeKey: true,
	})
	entry.SetInsertTime(s.nowFn())
}

func (s *dbShard) insertSeriesBatch(inserts []dbShardInsert) error {
	var (
		anyPendingAction   = false
		numPendingIndexing = 0
	)

	s.Lock()
	for i := range inserts {
		// If we are going to write to this entry then increment the
		// writer count so it does not look empty immediately after
		// we release the write lock.
		hasPendingWrite := inserts[i].opts.hasPendingWrite
		hasPendingIndexing := inserts[i].opts.hasPendingIndexing
		hasPendingRetrievedBlock := inserts[i].opts.hasPendingRetrievedBlock
		anyPendingAction = anyPendingAction || hasPendingWrite ||
			hasPendingRetrievedBlock || hasPendingIndexing

		if hasPendingIndexing {
			numPendingIndexing++
		}

		// we don't need to inc the entry ref count if we already have a ref on the entry. check if
		// that's the case.
		if inserts[i].opts.releaseEntryRef {
			// don't need to inc a ref on the entry, we were given as writable entry as input.
			continue
		}

		// i.e. we don't have a ref on provided entry, so we check if between the operation being
		// enqueue in the shard insert queue, and this function executing, an entry was created
		// for the same ID.
		entry, err := s.lookupEntryWithLock(inserts[i].entry.Series.ID())
		if entry != nil {
			// Already exists so update the entry we're pointed at for this insert.
			inserts[i].entry = entry
		}

		if hasPendingIndexing || hasPendingWrite || hasPendingRetrievedBlock {
			// We're definitely writing a value, ensure that the pending write is
			// visible before we release the lookup write lock.
			inserts[i].entry.IncrementReaderWriterCount()
			// also indicate that we have a ref count on this entry for this operation.
			inserts[i].opts.releaseEntryRef = true
		}

		if err == nil {
			// Already inserted.
			continue
		}

		if err != errShardEntryNotFound {
			// Shard is not taking inserts.
			s.Unlock()
			// FOLLOWUP(prateek): is this an existing bug? why don't we need to release any ref's we've inc'd
			// on entries in the loop before this point, i.e. in range [0, i). Otherwise, how are those entries
			// going to get cleaned up?
			s.metrics.insertAsyncInsertErrors.Inc(int64(len(inserts) - i))
			return err
		}

		// Insert still pending, perform the insert
		entry = inserts[i].entry
		s.insertNewShardEntryWithLock(entry)
	}
	s.Unlock()

	if !anyPendingAction {
		return nil
	}

	// Perform any indexing, pending writes or pending retrieved blocks outside of lock
	ctx := s.contextPool.Get()
	// TODO(prateek): pool this type
	indexBlockSize := s.namespace.Options().IndexOptions().BlockSize()
	indexBatch := index.NewWriteBatch(index.WriteBatchOptions{
		InitialCapacity: numPendingIndexing,
		IndexBlockSize:  indexBlockSize,
	})
	for i := range inserts {
		var (
			entry           = inserts[i].entry
			releaseEntryRef = inserts[i].opts.releaseEntryRef
			err             error
		)

		if inserts[i].opts.hasPendingWrite {
			write := inserts[i].opts.pendingWrite
			var annotationBytes []byte
			if write.annotation != nil {
				annotationBytes = write.annotation.Bytes()
			}
			// NB: Ignore the `wasWritten` return argument here since this is an async
			// operation and there is nothing further to do with this value.
			// TODO: Consider propagating the `wasWritten` argument back to the caller
			// using waitgroup (or otherwise) in the future.
			_, _, err = entry.Series.Write(ctx, write.timestamp, write.value,
				write.unit, annotationBytes, write.opts)
			if err != nil {
				if xerrors.IsInvalidParams(err) {
					s.metrics.insertAsyncWriteInvalidParamsErrors.Inc(1)
				} else {
					s.metrics.insertAsyncWriteInternalErrors.Inc(1)
					s.logger.Error("error with async insert write", zap.Error(err))
				}
			}

			if write.annotation != nil {
				// Now that we've performed the write, we can finalize the annotation because
				// we're done with it and all the code from the series downwards has copied any
				// data that it required.
				write.annotation.DecRef()
				write.annotation.Finalize()
			}
		}

		if inserts[i].opts.hasPendingIndexing {
			pendingIndex := inserts[i].opts.pendingIndex
			// increment the ref on the entry, as the original one was transferred to the
			// this method (insertSeriesBatch) via `releaseEntryRef` mechanism.
			entry.OnIndexPrepare(s.reverseIndex.BlockStartForWriteTime(pendingIndex.timestamp))

			writeBatchEntry := index.WriteBatchEntry{
				Timestamp:     pendingIndex.timestamp,
				OnIndexSeries: entry,
				EnqueuedAt:    pendingIndex.enqueuedAt,
			}

			indexBatch.Append(writeBatchEntry, entry.Series.Metadata())
		}

		if inserts[i].opts.hasPendingRetrievedBlock {
			block := inserts[i].opts.pendingRetrievedBlock
			entry.Series.OnRetrieveBlock(block.id, block.tags, block.start, block.segment, block.nsCtx)
		}

		// Entries in the shard insert queue are either of:
		// - new entries
		// - existing entries that we've taken a ref on (marked as releaseEntryRef)
		if releaseEntryRef {
			entry.DecrementReaderWriterCount()
		}
	}

	var err error
	// index all requested entries in batch.
	if n := indexBatch.Len(); n > 0 {
		err = s.reverseIndex.WriteBatch(indexBatch)
		if err != nil {
			s.metrics.insertAsyncIndexErrors.Inc(int64(n))
		}
	}

	// Avoid goroutine spinning up to close this context
	ctx.BlockingClose()

	return err
}

func (s *dbShard) FetchBlocks(
	ctx context.Context,
	id ident.ID,
	starts []xtime.UnixNano,
	nsCtx namespace.Context,
) ([]block.FetchBlockResult, error) {
	s.RLock()
	entry, err := s.lookupEntryWithLock(id)
	if entry != nil {
		// NB(r): Ensure readers have consistent view of this series, do
		// not expire the series while being read from.
		entry.IncrementReaderWriterCount()
		defer entry.DecrementReaderWriterCount()
	}
	s.RUnlock()

	if err == errShardEntryNotFound {
		switch s.opts.SeriesCachePolicy() {
		case series.CacheAll:
			// No-op, would be in memory if cached
			return nil, nil
		}
	} else if err != nil {
		return nil, err
	}

	if entry != nil {
		return entry.Series.FetchBlocks(ctx, starts, nsCtx)
	}

	retriever := s.seriesBlockRetriever
	onRetrieve := s.seriesOnRetrieveBlock
	opts := s.seriesOpts
	// Nil for onRead callback because we don't want peer bootstrapping to impact
	// the behavior of the LRU
	var onReadCb block.OnReadBlock
	reader := series.NewReaderUsingRetriever(id, retriever, onRetrieve, onReadCb, opts)
	return reader.FetchBlocks(ctx, starts, nsCtx)
}

func (s *dbShard) FetchBlocksForColdFlush(
	ctx context.Context,
	seriesID ident.ID,
	start xtime.UnixNano,
	version int,
	nsCtx namespace.Context,
) (block.FetchBlockResult, error) {
	s.RLock()
	entry, err := s.lookupEntryWithLock(seriesID)
	s.RUnlock()
	if entry == nil || err != nil {
		return block.FetchBlockResult{}, err
	}

	return entry.Series.FetchBlocksForColdFlush(ctx, start, version, nsCtx)
}

func (s *dbShard) fetchActiveBlocksMetadata(
	ctx context.Context,
	start, end xtime.UnixNano,
	limit int64,
	indexCursor int64,
	opts series.FetchBlocksMetadataOptions,
) (block.FetchBlocksMetadataResults, *int64, error) {
	var (
		res             = s.opts.FetchBlocksMetadataResultsPool().Get()
		fetchCtx        = s.contextPool.Get()
		nextIndexCursor *int64
	)

	var loopErr error
	s.forEachShardEntry(func(entry *Entry) bool {
		// Break out of the iteration loop once we've accumulated enough entries.
		if int64(len(res.Results())) >= limit {
			next := int64(entry.Index)
			nextIndexCursor = &next
			return false
		}

		// Fast forward past indexes lower than page token
		if int64(entry.Index) < indexCursor {
			return true
		}

		// Use a context here that we finalize immediately so the stream
		// readers can be returned to pool after we finish fetching the
		// metadata for this series.
		// NB(r): Use a pooled context for pooled finalizers/closers but
		// reuse so don't need to put and get from the pool each iteration.
		fetchCtx.Reset()
		metadata, err := entry.Series.FetchBlocksMetadata(ctx, start, end, opts)
		fetchCtx.BlockingCloseReset()
		if err != nil {
			loopErr = err
			return false
		}

		// If the blocksMetadata is empty, the series have no data within the specified
		// time range so we don't return it to the client
		if len(metadata.Blocks.Results()) == 0 {
			metadata.Blocks.Close()
			return true
		}

		// Otherwise add it to the result which takes care of closing the metadata
		res.Add(metadata)

		return true
	})

	return res, nextIndexCursor, loopErr
}

func (s *dbShard) FetchBlocksMetadataV2(
	ctx context.Context,
	start, end xtime.UnixNano,
	limit int64,
	encodedPageToken PageToken,
	opts block.FetchBlocksMetadataOptions,
) (block.FetchBlocksMetadataResults, PageToken, error) {
	token := new(pagetoken.PageToken)
	if encodedPageToken != nil {
		if err := proto.Unmarshal(encodedPageToken, token); err != nil {
			return nil, nil, xerrors.NewInvalidParamsError(errShardInvalidPageToken)
		}
	} else {
		// NB(bodu): Allow callers to specify that they only want results from disk.
		if opts.OnlyDisk {
			token.FlushedSeriesPhase = &pagetoken.PageToken_FlushedSeriesPhase{}
		}
	}

	// NB(r): If returning mixed in memory and disk results, then we return anything
	// that's mutable in memory first then all disk results.
	// We work backwards so we don't hit race conditions with blocks
	// being flushed and potentially missed between paginations. Working
	// backwards means that we might duplicate metadata sent back switching
	// between active phase and flushed phase, but that's better than missing
	// data working in the opposite direction. De-duping which block time ranges
	// were actually sent is also difficult as it's not always a consistent view
	// across async pagination.
	// Duplicating the metadata sent back means that consumers get a consistent
	// view of the world if they merge all the results together.
	// In the future we should consider the lifecycle of fileset files rather
	// than directly working with them here while filesystem cleanup manager
	// could delete them mid-read, on linux this is ok as it's just an unlink
	// and we'll finish our read cleanly. If there's a race between us thinking
	// the file is accessible and us opening a reader to it then this will bubble
	// an error to the client which will be retried.
	var (
		activePhase  = token.ActiveSeriesPhase
		flushedPhase = token.FlushedSeriesPhase
	)
	if flushedPhase == nil {
		// If first phase started or no phases started then return active
		// series metadata until we find a block start time that we have fileset
		// files for.
		indexCursor := int64(0)
		if activePhase != nil {
			indexCursor = activePhase.IndexCursor
		}
		// We do not include cached blocks because we'll send metadata for
		// those blocks when we send metadata directly from the flushed files.
		seriesFetchBlocksMetadataOpts := series.FetchBlocksMetadataOptions{
			FetchBlocksMetadataOptions: opts,
		}
		result, nextIndexCursor, err := s.fetchActiveBlocksMetadata(ctx, start, end,
			limit, indexCursor, seriesFetchBlocksMetadataOpts)
		if err != nil {
			return nil, nil, err
		}

		// Encode the next page token.
		if nextIndexCursor == nil {
			// Next phase, no more results from active series.
			token = &pagetoken.PageToken{
				FlushedSeriesPhase: &pagetoken.PageToken_FlushedSeriesPhase{},
			}
		} else {
			// This phase is still active.
			token = &pagetoken.PageToken{
				ActiveSeriesPhase: &pagetoken.PageToken_ActiveSeriesPhase{
					IndexCursor: *nextIndexCursor,
				},
			}
		}

		data, err := proto.Marshal(token)
		if err != nil {
			return nil, nil, err
		}

		return result, PageToken(data), nil
	}

	// Must be in the second phase, start with checking the latest possible
	// flushed block and work backwards.
	var (
		result    = s.opts.FetchBlocksMetadataResultsPool().Get()
		ropts     = s.namespace.Options().RetentionOptions()
		blockSize = ropts.BlockSize()
		// Subtract one blocksize because all fetch requests are exclusive on the end side.
		blockStart      = end.Truncate(blockSize).Add(-1 * blockSize)
		now             = xtime.ToUnixNano(s.nowFn())
		tokenBlockStart xtime.UnixNano
		numResults      int64
	)
	if flushedPhase.CurrBlockStartUnixNanos > 0 {
		tokenBlockStart = xtime.UnixNano(flushedPhase.CurrBlockStartUnixNanos)
		blockStart = tokenBlockStart
	}

	// Work backwards while in requested range and not before retention.
	for !blockStart.Before(start) &&
		!blockStart.Before(retention.FlushTimeStart(ropts, now)) {
		exists, err := s.namespaceReaderMgr.filesetExistsAt(s.shard, blockStart)
		if err != nil {
			return nil, nil, err
		}
		if !exists {
			// No fileset files here.
			blockStart = blockStart.Add(-1 * blockSize)
			continue
		}

		var pos readerPosition
		if !tokenBlockStart.IsZero() {
			// Was previously seeking through a previous block, need to validate
			// this is the correct one we found otherwise the file just went missing.
			if !blockStart.Equal(tokenBlockStart) {
				return nil, nil, fmt.Errorf(
					"was reading block at %v but next available block is: %v",
					tokenBlockStart, blockStart)
			}

			// Do not need to check if we move onto the next block that it matches
			// the token's block start on next iteration.
			tokenBlockStart = 0

			pos.metadataIdx = int(flushedPhase.CurrBlockEntryIdx)
			pos.volume = int(flushedPhase.Volume)
		}

		// Open a reader at this position, potentially from cache.
		reader, err := s.namespaceReaderMgr.get(s.shard, blockStart, pos)
		if err != nil {
			return nil, nil, err
		}

		for numResults < limit {
			id, tags, size, checksum, err := reader.ReadMetadata()
			if err == io.EOF {
				// Clean end of volume, we can break now.
				if err := reader.Close(); err != nil {
					return nil, nil, fmt.Errorf(
						"could not close metadata reader for block %v: %v",
						blockStart, err)
				}
				break
			}
			if err != nil {
				// Best effort to close the reader on a read error.
				if err := reader.Close(); err != nil {
					s.logger.Error("could not close reader on unexpected err", zap.Error(err))
				}
				return nil, nil, fmt.Errorf(
					"could not read metadata for block %v: %v",
					blockStart, err)
			}

			blockResult := s.opts.FetchBlockMetadataResultsPool().Get()
			value := block.FetchBlockMetadataResult{
				Start: blockStart,
			}
			if opts.IncludeSizes {
				value.Size = int64(size)
			}
			if opts.IncludeChecksums {
				v := checksum
				value.Checksum = &v
			}
			blockResult.Add(value)

			numResults++
			result.Add(block.NewFetchBlocksMetadataResult(id, tags,
				blockResult))
		}

		endPos := int64(reader.MetadataRead())
		// This volume may be different from the one initially requested,
		// e.g. if there was a compaction between the last call and this
		// one, so be sure to update the state of the pageToken. If this is not
		// updated, the request would have to start from the beginning since it
		// would be requesting a stale volume, which could result in an infinite
		// loop of requests that never complete.
		volume := int64(reader.Status().Volume)

		// Return the reader to the cache. Since this is effectively putting
		// the reader into a shared pool, don't use the reader after this call.
		err = s.namespaceReaderMgr.put(reader)
		if err != nil {
			return nil, nil, err
		}

		if numResults >= limit {
			// We hit the limit, return results with page token.
			token = &pagetoken.PageToken{
				FlushedSeriesPhase: &pagetoken.PageToken_FlushedSeriesPhase{
					CurrBlockStartUnixNanos: int64(blockStart),
					CurrBlockEntryIdx:       endPos,
					Volume:                  volume,
				},
			}
			data, err := proto.Marshal(token)
			if err != nil {
				return nil, nil, err
			}
			return result, data, nil
		}

		// Otherwise we move on to the previous block.
		blockStart = blockStart.Add(-1 * blockSize)
	}

	// No more results if we fall through.
	return result, nil, nil
}

func (s *dbShard) PrepareBootstrap(ctx context.Context) error {
	ctx, span, sampled := ctx.StartSampledTraceSpan(tracepoint.ShardPrepareBootstrap)
	defer span.Finish()

	if sampled {
		span.LogFields(log.Int("shard", int(s.shard)))
	}

	// Iterate flushed time ranges to determine which blocks are retrievable.
	// NB(r): This must be done before bootstrap since during bootstrapping
	// series will load blocks into series with series.LoadBlock(...) which
	// needs to ask the shard whether certain time windows have been flushed or
	// not.
	s.initializeFlushStates()
	return nil
}

func (s *dbShard) initializeFlushStates() {
	s.flushState.RLock()
	initialized := s.flushState.initialized
	s.flushState.RUnlock()
	if initialized {
		return
	}

	defer func() {
		s.flushState.Lock()
		s.flushState.initialized = true
		s.flushState.Unlock()
	}()

	s.UpdateFlushStates()
	return
}

func (s *dbShard) UpdateFlushStates() {
	fsOpts := s.opts.CommitLogOptions().FilesystemOptions()
	readInfoFilesResults := fs.ReadInfoFiles(fsOpts.FilePathPrefix(), s.namespace.ID(), s.shard,
		fsOpts.InfoReaderBufferSize(), fsOpts.DecodingOptions(), persist.FileSetFlushType)

	for _, result := range readInfoFilesResults {
		if err := result.Err.Error(); err != nil {
			s.logger.Error("unable to read info files in shard bootstrap",
				zap.Uint32("shard", s.ID()),
				zap.Stringer("namespace", s.namespace.ID()),
				zap.String("filepath", result.Err.Filepath()),
				zap.Error(err))
			continue
		}

		info := result.Info
		at := xtime.UnixNano(info.BlockStart)
		currState := s.flushStateNoBootstrapCheck(at)

		if currState.WarmStatus.DataFlushed != fileOpSuccess {
			s.markWarmDataFlushStateSuccess(at)
		}

		// Cold version needs to get bootstrapped so that the 1:1 relationship
		// between volume number and cold version is maintained and the volume
		// numbers / flush versions remain monotonically increasing.
		//
		// Note that there can be multiple info files for the same block, for
		// example if the database didn't get to clean up compacted filesets
		// before terminating.
		if currState.ColdVersionRetrievable < info.VolumeIndex {
			s.setFlushStateColdVersionRetrievable(at, info.VolumeIndex)
			s.setFlushStateColdVersionFlushed(at, info.VolumeIndex)
		}
	}

	// Populate index flush state only if enabled.
	if !s.indexEnabled {
		return
	}

	blockSize := s.namespace.Options().RetentionOptions().BlockSize()
	indexBlockSize := s.namespace.Options().IndexOptions().BlockSize()

	indexFlushedBlockStarts := s.reverseIndex.WarmFlushBlockStarts()
	for _, blockStart := range indexFlushedBlockStarts {
		// Index block size is wider than data block size, so we want to set all data blockStarts
		// within the range of a given index blockStart
		blockEnd := blockStart.Add(indexBlockSize)
		for at := blockStart; at < blockEnd; at = at.Add(blockSize) {
			currState := s.flushStateNoBootstrapCheck(at)
			if currState.WarmStatus.IndexFlushed != fileOpSuccess {
				s.markWarmIndexFlushStateSuccess(at)
			}
		}
	}
}

func (s *dbShard) Bootstrap(
	ctx context.Context,
	nsCtx namespace.Context,
) error {
	ctx, span, sampled := ctx.StartSampledTraceSpan(tracepoint.ShardBootstrap)
	defer span.Finish()

	if sampled {
		span.LogFields(log.Int("shard", int(s.shard)))
	}

	s.Lock()
	if s.bootstrapState == Bootstrapped {
		s.Unlock()
		return errShardAlreadyBootstrapped
	}
	if s.bootstrapState == Bootstrapping {
		s.Unlock()
		return errShardIsBootstrapping
	}
	s.bootstrapState = Bootstrapping
	s.Unlock()

	multiErr := xerrors.NewMultiError()

	// Initialize the flush states if we haven't called prepare bootstrap.
	if err := s.PrepareBootstrap(ctx); err != nil {
		multiErr = multiErr.Add(err)
	}

	// Now that this shard has finished bootstrapping, attempt to cache all of its seekers. Cannot call
	// this earlier as block lease verification will fail due to the shards not being bootstrapped
	// (and as a result no leases can be verified since the flush state is not yet known).
	if err := s.cacheShardIndices(); err != nil {
		multiErr = multiErr.Add(err)
	}

	// Move any bootstrap buffers into position for reading.
	s.forEachShardEntry(func(entry *Entry) bool {
		if err := entry.Series.Bootstrap(nsCtx); err != nil {
			multiErr = multiErr.Add(err)
		}
		return true
	})

	s.Lock()
	s.bootstrapState = Bootstrapped
	s.Unlock()

	return multiErr.FinalError()
}

func (s *dbShard) LoadBlocks(
	seriesToLoad *result.Map,
) error {
	if seriesToLoad == nil {
		return errTriedToLoadNilSeries
	}

	s.Lock()
	// Don't allow loads until the shard is bootstrapped because the shard flush states need to be
	// bootstrapped in order to safely load blocks. This also keeps things simpler to reason about.
	if s.bootstrapState != Bootstrapped {
		s.Unlock()
		return errShardIsNotBootstrapped
	}
	s.Unlock()

	memTracker := s.opts.MemoryTracker()
	estimatedSize := result.EstimateMapBytesSize(seriesToLoad)
	ok := memTracker.IncNumLoadedBytes(estimatedSize)
	if !ok {
		return ErrDatabaseLoadLimitHit
	}

	multiErr := xerrors.NewMultiError()
	for _, elem := range seriesToLoad.Iter() {
		dbBlocks := elem.Value()
		id := dbBlocks.ID
		tags := dbBlocks.Tags

		canFinalizeTagsAll := true
		for _, block := range dbBlocks.Blocks.AllBlocks() {
			result, err := s.loadBlock(id, tags, block)
			if err != nil {
				multiErr = multiErr.Add(err)
			}

			canFinalizeTagsAll = canFinalizeTagsAll && result.canFinalizeTags
		}

		if canFinalizeTagsAll {
			tags.Finalize()
		}
	}

	return multiErr.FinalError()
}

type loadBlockResult struct {
	canFinalizeTags bool
}

func (s *dbShard) loadBlock(
	id ident.ID,
	tags ident.Tags,
	block block.DatabaseBlock,
) (loadBlockResult, error) {
	var (
		timestamp = block.StartTime()
		result    loadBlockResult
	)

	// First lookup if series already exists.
	entry, shardOpts, err := s.TryRetrieveSeriesAndIncrementReaderWriterCount(id)
	if err != nil && err != errShardEntryNotFound {
		return result, err
	}
	if entry == nil {
		// Synchronously insert to avoid waiting for the insert queue which could potentially
		// delay the insert.
		entry, err = s.insertSeriesSync(id, convert.NewTagsMetadataResolver(tags),
			insertSyncOptions{
				// NB(r): Because insertSyncIncReaderWriterCount is used here we
				// don't need to explicitly increment the reader/writer count and it
				// will happen while the write lock is held so that it can't immediately
				// be expired.
				insertType:      insertSyncIncReaderWriterCount,
				hasPendingIndex: s.reverseIndex != nil,
				pendingIndex: dbShardPendingIndex{
					timestamp:  timestamp,
					enqueuedAt: s.nowFn(),
				},
			})
		if err != nil {
			return result, err
		}
	} else {
		// No longer needed as we found the series and we don't require
		// them for insertion.
		// FOLLOWUP(r): Audit places that keep refs to the ID from a
		// bootstrap result, newShardEntry copies it but some of the
		// bootstrapped blocks when using certain series cache policies
		// keeps refs to the ID with seriesID, so for now these IDs will
		// be garbage collected)
		result.canFinalizeTags = true
	}

	// Always decrement the reader writer count.
	defer entry.DecrementReaderWriterCount()

	// NB(rartoul): The data being loaded is not part of the bootstrap process then it needs to be
	// loaded as a cold write because the load could be happening concurrently with
	// other processes like the flush (as opposed to bootstrap which cannot happen
	// concurrently with a flush) and there is no way to know if this series/block
	// combination has been warm flushed or not yet since updating the shard block state
	// doesn't happen until the entire flush completes.
	//
	// As a result the only safe operation is to load the block as a cold write which
	// ensures that the data will eventually be flushed and merged with the existing data
	// on disk in the two scenarios where the Load() API is used (cold writes and repairs).
	if err := entry.Series.LoadBlock(block, series.ColdWrite); err != nil {
		return result, err
	}
	// Cannot close blocks once done as series takes ref to them.

	// Check if needs to be reverse indexed.
	if s.reverseIndex != nil &&
		entry.NeedsIndexUpdate(s.reverseIndex.BlockStartForWriteTime(timestamp)) {
		err = s.insertSeriesForIndexingAsyncBatched(entry, timestamp,
			shardOpts.WriteNewSeriesAsync)
		if err != nil {
			return result, err
		}
	}

	return result, nil
}

func (s *dbShard) cacheShardIndices() error {
	retriever := s.DatabaseBlockRetriever
	// May be nil depending on the caching policy.
	if retriever == nil {
		return nil
	}

	s.logger.Debug("caching shard indices", zap.Uint32("shard", s.ID()))
	if err := retriever.CacheShardIndices([]uint32{s.ID()}); err != nil {
		s.logger.Error("caching shard indices error",
			zap.Uint32("shard", s.ID()),
			zap.Error(err))
		return err
	}

	s.logger.Debug("caching shard indices completed successfully",
		zap.Uint32("shard", s.ID()))
	return nil
}

func (s *dbShard) WarmFlush(
	blockStart xtime.UnixNano,
	flushPreparer persist.FlushPreparer,
	nsCtx namespace.Context,
) error {
	// We don't flush data when the shard is still bootstrapping
	s.RLock()
	if s.bootstrapState != Bootstrapped {
		s.RUnlock()
		return errShardNotBootstrappedToFlush
	}
	s.RUnlock()

	prepareOpts := persist.DataPrepareOptions{
		NamespaceMetadata: s.namespace,
		Shard:             s.ID(),
		BlockStart:        blockStart,
		// Volume index is always 0 for warm flushes because a warm flush must
		// happen first before cold flushes happen.
		VolumeIndex: 0,
		// We explicitly set delete if exists to false here as we track which
		// filesets exist at bootstrap time so we should never encounter a time
		// where a fileset already exists when we attempt to flush unless there
		// is a bug in the code.
		DeleteIfExists: false,
		FileSetType:    persist.FileSetFlushType,
	}
	prepared, err := flushPreparer.PrepareData(prepareOpts)
	if err != nil {
		return err
	}

	var multiErr xerrors.MultiError
	flushCtx := s.contextPool.Get() // From pool so finalizers are from pool.

	flushResult := dbShardFlushResult{}
	s.forEachShardEntry(func(entry *Entry) bool {
		curr := entry.Series
		// Use a temporary context here so the stream readers can be returned to
		// the pool after we finish fetching flushing the series.
		flushCtx.Reset()
		flushOutcome, err := curr.WarmFlush(flushCtx, blockStart, prepared.Persist, nsCtx)
		// Use BlockingCloseReset so context doesn't get returned to the pool.
		flushCtx.BlockingCloseReset()

		if err != nil {
			multiErr = multiErr.Add(err)
			// If we encounter an error when persisting a series, don't continue as
			// the file on disk could be in a corrupt state.
			return false
		}

		flushResult.update(flushOutcome)

		return true
	})

	s.logFlushResult(flushResult)

	if err := prepared.Close(); err != nil {
		multiErr = multiErr.Add(err)
	}

	return s.markWarmDataFlushStateSuccessOrError(blockStart, multiErr.FinalError())
}

func (s *dbShard) ColdFlush(
	flushPreparer persist.FlushPreparer,
	resources coldFlushReusableResources,
	nsCtx namespace.Context,
	onFlushSeries persist.OnFlushSeries,
) (ShardColdFlush, error) {
	// We don't flush data when the shard is still bootstrapping.
	s.RLock()
	if s.bootstrapState != Bootstrapped {
		s.RUnlock()
		return shardColdFlush{}, errShardNotBootstrappedToFlush
	}
	// Use blockStatesSnapshotWithRLock to avoid having to re-acquire read lock.
	blockStates := s.blockStatesSnapshotWithRLock()
	s.RUnlock()

	resources.reset()
	var (
		multiErr           xerrors.MultiError
		dirtySeries        = resources.dirtySeries
		dirtySeriesToWrite = resources.dirtySeriesToWrite
		idElementPool      = resources.idElementPool
	)

	blockStatesSnapshot, bootstrapped := blockStates.UnwrapValue()
	if !bootstrapped {
		return shardColdFlush{}, errFlushStateIsNotInitialized
	}

	var (
		// forEachShardEntry should not execute in parallel, but protect with a lock anyways for paranoia.
		loopErrLock sync.Mutex
		loopErr     error
	)
	// First, loop through all series to capture data on which blocks have dirty
	// series and add them to the resources for further processing.
	s.forEachShardEntry(func(entry *Entry) bool {
		curr := entry.Series
		seriesMetadata := curr.Metadata()
		blockStarts := curr.ColdFlushBlockStarts(blockStatesSnapshot)
		blockStarts.ForEach(func(t xtime.UnixNano) {
			// Cold flushes can only happen on blockStarts that have been
			// warm flushed, because warm flush logic does not currently
			// perform any merging logic.
			hasWarmFlushed, err := s.hasWarmFlushed(t)
			if err != nil {
				loopErrLock.Lock()
				loopErr = err
				loopErrLock.Unlock()
				return
			}
			if !hasWarmFlushed {
				return
			}

			seriesList := dirtySeriesToWrite[t]
			if seriesList == nil {
				seriesList = newIDList(idElementPool)
				dirtySeriesToWrite[t] = seriesList
			}
			element := seriesList.PushBack(seriesMetadata)

			dirtySeries.Set(idAndBlockStart{
				blockStart: t,
				id:         seriesMetadata.ID,
			}, element)
		})

		return true
	})
	if loopErr != nil {
		return shardColdFlush{}, loopErr
	}

	if dirtySeries.Len() == 0 {
		// Early exit if there is nothing dirty to merge. dirtySeriesToWrite
		// may be non-empty when dirtySeries is empty because we purposely
		// leave empty seriesLists in the dirtySeriesToWrite map to avoid having
		// to reallocate them in subsequent usages of the shared resource.
		return shardColdFlush{}, nil
	}

	flush := shardColdFlush{
		shard:   s,
		doneFns: make([]shardColdFlushDone, 0, len(dirtySeriesToWrite)),
	}
	merger := s.newMergerFn(resources.fsReader, s.opts.DatabaseBlockOptions().DatabaseBlockAllocSize(),
		s.opts.SegmentReaderPool(), s.opts.MultiReaderIteratorPool(),
		s.opts.IdentifierPool(), s.opts.EncoderPool(), s.opts.ContextPool(),
		s.opts.CommitLogOptions().FilesystemOptions().FilePathPrefix(), s.namespace.Options())
	mergeWithMem := s.newFSMergeWithMemFn(s, s, dirtySeries, dirtySeriesToWrite)
	// Loop through each block that we know has ColdWrites. Since each block
	// has its own fileset, if we encounter an error while trying to persist
	// a block, we continue to try persisting other blocks.
	for startTime := range dirtySeriesToWrite {
		coldVersion, err := s.RetrievableBlockColdVersion(startTime)
		if err != nil {
			multiErr = multiErr.Add(err)
			continue
		}

		fsID := fs.FileSetFileIdentifier{
			Namespace:   s.namespace.ID(),
			Shard:       s.ID(),
			BlockStart:  startTime,
			VolumeIndex: coldVersion,
		}

		nextVersion := coldVersion + 1
		close, err := merger.Merge(fsID, mergeWithMem, nextVersion, flushPreparer, nsCtx,
			onFlushSeries)
		if err != nil {
			multiErr = multiErr.Add(err)
			continue
		}
		flush.doneFns = append(flush.doneFns, shardColdFlushDone{
			startTime:   startTime,
			nextVersion: nextVersion,
			close:       close,
		})
	}
	return flush, multiErr.FinalError()
}

func (s *dbShard) FilterBlocksNeedSnapshot(blockStarts []xtime.UnixNano) []xtime.UnixNano {
	if !s.IsBootstrapped() {
		return nil
	}

	needs := map[xtime.UnixNano]struct{}{}
	s.forEachShardEntry(func(entry *Entry) bool {
		entry.Series.MarkNonEmptyBlocks(needs)
		if len(needs) < len(blockStarts) {
			return true
		}
		// Note: entries.Series might have non empty blocks that are not contained in blockStarts.
		// This prevents usage of len(needs) < len(blockStarts) as early exit criteria.
		for _, bl := range blockStarts {
			if _, ok := needs[bl]; !ok {
				return true
			}
		}
		return false
	})

	// Note: doing this to keep original ordering. Not sure if that matters though.
	filtered := make([]xtime.UnixNano, 0, len(needs))
	for _, bl := range blockStarts {
		if _, ok := needs[bl]; ok {
			filtered = append(filtered, bl)
		}
	}
	return filtered
}

func (s *dbShard) Snapshot(
	blockStart xtime.UnixNano,
	snapshotTime xtime.UnixNano,
	snapshotPreparer persist.SnapshotPreparer,
	nsCtx namespace.Context,
) (ShardSnapshotResult, error) {
	// We don't snapshot data when the shard is still bootstrapping
	if !s.IsBootstrapped() {
		return ShardSnapshotResult{}, errShardNotBootstrappedToSnapshot
	}

	// Record per-shard snapshot latency, not many shards so safe
	// to use a timer.
	totalTimer := s.metrics.snapshotTotalLatency.Start()
	defer totalTimer.Stop()

	prepareOpts := persist.DataPrepareOptions{
		NamespaceMetadata: s.namespace,
		Shard:             s.ID(),
		BlockStart:        blockStart,
		FileSetType:       persist.FileSetSnapshotType,
		// We explicitly set delete if exists to false here as we do not
		// expect there to be a collision as snapshots files are appended
		// with a monotonically increasing number to avoid collisions, there
		// would have to be a competing process to cause a collision.
		DeleteIfExists: false,
		Snapshot: persist.DataPrepareSnapshotOptions{
			SnapshotTime: snapshotTime,
		},
	}
	prepareTimer := s.metrics.snapshotPrepareLatency.Start()
	prepared, err := snapshotPreparer.PrepareData(prepareOpts)
	prepareTimer.Stop()
	if err != nil {
		return ShardSnapshotResult{}, err
	}

	var (
		snapshotCtx = s.contextPool.Get()
		persist     int
		stats       series.SnapshotResultStats
		multiErr    xerrors.MultiError
	)
	s.forEachShardEntry(func(entry *Entry) bool {
		series := entry.Series
		// Use a temporary context here so the stream readers can be returned to
		// pool after we finish fetching flushing the series
		snapshotCtx.Reset()
		result, err := series.Snapshot(snapshotCtx, blockStart, prepared.Persist, nsCtx)
		snapshotCtx.BlockingCloseReset()

		if err != nil {
			multiErr = multiErr.Add(err)
			// If we encounter an error when persisting a series, don't continue as
			// the file on disk could be in a corrupt state.
			return false
		}

		if result.Persist {
			persist++
		}

		// Add snapshot result to cumulative result.
		stats.Add(result.Stats)
		return true
	})

	// Emit cumulative snapshot result timings.
	if multiErr.NumErrors() == 0 {
		s.metrics.snapshotMergeByBucketLatency.Record(stats.TimeMergeByBucket)
		s.metrics.snapshotMergeAcrossBucketsLatency.Record(stats.TimeMergeAcrossBuckets)
		s.metrics.snapshotChecksumLatency.Record(stats.TimeChecksum)
		s.metrics.snapshotPersistLatency.Record(stats.TimePersist)
	}

	closeTimer := s.metrics.snapshotCloseLatency.Start()
	multiErr = multiErr.Add(prepared.Close())
	closeTimer.Stop()

	if err := multiErr.FinalError(); err != nil {
		return ShardSnapshotResult{}, err
	}

	return ShardSnapshotResult{
		SeriesPersist: persist,
	}, nil
}

func (s *dbShard) FlushState(blockStart xtime.UnixNano) (fileOpState, error) {
	s.flushState.RLock()
	initialized := s.flushState.initialized
	state := s.flushStateWithRLock(blockStart)
	s.flushState.RUnlock()

	if !initialized {
		return fileOpState{}, errFlushStateIsNotInitialized
	}

	return state, nil
}

func (s *dbShard) flushStateNoBootstrapCheck(blockStart xtime.UnixNano) fileOpState {
	s.flushState.RLock()
	check := s.flushStateWithRLock(blockStart)
	s.flushState.RUnlock()
	return check
}

func (s *dbShard) flushStateWithRLock(blockStart xtime.UnixNano) fileOpState {
	state, ok := s.flushState.statesByTime[blockStart]
	if !ok {
		return fileOpState{WarmStatus: warmStatus{
			DataFlushed:  fileOpNotStarted,
			IndexFlushed: fileOpNotStarted,
		}}
	}
	return state
}

func (s *dbShard) markWarmDataFlushStateSuccessOrError(blockStart xtime.UnixNano, err error) error {
	// Track flush state for block state
	if err == nil {
		s.markWarmDataFlushStateSuccess(blockStart)
	} else {
		s.markWarmDataFlushStateFail(blockStart)
	}
	return err
}

func (s *dbShard) markWarmDataFlushStateSuccess(blockStart xtime.UnixNano) {
	s.flushState.Lock()
	state := s.flushState.statesByTime[blockStart]
	state.WarmStatus.DataFlushed = fileOpSuccess
	s.flushState.statesByTime[blockStart] = state
	s.flushState.Unlock()
}

func (s *dbShard) markWarmDataFlushStateFail(blockStart xtime.UnixNano) {
	s.flushState.Lock()
	state := s.flushState.statesByTime[blockStart]
	state.WarmStatus.DataFlushed = fileOpFailed
	state.NumFailures++
	s.flushState.statesByTime[blockStart] = state
	s.flushState.Unlock()
}

// MarkWarmIndexFlushStateSuccessOrError marks the blockStart as
// success or fail based on the provided err.
func (s *dbShard) MarkWarmIndexFlushStateSuccessOrError(blockStart xtime.UnixNano, err error) {
	// Track flush state for block state
	if err == nil {
		s.markWarmIndexFlushStateSuccess(blockStart)
	} else {
		s.markWarmIndexFlushStateFail(blockStart)
	}
}

func (s *dbShard) markWarmIndexFlushStateSuccess(blockStart xtime.UnixNano) {
	s.flushState.Lock()
	state := s.flushState.statesByTime[blockStart]
	state.WarmStatus.IndexFlushed = fileOpSuccess
	s.flushState.statesByTime[blockStart] = state
	s.flushState.Unlock()
}

func (s *dbShard) markWarmIndexFlushStateFail(blockStart xtime.UnixNano) {
	s.flushState.Lock()
	state := s.flushState.statesByTime[blockStart]
	state.WarmStatus.IndexFlushed = fileOpFailed
	state.NumFailures++
	s.flushState.statesByTime[blockStart] = state
	s.flushState.Unlock()
}

func (s *dbShard) setFlushStateColdVersionRetrievable(blockStart xtime.UnixNano, version int) {
	s.flushState.Lock()
	state := s.flushState.statesByTime[blockStart]
	state.ColdVersionRetrievable = version
	s.flushState.statesByTime[blockStart] = state
	s.flushState.Unlock()
}

func (s *dbShard) setFlushStateColdVersionFlushed(blockStart xtime.UnixNano, version int) {
	s.flushState.Lock()
	state := s.flushState.statesByTime[blockStart]
	state.ColdVersionFlushed = version
	s.flushState.statesByTime[blockStart] = state
	s.flushState.Unlock()
}

func (s *dbShard) removeAnyFlushStatesTooEarly(startTime xtime.UnixNano) {
	s.flushState.Lock()
	earliestFlush := retention.FlushTimeStart(s.namespace.Options().RetentionOptions(), startTime)
	for t := range s.flushState.statesByTime {
		if t.Before(earliestFlush) {
			delete(s.flushState.statesByTime, t)
		}
	}
	s.flushState.Unlock()
}

func (s *dbShard) CleanupExpiredFileSets(earliestToRetain xtime.UnixNano) error {
	filePathPrefix := s.opts.CommitLogOptions().FilesystemOptions().FilePathPrefix()
	expired, err := s.filesetPathsBeforeFn(filePathPrefix, s.namespace.ID(), s.ID(), earliestToRetain)
	if err != nil {
		return fmt.Errorf("encountered errors when getting fileset files for prefix %s namespace %s shard %d: %v",
			filePathPrefix, s.namespace.ID(), s.ID(), err)
	}

	return s.deleteFilesFn(expired)
}

func (s *dbShard) CleanupCompactedFileSets() error {
	filePathPrefix := s.opts.CommitLogOptions().FilesystemOptions().FilePathPrefix()
	filesets, err := s.filesetsFn(filePathPrefix, s.namespace.ID(), s.ID())
	if err != nil {
		return fmt.Errorf("encountered errors when getting fileset files for prefix %s namespace %s shard %d: %v",
			filePathPrefix, s.namespace.ID(), s.ID(), err)
	}

	// Get a snapshot of all states here to prevent constantly getting/releasing
	// locks in a tight loop below. This snapshot won't become stale halfway
	// through this because flushing and cleanup never happen in parallel.
	blockStates := s.BlockStatesSnapshot()
	blockStatesSnapshot, bootstrapped := blockStates.UnwrapValue()
	if !bootstrapped {
		return errShardIsNotBootstrapped
	}

	toDelete := fs.FileSetFilesSlice(make([]fs.FileSetFile, 0, len(filesets)))
	for _, datafile := range filesets {
		fileID := datafile.ID
		blockState := blockStatesSnapshot.Snapshot[fileID.BlockStart]
		if fileID.VolumeIndex < blockState.ColdVersion {
			toDelete = append(toDelete, datafile)
		}
	}

	return s.deleteFilesFn(toDelete.Filepaths())
}

func (s *dbShard) Repair(
	ctx context.Context,
	nsCtx namespace.Context,
	nsMeta namespace.Metadata,
	tr xtime.Range,
	repairer databaseShardRepairer,
) (repair.MetadataComparisonResult, error) {
	return repairer.Repair(ctx, nsCtx, nsMeta, tr, s)
}

func (s *dbShard) AggregateTiles(
	ctx context.Context,
	sourceNs, targetNs Namespace,
	shardID uint32,
	onFlushSeries persist.OnFlushSeries,
	opts AggregateTilesOptions,
) (int64, error) {
	var multiErr xerrors.MultiError

	processedTileCount, nextVolume, err := s.tileAggregator.AggregateTiles(
		ctx, sourceNs, targetNs, shardID, onFlushSeries, opts)
	if err != nil {
		// NB: cannot return on the error here, must finish writing.
		multiErr = multiErr.Add(err)
	} else {
		// Notify all block leasers that a new volume for the namespace/shard/blockstart
		// has been created. This will block until all leasers have relinquished their
		// leases.
		// NB: markWarmFlushStateSuccess=true because there are no flushes happening in this
		// flow, and we need to set WarmStatus to fileOpSuccess explicitly in order to make
		// the new blocks readable.
		if err = s.finishWriting(opts.Start, nextVolume, true); err != nil {
			multiErr = multiErr.Add(err)
		}
	}

	if err := multiErr.FinalError(); err != nil {
		return 0, err
	}

	s.logger.Debug("finished aggregating tiles",
		zap.Uint32("shard", s.ID()),
		zap.Int64("processedTiles", processedTileCount))

	return processedTileCount, nil
}

func (s *dbShard) BootstrapState() BootstrapState {
	s.RLock()
	bs := s.bootstrapState
	s.RUnlock()
	return bs
}

func (s *dbShard) DocRef(id ident.ID) (doc.Metadata, bool, error) {
	s.RLock()
	defer s.RUnlock()

	entry, err := s.lookupEntryWithLock(id)
	if err == nil {
		return entry.Series.Metadata(), true, nil
	}
	if err == errShardEntryNotFound {
		return emptyDoc, false, nil
	}
	return emptyDoc, false, err
}

func (s *dbShard) LatestVolume(blockStart xtime.UnixNano) (int, error) {
	return s.namespaceReaderMgr.latestVolume(s.shard, blockStart)
}

func (s *dbShard) OpenStreamingReader(blockStart xtime.UnixNano) (fs.DataFileSetReader, error) {
	latestVolume, err := s.LatestVolume(blockStart)
	if err != nil {
		return nil, err
	}

	reader, err := s.newReaderFn(s.opts.BytesPool(), s.opts.CommitLogOptions().FilesystemOptions())
	if err != nil {
		return nil, err
	}

	openOpts := fs.DataReaderOpenOptions{
		Identifier: fs.FileSetFileIdentifier{
			Namespace:   s.namespace.ID(),
			Shard:       s.ID(),
			BlockStart:  blockStart,
			VolumeIndex: latestVolume,
		},
		FileSetType:      persist.FileSetFlushType,
		StreamingEnabled: true,
	}

	if err := reader.Open(openOpts); err != nil {
		return nil, err
	}

	return reader, nil
}

func (s *dbShard) logFlushResult(r dbShardFlushResult) {
	s.logger.Debug("shard flush outcome",
		zap.Uint32("shard", s.ID()),
		zap.Int64("numBlockDoesNotExist", r.numBlockDoesNotExist),
	)
}

func (s *dbShard) finishWriting(
	blockStart xtime.UnixNano,
	nextVersion int,
	markWarmFlushStateSuccess bool,
) error {
	if markWarmFlushStateSuccess {
		s.markWarmDataFlushStateSuccess(blockStart)
		s.markWarmIndexFlushStateSuccess(blockStart)
	}

	// After writing the full block successfully update the ColdVersionFlushed number. This will
	// allow the SeekerManager to open a lease on the latest version of the fileset files because
	// the BlockLeaseVerifier will check the ColdVersionFlushed value, but the buffer only looks at
	// ColdVersionRetrievable so a concurrent tick will not yet cause the blocks in memory to be
	// evicted (which is the desired behavior because we haven't updated the open leases yet which
	// means the newly written data is not available for querying via the SeekerManager yet.)
	s.setFlushStateColdVersionFlushed(blockStart, nextVersion)

	// Notify all block leasers that a new volume for the namespace/shard/blockstart
	// has been created. This will block until all leasers have relinquished their
	// leases.
	_, err := s.opts.BlockLeaseManager().UpdateOpenLeases(block.LeaseDescriptor{
		Namespace:  s.namespace.ID(),
		Shard:      s.ID(),
		BlockStart: blockStart,
	}, block.LeaseState{Volume: nextVersion})
	// After writing the full block successfully **and** propagating the new lease to the
	// BlockLeaseManager, update the ColdVersionRetrievable in the flush state. Once this function
	// completes concurrent ticks will be able to evict the data from memory that was just flushed
	// (which is now safe to do since the SeekerManager has been notified of the presence of new
	// files).
	//
	// NB(rartoul): Ideally the ColdVersionRetrievable would only be updated if the call to UpdateOpenLeases
	// succeeded, but that would allow the ColdVersionRetrievable and ColdVersionFlushed numbers to drift
	// which would increase the complexity of the code to address a situation that is probably not
	// recoverable (failure to UpdateOpenLeases is an invariant violated error).
	s.setFlushStateColdVersionRetrievable(blockStart, nextVersion)
	if err != nil {
		instrument.EmitAndLogInvariantViolation(s.opts.InstrumentOptions(), func(l *zap.Logger) {
			l.With(
				zap.String("namespace", s.namespace.ID().String()),
				zap.Uint32("shard", s.ID()),
				zap.Time("blockStart", blockStart.ToTime()),
				zap.Int("nextVersion", nextVersion),
				zap.Error(err),
			).Error("failed to update open leases after updating flush state cold version")
		})
		return err
	}
	return nil
}

type shardColdFlushDone struct {
	startTime   xtime.UnixNano
	nextVersion int
	close       persist.DataCloser
}

type shardColdFlush struct {
	shard   *dbShard
	doneFns []shardColdFlushDone
}

func (s shardColdFlush) Done() error {
	multiErr := xerrors.NewMultiError()
	for _, done := range s.doneFns {
		startTime := done.startTime
		nextVersion := done.nextVersion

		if err := done.close(); err != nil {
			multiErr = multiErr.Add(err)
			continue
		}

		err := s.shard.finishWriting(startTime, nextVersion, false)
		if err != nil {
			multiErr = multiErr.Add(err)
		}
	}
	return multiErr.FinalError()
}

// dbShardFlushResult is a helper struct for keeping track of the result of flushing all the
// series in the shard.
type dbShardFlushResult struct {
	numBlockDoesNotExist int64
}

func (r *dbShardFlushResult) update(u series.FlushOutcome) {
	if u == series.FlushOutcomeBlockDoesNotExist {
		r.numBlockDoesNotExist++
	}
}
