package shared

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/fclairamb/dbbat/internal/store"
)

// Batch and queue sizing for the row writer.
//
// The batch caps are deliberately large. Batching pays by amortizing the
// INSERT round-trip, and a bulk insert of 1000 small JSONB rows costs barely
// more than one of 50 — while at ~1 ms per insert a 50-row cap would ceiling
// throughput near 50k rows/s, over a minute of pure insert time on a
// multi-million-row capture.
//
// The byte budget matters as much as the row count: 1000 rows is nothing for a
// single-column select and tens of megabytes for wide rows, so whichever cap
// trips first ends the batch.
const (
	// MaxBatchRows is the largest number of rows sent in one INSERT.
	MaxBatchRows = 1000
	// MaxBatchBytes is the largest row payload sent in one INSERT (8 MiB).
	MaxBatchBytes = 8 << 20
	// QueueCapacityRows is the depth of the submit queue, in rows.
	QueueCapacityRows = 4096
	// MaxQueuedBytes bounds the row payload waiting in the queue (32 MiB).
	// A row cap alone does not bound memory — one wide row can be megabytes —
	// so the queue enforces both.
	MaxQueuedBytes = 32 << 20
)

// sinkResolveTimeout bounds how long the drain goroutine waits for query
// records to exist before giving up on the rows that need them. Resolution
// normally happens long before the rows reach the head of a batch (the
// producer starts the parent INSERT as soon as it captures its first row), so
// this only exists so that a producer that dies without resolving its sink can
// never wedge the process-wide writer.
//
// The bound is per flush, not per row: one deadline is computed when a batch
// starts landing and shared by every row in it, and a sink that misses it is
// failed outright so its remaining rows — and its rows in later batches —
// resolve instantly. There is exactly one drain goroutine for the whole
// process, so a per-row bound would let a single dead producer stall all row
// persistence for (timeout x rows), which is hours rather than seconds.
const sinkResolveTimeout = 30 * time.Second

// RowStore is the slice of the store the row writer needs.
type RowStore interface {
	StoreQueryRows(ctx context.Context, rows []store.PendingQueryRow) error

	// SealQueryRowChain stamps the final head of a capture's tamper-evident
	// row chain onto its query. It is called once per capture, at the flush
	// barrier, because that is the first moment the head is final — see
	// store.SealQueryRowChain and docs/audit-chain.md. Cheap for a query that
	// captured nothing.
	SealQueryRowChain(ctx context.Context, queryUID uuid.UUID) error
}

// RowWriter persists captured result rows in batches, off the proxy's data
// path. One writer is shared by the whole process: its batches span protocols,
// sessions and queries, so a busy proxy running many small result sets issues
// one INSERT per ~1000 rows overall rather than one per query.
//
// The queue is a bounded channel drained opportunistically: the drain
// goroutine blocks for the first row, then takes whatever is already queued
// until a cap trips. Batches therefore size themselves to load — an idle
// producer gets batches of one and minimal latency, a fast producer gets full
// batches — with no timer to tune.
//
// Submitting from a capture path is non-blocking and drops on a full queue
// (see QuerySink.Add). The capture path runs inline with forwarding rows to
// the client, so a slow moment in dbbat's own storage must never become a
// stall on the customer's query.
//
// A nil *RowWriter is usable: every method is a no-op and NewSink returns a
// nil sink, which is likewise inert. That keeps the protocol call sites free
// of nil checks when there is no store to write to.
type RowWriter struct {
	store  RowStore
	logger *slog.Logger

	queue chan queueItem

	// queuedBytes is the row payload currently sitting in queue. Bounded by
	// MaxQueuedBytes so wide rows cannot blow memory up within the row cap.
	queuedBytes atomic.Int64

	// stop is closed by Close; the drain goroutine finishes the queue and
	// exits. The queue channel itself is never closed, so a producer racing
	// with shutdown can never panic on send.
	stop     chan struct{}
	stopOnce sync.Once
	stopped  atomic.Bool

	// done is closed when the drain goroutine has exited. Producers blocked
	// on a send or a barrier select on it so they cannot hang past shutdown.
	done chan struct{}

	ctx    context.Context //nolint:containedctx // the drain goroutine's lifetime context
	cancel context.CancelFunc
}

// queueItem is either a captured row or a flush barrier.
type queueItem struct {
	sink *QuerySink
	row  store.QueryRow
	// barrier, when non-nil, marks a flush barrier: the drain goroutine
	// flushes everything collected so far and then closes it.
	barrier chan struct{}
}

// GoroutineNameRowWriterDrain is what a panic in the drain loop is logged under.
const GoroutineNameRowWriterDrain = "captured row writer drain"

// NewRowWriter starts a writer against st. A nil store yields a nil writer,
// which is inert — callers with no store keep working unchanged.
func NewRowWriter(st RowStore, logger *slog.Logger) *RowWriter {
	if st == nil {
		return nil
	}

	if logger == nil {
		logger = slog.Default()
	}

	ctx, cancel := context.WithCancel(context.Background())

	w := &RowWriter{
		store:  st,
		logger: logger,
		queue:  make(chan queueItem, QueueCapacityRows),
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
		ctx:    ctx,
		cancel: cancel,
	}

	// Whole-loop guard, and one of the few places that is the right shape: run's
	// own `defer close(w.done)` releases every producer parked in AddAll or
	// Flush, so a recovered panic degrades capture process-wide instead of
	// ending the process. See RunGuarded.
	go RunGuarded(ctx, logger, GoroutineNameRowWriterDrain, w.run)

	return w
}

// NewSink returns a sink for one query whose record does not exist yet. Rows
// may be submitted immediately; they are held in the queue and only inserted
// once Resolve supplies the parent query uid, which is what keeps the
// query_rows -> queries foreign key satisfied without the producer ever
// blocking on the parent INSERT.
func (w *RowWriter) NewSink() *QuerySink {
	if w == nil {
		return nil
	}

	return &QuerySink{writer: w, ready: make(chan struct{})}
}

// NewSinkFor returns a sink for a query whose record already exists.
func (w *RowWriter) NewSinkFor(queryUID uuid.UUID) *QuerySink {
	sink := w.NewSink()
	sink.Resolve(queryUID)

	return sink
}

// Close drains what is queued and stops the writer. It is safe to call twice.
func (w *RowWriter) Close(ctx context.Context) {
	if w == nil {
		return
	}

	w.stopOnce.Do(func() {
		w.stopped.Store(true)
		close(w.stop)
	})

	select {
	case <-w.done:
	case <-ctx.Done():
	}

	w.cancel()
}

// run is the drain loop: block for the first item, take whatever else is
// already queued until a cap trips or the queue runs dry, then flush.
func (w *RowWriter) run() {
	defer close(w.done)

	// Producers that only ever call Add never block, so w.done alone would not
	// stop them queueing rows into a channel nobody drains. Marking the writer
	// stopped is what turns those into an honest "dropped". On the normal exit
	// path Close has already set it, so this changes nothing there.
	defer w.stopped.Store(true)

	batch := &rowBatch{}

	// A panic below leaves this batch unwritten while its captures still claim a
	// complete result set, and the recover above cannot know which sinks were in
	// flight. Marking them here is what keeps a lost capture reading as lost. On
	// every normal exit the batch has already been flushed and reset, so this is
	// a no-op there.
	defer batch.markDropped()

	for {
		var item queueItem

		select {
		case item = <-w.queue:
		case <-w.stop:
			// Shutdown: take whatever is still queued, flush it, and go.
			for {
				select {
				case leftover := <-w.queue:
					batch.take(leftover)

					if batch.full() {
						w.flush(batch)
					}
				default:
					w.flush(batch)

					return
				}
			}
		}

		batch.take(item)
		w.drain(batch)
		w.flush(batch)
	}
}

// drain takes everything already queued into batch, without ever waiting. It
// stops on a cap or on a barrier (a barrier must be answered by the flush that
// covers the rows queued before it).
func (w *RowWriter) drain(batch *rowBatch) {
	for !batch.full() && !batch.barriered() {
		select {
		case next := <-w.queue:
			batch.take(next)
		default:
			return
		}
	}
}

// flush inserts the batch and answers any barrier it carried. A failed insert
// is logged and marks every contributing query as having dropped rows: the
// capture is now partial, and only saying so in the log would let the UI claim
// a complete result set.
func (w *RowWriter) flush(batch *rowBatch) {
	rows := make([]store.PendingQueryRow, 0, len(batch.items))
	contributors := make([]*QuerySink, 0, 4)

	// One deadline for the whole batch: see sinkResolveTimeout.
	deadline := time.Now().Add(sinkResolveTimeout)

	for _, item := range batch.items {
		w.queuedBytes.Add(-item.row.RowSizeBytes)

		queryUID, ok := item.sink.await(w.ctx, deadline)
		if !ok {
			// The parent query record never materialized, so the row has
			// nowhere to go.
			item.sink.markDropped()

			continue
		}

		rows = append(rows, store.PendingQueryRow{
			QueryID:      queryUID,
			RowNumber:    item.row.RowNumber,
			RowData:      item.row.RowData,
			RowSizeBytes: item.row.RowSizeBytes,
		})

		if !containsSink(contributors, item.sink) {
			contributors = append(contributors, item.sink)
		}
	}

	if len(rows) > 0 {
		if err := w.store.StoreQueryRows(w.ctx, rows); err != nil {
			w.logger.ErrorContext(w.ctx, "failed to store captured rows",
				slog.Int("rows", len(rows)),
				slog.Any("error", err))

			for _, sink := range contributors {
				sink.markDropped()
			}
		}
	}

	batch.reset()
}

func containsSink(sinks []*QuerySink, sink *QuerySink) bool {
	for _, s := range sinks {
		if s == sink {
			return true
		}
	}

	return false
}

// rowBatch accumulates one INSERT worth of rows plus the barriers that must be
// answered once it lands.
type rowBatch struct {
	items    []queueItem
	bytes    int64
	barriers []chan struct{}
}

func (b *rowBatch) take(item queueItem) {
	if item.barrier != nil {
		b.barriers = append(b.barriers, item.barrier)

		return
	}

	b.items = append(b.items, item)
	b.bytes += item.row.RowSizeBytes
}

func (b *rowBatch) full() bool {
	return len(b.items) >= MaxBatchRows || b.bytes >= MaxBatchBytes
}

func (b *rowBatch) barriered() bool {
	return len(b.barriers) > 0
}

// markDropped reports every capture contributing to this batch as having lost
// rows. Only the drain's panic path uses it: a batch that reaches flush is
// accounted for there, one that does not is simply gone.
func (b *rowBatch) markDropped() {
	for _, item := range b.items {
		item.sink.markDropped()
	}
}

// reset clears the batch and releases the barriers it carried — callers must
// only invoke it once the rows are durable (or definitively lost).
func (b *rowBatch) reset() {
	for _, barrier := range b.barriers {
		close(barrier)
	}

	b.items = b.items[:0]
	b.barriers = b.barriers[:0]
	b.bytes = 0
}

// QuerySink is one query's handle on the shared writer. It carries the parent
// query uid (once known) and records whether the capture lost rows.
//
// A nil sink is inert: Add reports the row as not accepted, everything else is
// a no-op. That is what a session with no store gets.
type QuerySink struct {
	writer *RowWriter

	// ready is closed by Resolve or Fail. The drain goroutine waits on it
	// before inserting the sink's rows, which is how the parent queries row
	// is guaranteed to exist first.
	ready       chan struct{}
	resolveOnce sync.Once
	queryUID    uuid.UUID
	failed      atomic.Bool

	// dropped records rows lost because the queue was full, a batch insert
	// failed, or the parent query record never landed. It is deliberately not
	// the same thing as truncation: truncation is a configured limit, a drop
	// is dbbat failing to keep up.
	dropped atomic.Bool
}

// Resolve publishes the uid of the query record the sink's rows belong to.
// Must be called exactly once, after the record has been inserted.
func (s *QuerySink) Resolve(queryUID uuid.UUID) {
	if s == nil {
		return
	}

	s.resolveOnce.Do(func() {
		s.queryUID = queryUID
		close(s.ready)
	})
}

// Fail reports that the query record could not be created, so the sink's rows
// have no parent to hang from. Queued rows are discarded and the capture is
// marked as having dropped rows.
func (s *QuerySink) Fail() {
	if s == nil {
		return
	}

	s.resolveOnce.Do(func() {
		s.failed.Store(true)
		s.dropped.Store(true)
		close(s.ready)
	})
}

// Add queues one captured row. It never blocks: on a full queue (or an
// exhausted byte budget) the row is dropped, the capture is marked degraded,
// and false is returned. This is the entry point for capture paths that run
// inline with forwarding rows to the client.
func (s *QuerySink) Add(row store.QueryRow) bool {
	if s == nil || s.writer == nil {
		return false
	}

	w := s.writer

	if w.stopped.Load() {
		s.dropped.Store(true)

		return false
	}

	if w.queuedBytes.Add(row.RowSizeBytes) > MaxQueuedBytes {
		w.queuedBytes.Add(-row.RowSizeBytes)
		s.dropped.Store(true)

		return false
	}

	select {
	case w.queue <- queueItem{sink: s, row: row}:
		return true
	default:
		w.queuedBytes.Add(-row.RowSizeBytes)
		s.dropped.Store(true)

		return false
	}
}

// AddAll queues a whole result set, waiting for queue space rather than
// dropping. It is only for callers that already hold every row in memory and
// run off the data path (MySQL, MongoDB, PostgreSQL COPY): those rows are
// already resident, so queueing them costs no extra memory, and dropping them
// would lose a capture that used to be stored whole.
//
// The sink must already be resolved — the drain goroutine cannot make progress
// on rows whose parent query record is still unknown while the caller holds it.
func (s *QuerySink) AddAll(ctx context.Context, rows []store.QueryRow) {
	if s == nil || s.writer == nil || len(rows) == 0 {
		return
	}

	w := s.writer

	for _, row := range rows {
		if w.stopped.Load() {
			s.dropped.Store(true)

			return
		}

		w.queuedBytes.Add(row.RowSizeBytes)

		select {
		case w.queue <- queueItem{sink: s, row: row}:
		case <-ctx.Done():
			w.queuedBytes.Add(-row.RowSizeBytes)
			s.dropped.Store(true)

			return
		case <-w.done:
			w.queuedBytes.Add(-row.RowSizeBytes)
			s.dropped.Store(true)

			return
		}
	}
}

// Flush blocks until every row submitted so far has been inserted (or
// definitively lost). It is the barrier a query needs before it is marked
// complete: without it the UI would show a finished query with rows still
// arriving.
//
// It is also where the capture's tamper-evident row chain is sealed, because
// the barrier is the first moment the chain head is final.
//
// Call it from the completion goroutine, never from the capture path.
func (s *QuerySink) Flush(ctx context.Context) {
	if s == nil || s.writer == nil {
		return
	}

	w := s.writer
	barrier := make(chan struct{})

	select {
	case w.queue <- queueItem{sink: s, barrier: barrier}:
	case <-ctx.Done():
		return
	case <-w.done:
		return
	}

	select {
	case <-barrier:
		s.seal(ctx)
	case <-ctx.Done():
	case <-w.done:
	}
}

// seal stamps the head of the capture's row chain, now that every row this
// sink submitted is durable. Sealing any earlier would record a head the
// capture then grows past; sealing on a sink whose parent record never
// materialized would stamp a query that does not exist.
//
// A capture that stored nothing costs no round trip — the store answers from
// its own head cache.
func (s *QuerySink) seal(ctx context.Context) {
	select {
	case <-s.ready:
	default:
		// Nothing resolved this sink, so it owns no query to stamp.
		return
	}

	queryUID, ok := s.resolution()
	if !ok {
		return
	}

	if err := s.writer.store.SealQueryRowChain(ctx, queryUID); err != nil {
		s.writer.logger.ErrorContext(ctx, "failed to seal the captured row chain",
			slog.String("query", queryUID.String()),
			slog.Any("error", err))
	}
}

// Dropped reports whether any row was lost — queue full, batch insert failed,
// or no parent query record. Read it after Flush so the answer covers the
// whole capture.
func (s *QuerySink) Dropped() bool {
	if s == nil {
		return false
	}

	return s.dropped.Load()
}

// QueryUID reports the uid of the query record this sink's rows hang from,
// blocking until it is known. It reports false when there is no sink, when the
// record could not be created, or when nothing resolved it in time — in which
// case the caller owns creating the record itself.
func (s *QuerySink) QueryUID(ctx context.Context) (uuid.UUID, bool) {
	if s == nil {
		return uuid.Nil, false
	}

	return s.await(ctx, time.Now().Add(sinkResolveTimeout))
}

// markDropped is how the drain goroutine reports a loss back to the capture.
func (s *QuerySink) markDropped() {
	if s == nil {
		return
	}

	s.dropped.Store(true)
}

// await blocks until the sink knows its parent query uid, reporting false if
// the record failed, the writer shut down, or deadline passed first.
//
// The already-resolved case — which is every row in a healthy process — takes
// the fast path and allocates nothing. A sink that misses the deadline is
// failed, so the answer is cached: no later row of that sink waits again.
func (s *QuerySink) await(ctx context.Context, deadline time.Time) (uuid.UUID, bool) {
	select {
	case <-s.ready:
		return s.resolution()
	default:
	}

	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()

	select {
	case <-s.ready:
		return s.resolution()
	case <-ctx.Done():
		return uuid.Nil, false
	case <-timer.C:
		// Nobody is going to resolve this. Fail it so the sink's remaining
		// rows — here and in later batches — are dropped immediately instead
		// of each paying the wait again.
		s.Fail()

		return uuid.Nil, false
	}
}

// resolution reads a sink that is known to be resolved (ready is closed).
func (s *QuerySink) resolution() (uuid.UUID, bool) {
	if s.failed.Load() {
		return uuid.Nil, false
	}

	return s.queryUID, true
}
