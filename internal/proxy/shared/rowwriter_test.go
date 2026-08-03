package shared

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/store"
)

// errFakeInsert is the canned failure returned by fakeRowStore.
var errFakeInsert = errors.New("fake insert failure")

// fakeRowStore records every batch it is handed, so tests can assert on batch
// boundaries and not just on the rows that landed.
type fakeRowStore struct {
	mu       sync.Mutex
	batches  [][]store.PendingQueryRow
	err      error
	blockOn  chan struct{} // when non-nil, every insert waits for it
	inserted chan struct{} // when non-nil, signaled after each insert
}

func (f *fakeRowStore) StoreQueryRows(_ context.Context, rows []store.PendingQueryRow) error {
	if f.blockOn != nil {
		<-f.blockOn
	}

	f.mu.Lock()
	batch := make([]store.PendingQueryRow, len(rows))
	copy(batch, rows)
	f.batches = append(f.batches, batch)
	err := f.err
	f.mu.Unlock()

	if f.inserted != nil {
		f.inserted <- struct{}{}
	}

	return err
}

func (f *fakeRowStore) snapshot() [][]store.PendingQueryRow {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make([][]store.PendingQueryRow, len(f.batches))
	copy(out, f.batches)

	return out
}

func (f *fakeRowStore) rowCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	n := 0
	for _, b := range f.batches {
		n += len(b)
	}

	return n
}

func testRow(n int, size int64) store.QueryRow {
	return store.QueryRow{
		RowNumber:    n,
		RowData:      json.RawMessage(`{"n":` + string(rune('0'+n%10)) + `}`),
		RowSizeBytes: size,
	}
}

func TestRowWriter_IdleProducerGetsBatchesOfOne(t *testing.T) {
	t.Parallel()

	fake := &fakeRowStore{inserted: make(chan struct{}, 8)}
	w := NewRowWriter(fake, slog.Default())

	sink := w.NewSinkFor(uuid.New())

	for i := 1; i <= 3; i++ {
		require.True(t, sink.Add(testRow(i, 10)))
		// Wait for the insert before queueing the next row: an idle producer
		// must yield one batch per row, not one batch of three.
		select {
		case <-fake.inserted:
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for the batch insert")
		}
	}

	w.Close(context.Background())

	batches := fake.snapshot()
	require.Len(t, batches, 3)

	for _, b := range batches {
		assert.Len(t, b, 1, "an idle producer must produce batches of one")
	}
}

func TestRowWriter_FastProducerGetsFullBatches(t *testing.T) {
	t.Parallel()

	// Hold the first insert so the queue builds up behind it, then let go:
	// the drain must sweep up everything queued in one batch.
	release := make(chan struct{})
	fake := &fakeRowStore{blockOn: release}
	w := NewRowWriter(fake, slog.Default())

	sink := w.NewSinkFor(uuid.New())

	const rows = 200

	accepted := 0

	for i := 1; i <= rows; i++ {
		if sink.Add(testRow(i, 10)) {
			accepted++
		}
	}

	require.Equal(t, rows, accepted, "queue is far larger than this burst")

	close(release)
	w.Close(context.Background())

	batches := fake.snapshot()
	require.NotEmpty(t, batches)
	assert.Less(t, len(batches), rows, "a fast producer must not yield one batch per row")
	assert.Equal(t, rows, fake.rowCount())
}

func TestRowWriter_RowCapEndsTheBatch(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	fake := &fakeRowStore{blockOn: release}
	w := NewRowWriter(fake, slog.Default())

	sink := w.NewSinkFor(uuid.New())

	// One row more than two full batches, all queued before anything drains.
	total := MaxBatchRows + 10
	for i := 1; i <= total; i++ {
		require.True(t, sink.Add(testRow(i, 1)))
	}

	close(release)
	w.Close(context.Background())

	batches := fake.snapshot()
	require.GreaterOrEqual(t, len(batches), 2)

	for _, b := range batches {
		assert.LessOrEqual(t, len(b), MaxBatchRows, "no batch may exceed the row cap")
	}

	assert.Equal(t, total, fake.rowCount())
}

func TestRowWriter_ByteCapEndsTheBatch(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	fake := &fakeRowStore{blockOn: release}
	w := NewRowWriter(fake, slog.Default())

	sink := w.NewSinkFor(uuid.New())

	// Rows well under the row cap in count, but past the byte budget: five
	// rows of 2 MiB trip MaxBatchBytes (8 MiB).
	const (
		rowSize = 2 << 20
		rows    = 5
	)

	for i := 1; i <= rows; i++ {
		require.True(t, sink.Add(testRow(i, rowSize)))
	}

	close(release)
	w.Close(context.Background())

	batches := fake.snapshot()
	require.GreaterOrEqual(t, len(batches), 2, "the byte budget must split the batch even far below the row cap")

	for _, b := range batches {
		var bytes int64
		for _, row := range b {
			bytes += row.RowSizeBytes
		}

		assert.LessOrEqual(t, bytes-b[len(b)-1].RowSizeBytes, int64(MaxBatchBytes),
			"a batch may only cross the byte budget with its final row")
	}

	assert.Equal(t, rows, fake.rowCount())
}

func TestRowWriter_DropsWhenQueueIsFull(t *testing.T) {
	t.Parallel()

	// The insert never returns, so the queue fills and stays full.
	release := make(chan struct{})
	defer close(release)

	fake := &fakeRowStore{blockOn: release}
	w := NewRowWriter(fake, slog.Default())

	sink := w.NewSinkFor(uuid.New())

	dropped := false

	for i := 1; i <= QueueCapacityRows+MaxBatchRows+100; i++ {
		if !sink.Add(testRow(i, 1)) {
			dropped = true

			break
		}
	}

	require.True(t, dropped, "a full queue must refuse rows instead of blocking")
	assert.True(t, sink.Dropped(), "a refused row must mark the capture as having dropped rows")
}

func TestRowWriter_DropsWhenByteBudgetIsExhausted(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	defer close(release)

	fake := &fakeRowStore{blockOn: release}
	w := NewRowWriter(fake, slog.Default())

	sink := w.NewSinkFor(uuid.New())

	// Wide rows: the byte budget trips long before the row cap would.
	const rowSize = 4 << 20

	accepted := 0

	for i := 1; i <= 64; i++ {
		if !sink.Add(testRow(i, rowSize)) {
			break
		}

		accepted++
	}

	assert.Less(t, accepted, QueueCapacityRows, "the byte budget must bound the queue, not just the row count")
	assert.True(t, sink.Dropped())
}

func TestRowWriter_FlushIsABarrier(t *testing.T) {
	t.Parallel()

	fake := &fakeRowStore{}
	w := NewRowWriter(fake, slog.Default())

	defer w.Close(context.Background())

	sink := w.NewSinkFor(uuid.New())

	const rows = 250
	for i := 1; i <= rows; i++ {
		require.True(t, sink.Add(testRow(i, 8)))
	}

	sink.Flush(context.Background())

	assert.Equal(t, rows, fake.rowCount(), "every row submitted before the barrier must have landed")
}

func TestRowWriter_RowsWaitForTheParentQueryRecord(t *testing.T) {
	t.Parallel()

	fake := &fakeRowStore{}
	w := NewRowWriter(fake, slog.Default())

	defer w.Close(context.Background())

	// Unresolved sink: the query record does not exist yet.
	sink := w.NewSink()

	for i := 1; i <= 5; i++ {
		require.True(t, sink.Add(testRow(i, 8)))
	}

	// Give the drain goroutine every chance to insert prematurely.
	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, 0, fake.rowCount(), "no row may be inserted before its parent query exists")

	queryUID := uuid.New()
	sink.Resolve(queryUID)
	sink.Flush(context.Background())

	assert.Equal(t, 5, fake.rowCount())

	for _, batch := range fake.snapshot() {
		for _, row := range batch {
			assert.Equal(t, queryUID, row.QueryID)
		}
	}
}

func TestRowWriter_FailedParentDropsRows(t *testing.T) {
	t.Parallel()

	fake := &fakeRowStore{}
	w := NewRowWriter(fake, slog.Default())

	defer w.Close(context.Background())

	sink := w.NewSink()

	require.True(t, sink.Add(testRow(1, 8)))

	sink.Fail()
	sink.Flush(context.Background())

	assert.Equal(t, 0, fake.rowCount())
	assert.True(t, sink.Dropped(), "rows with no parent query are dropped, not silently forgotten")
}

func TestRowWriter_FailedInsertMarksTheCapturePartial(t *testing.T) {
	t.Parallel()

	fake := &fakeRowStore{err: errFakeInsert}
	w := NewRowWriter(fake, slog.Default())

	defer w.Close(context.Background())

	sink := w.NewSinkFor(uuid.New())

	require.True(t, sink.Add(testRow(1, 8)))
	sink.Flush(context.Background())

	assert.True(t, sink.Dropped(), "a failed batch loses rows, so the query must read as partial")
}

func TestRowWriter_BatchesSpanQueries(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	fake := &fakeRowStore{blockOn: release}
	w := NewRowWriter(fake, slog.Default())

	first := uuid.New()
	second := uuid.New()
	sinkA := w.NewSinkFor(first)
	sinkB := w.NewSinkFor(second)

	for i := 1; i <= 50; i++ {
		require.True(t, sinkA.Add(testRow(i, 4)))
		require.True(t, sinkB.Add(testRow(i, 4)))
	}

	close(release)
	w.Close(context.Background())

	mixed := false

	for _, batch := range fake.snapshot() {
		seen := map[uuid.UUID]bool{}
		for _, row := range batch {
			seen[row.QueryID] = true
		}

		if len(seen) > 1 {
			mixed = true
		}
	}

	assert.True(t, mixed, "one process-wide writer must batch rows across concurrent queries")
	assert.Equal(t, 100, fake.rowCount())
}

func TestRowWriter_AddAllQueuesEveryRow(t *testing.T) {
	t.Parallel()

	fake := &fakeRowStore{}
	w := NewRowWriter(fake, slog.Default())

	defer w.Close(context.Background())

	sink := w.NewSinkFor(uuid.New())

	rows := make([]store.QueryRow, 0, QueueCapacityRows*2)
	for i := 1; i <= QueueCapacityRows*2; i++ {
		rows = append(rows, testRow(i, 4))
	}

	sink.AddAll(context.Background(), rows)
	sink.Flush(context.Background())

	assert.Equal(t, len(rows), fake.rowCount(), "a materialized result set must never be dropped")
	assert.False(t, sink.Dropped())
}

func TestRowWriter_NilWriterIsInert(t *testing.T) {
	t.Parallel()

	var w *RowWriter

	sink := w.NewSink()

	assert.False(t, sink.Add(testRow(1, 1)))
	assert.False(t, sink.Dropped())

	sink.Resolve(uuid.New())
	sink.AddAll(context.Background(), []store.QueryRow{testRow(1, 1)})
	sink.Flush(context.Background())
	w.Close(context.Background())

	assert.Nil(t, NewRowWriter(nil, slog.Default()))
}

func TestRowWriter_CloseDrainsWhatIsQueued(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	fake := &fakeRowStore{blockOn: release}
	w := NewRowWriter(fake, slog.Default())

	sink := w.NewSinkFor(uuid.New())

	for i := 1; i <= 100; i++ {
		require.True(t, sink.Add(testRow(i, 4)))
	}

	close(release)
	w.Close(context.Background())

	assert.Equal(t, 100, fake.rowCount(), "shutdown must not throw away what is already queued")
	assert.False(t, sink.Add(testRow(101, 4)), "a closed writer accepts nothing")
	assert.True(t, sink.Dropped())
}
