package oracle

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/proxy/shared"
	"github.com/fclairamb/dbbat/internal/store"
)

// captureTestMaxBytes is a byte ceiling high enough that only the row cap
// can ever trip in these tests.
const captureTestMaxBytes = int64(1) << 30

// captureRowStore records the batches the shared writer hands it.
type captureRowStore struct {
	mu      sync.Mutex
	batches [][]store.PendingQueryRow
}

func (c *captureRowStore) StoreQueryRows(_ context.Context, rows []store.PendingQueryRow) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	batch := make([]store.PendingQueryRow, len(rows))
	copy(batch, rows)
	c.batches = append(c.batches, batch)

	return nil
}

// rowNumbers flattens every batch, in insert order.
func (c *captureRowStore) rowNumbers() []int {
	c.mu.Lock()
	defer c.mu.Unlock()

	numbers := make([]int, 0)

	for _, batch := range c.batches {
		for _, row := range batch {
			numbers = append(numbers, row.RowNumber)
		}
	}

	return numbers
}

func (c *captureRowStore) batchCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return len(c.batches)
}

// newCapturingSession wires a session whose captured rows go to a fake store
// through the real shared writer, with the query record pretended to exist.
func newCapturingSession(t *testing.T, maxRows int) (*session, *captureRowStore, uuid.UUID) {
	t.Helper()

	rowStore := &captureRowStore{}
	writer := shared.NewRowWriter(rowStore, testLogger())

	t.Cleanup(func() { writer.Close(context.Background()) })

	s := newTestSessionWithStorage(&store.Grant{}, true, maxRows, captureTestMaxBytes)
	s.rowWriter = writer

	_ = s.handleOALL8(buildOALL8("SELECT id FROM emp", nil, 1))
	require.NotNil(t, s.tracker.pendingQuery)

	queryUID := uuid.New()
	s.tracker.pendingQuery.queryPersisted = true
	s.tracker.pendingQuery.queryUID = queryUID
	s.tracker.pendingQuery.rowSink = writer.NewSinkFor(queryUID)

	return s, rowStore, queryUID
}

// TestOracleCapture_BatchesRowsInsteadOfOneInsertPerRow is the point of the
// whole exercise: Oracle used to issue one synchronous INSERT per captured row.
func TestOracleCapture_BatchesRowsInsteadOfOneInsertPerRow(t *testing.T) {
	t.Parallel()

	const rows = 500

	s, rowStore, queryUID := newCapturingSession(t, 10000)

	cols := []columnDef{{Name: "ID", TypeCode: OracleTypeNUMBER}}

	for i := range rows {
		s.captureRow(cols, []interface{}{i})
	}

	s.tracker.pendingQuery.rowSink.Flush(context.Background())

	assert.Equal(t, rows, s.tracker.pendingQuery.rowNumber)
	assert.Len(t, rowStore.rowNumbers(), rows, "every captured row must be persisted")
	assert.Less(t, rowStore.batchCount(), rows, "rows must be batched, not inserted one at a time")

	for _, batch := range rowStore.batches {
		for _, row := range batch {
			assert.Equal(t, queryUID, row.QueryID)
		}
	}
}

// TestOracleCapture_RowNumbersStayContiguousAcrossFlushes covers bite #1: row
// numbers come from a producer-side running counter, so an early flush cannot
// renumber anything.
func TestOracleCapture_RowNumbersStayContiguousAcrossFlushes(t *testing.T) {
	t.Parallel()

	const rows = 2500 // more than two full batches

	s, rowStore, _ := newCapturingSession(t, 10000)

	cols := []columnDef{{Name: "ID", TypeCode: OracleTypeNUMBER}}

	for i := range rows {
		s.captureRow(cols, []interface{}{i})

		// Flush mid-capture so later rows land in a different batch.
		if i == 5 || i == 1500 {
			s.tracker.pendingQuery.rowSink.Flush(context.Background())
		}
	}

	s.tracker.pendingQuery.rowSink.Flush(context.Background())

	numbers := rowStore.rowNumbers()
	require.Len(t, numbers, rows)

	for i, n := range numbers {
		require.Equal(t, i+1, n, "row numbers must be contiguous and 1-based across flush boundaries")
	}

	assert.Greater(t, rowStore.batchCount(), 2, "the capture must actually have spanned several batches")
}

// TestOracleCapture_TruncationStillStopsCapture guards the behavior the
// batching must not disturb: hitting a storage limit keeps the prefix and flags
// the query.
func TestOracleCapture_TruncationStillStopsCapture(t *testing.T) {
	t.Parallel()

	const maxRows = 5

	s, rowStore, _ := newCapturingSession(t, maxRows)

	cols := []columnDef{{Name: "ID", TypeCode: OracleTypeNUMBER}}

	for i := range 50 {
		s.captureRow(cols, []interface{}{i})
	}

	s.tracker.pendingQuery.rowSink.Flush(context.Background())

	assert.True(t, s.tracker.pendingQuery.truncated)
	assert.Equal(t, maxRows, s.tracker.pendingQuery.rowNumber)
	assert.Len(t, rowStore.rowNumbers(), maxRows, "the prefix is kept, the rest is not captured")
	assert.False(t, s.tracker.pendingQuery.rowSink.Dropped(),
		"a configured limit is truncation, never a drop")
}

// TestOracleCapture_RowDataIsPreserved checks the payload survives the trip
// through the writer.
func TestOracleCapture_RowDataIsPreserved(t *testing.T) {
	t.Parallel()

	s, rowStore, _ := newCapturingSession(t, 100)

	cols := []columnDef{
		{Name: "ID", TypeCode: OracleTypeNUMBER},
		{Name: "NAME", TypeCode: OracleTypeVARCHAR2},
	}

	s.captureRow(cols, []interface{}{"1", "Alice"})
	s.tracker.pendingQuery.rowSink.Flush(context.Background())

	rowStore.mu.Lock()
	defer rowStore.mu.Unlock()

	require.Len(t, rowStore.batches, 1)
	require.Len(t, rowStore.batches[0], 1)

	var decoded map[string]interface{}
	require.NoError(t, json.Unmarshal(rowStore.batches[0][0].RowData, &decoded))
	assert.Equal(t, "1", decoded["ID"])
	assert.Equal(t, "Alice", decoded["NAME"])
}
