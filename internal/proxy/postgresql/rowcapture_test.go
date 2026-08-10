package postgresql

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/proxy/shared"
	"github.com/fclairamb/dbbat/internal/store"
)

// errAbortedForTest stands in for the mid-stream cause of an abort.
var errAbortedForTest = errors.New("grant expired")

// captureRowStore records the batches the shared writer hands it.
type captureRowStore struct {
	mu      sync.Mutex
	batches [][]store.PendingQueryRow
	sealed  []uuid.UUID
}

func (c *captureRowStore) StoreQueryRows(_ context.Context, rows []store.PendingQueryRow) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	batch := make([]store.PendingQueryRow, len(rows))
	copy(batch, rows)
	c.batches = append(c.batches, batch)

	return nil
}

// SealQueryRowChain is the row-chain stamp the real store writes at the flush
// barrier; these tests only care that the writer calls it.
func (c *captureRowStore) SealQueryRowChain(_ context.Context, queryUID uuid.UUID) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.sealed = append(c.sealed, queryUID)

	return nil
}

func (c *captureRowStore) rows() []store.PendingQueryRow {
	c.mu.Lock()
	defer c.mu.Unlock()

	out := make([]store.PendingQueryRow, 0)
	for _, batch := range c.batches {
		out = append(out, batch...)
	}

	return out
}

func (c *captureRowStore) batchCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return len(c.batches)
}

// newCapturingSession returns a session whose captured rows travel through the
// real shared writer into a fake store, with the parent query record already
// in place.
func newCapturingSession(t *testing.T, maxRows int, maxBytes int64) (*Session, *captureRowStore, uuid.UUID) {
	t.Helper()

	rowStore := &captureRowStore{}
	writer := shared.NewRowWriter(rowStore, slog.Default())

	t.Cleanup(func() { writer.Close(context.Background()) })

	s := newTestSession("write")
	s.rowWriter = writer
	s.queryStorage.StoreResults = true
	s.queryStorage.MaxResultRows = maxRows
	s.queryStorage.MaxResultBytes = maxBytes

	require.NoError(t, s.handleQuery(&pgproto3.Query{String: "SELECT id FROM big_table"}))

	s.captureRowDescription(&pgproto3.RowDescription{
		Fields: []pgproto3.FieldDescription{{Name: []byte("id"), DataTypeOID: 23}},
	})

	queryUID := uuid.New()
	s.currentQuery.rowSink = writer.NewSinkFor(queryUID)

	return s, rowStore, queryUID
}

func captureRows(s *Session, n int) {
	for i := range n {
		s.captureDataRow(&pgproto3.DataRow{Values: [][]byte{[]byte(strconv.Itoa(i))}})
	}
}

// TestPGCapture_StreamsRowsInsteadOfHoldingThemUntilQueryEnd is the point of
// the change: rows leave the session as they arrive, so peak memory is the
// writer's bounded queue rather than max_result_bytes per in-flight query.
func TestPGCapture_StreamsRowsInsteadOfHoldingThemUntilQueryEnd(t *testing.T) {
	t.Parallel()

	const rows = 2500

	s, rowStore, queryUID := newCapturingSession(t, 100000, 1<<30)

	captureRows(s, rows)
	s.currentQuery.rowSink.Flush(context.Background())

	stored := rowStore.rows()
	require.Len(t, stored, rows)
	assert.Less(t, rowStore.batchCount(), rows, "rows must be batched")
	assert.Greater(t, rowStore.batchCount(), 1, "a long capture must flush before the query ends")

	for _, row := range stored {
		assert.Equal(t, queryUID, row.QueryID)
	}
}

// TestPGCapture_RowNumbersAreAProducerSideCounter covers bite #1: PostgreSQL
// used to number rows after the fact (capturedRows[i].RowNumber = i + 1), which
// stops working the moment a batch flushes mid-query.
func TestPGCapture_RowNumbersAreAProducerSideCounter(t *testing.T) {
	t.Parallel()

	const rows = 2500

	s, rowStore, _ := newCapturingSession(t, 100000, 1<<30)

	for i := range rows {
		s.captureDataRow(&pgproto3.DataRow{Values: [][]byte{[]byte(strconv.Itoa(i))}})

		if i == 3 || i == 1200 {
			s.currentQuery.rowSink.Flush(context.Background())
		}
	}

	s.currentQuery.rowSink.Flush(context.Background())

	stored := rowStore.rows()
	require.Len(t, stored, rows)

	for i, row := range stored {
		require.Equal(t, i+1, row.RowNumber, "row numbers must stay contiguous across flush boundaries")
	}

	assert.Greater(t, rowStore.batchCount(), 2, "the capture must actually have spanned several batches")
}

// TestPGCapture_TruncationKeepsThePrefixAndIsNotADrop guards the distinction
// the UI depends on: a configured limit is truncation, never a drop.
func TestPGCapture_TruncationKeepsThePrefixAndIsNotADrop(t *testing.T) {
	t.Parallel()

	const maxRows = 5

	s, rowStore, _ := newCapturingSession(t, maxRows, 1<<20)

	captureRows(s, 50)
	s.currentQuery.rowSink.Flush(context.Background())

	assert.True(t, s.currentQuery.truncated)
	assert.Equal(t, maxRows, s.currentQuery.rowNumber)
	assert.Len(t, rowStore.rows(), maxRows, "the captured prefix must be kept")
	assert.False(t, s.currentQuery.rowSink.Dropped(), "truncation is not a drop")
}

// TestPGCapture_RowPayloadSurvivesTheWriter checks the decoded row still
// carries its column values.
func TestPGCapture_RowPayloadSurvivesTheWriter(t *testing.T) {
	t.Parallel()

	s, rowStore, _ := newCapturingSession(t, 100, 1<<20)

	captureRows(s, 1)
	s.currentQuery.rowSink.Flush(context.Background())

	stored := rowStore.rows()
	require.Len(t, stored, 1)

	var decoded map[string]interface{}
	require.NoError(t, json.Unmarshal(stored[0].RowData, &decoded))
	assert.InDelta(t, float64(0), decoded["id"], 0.0001)
}

// TestPGCapture_NoStoreFailsTheSinkInsteadOfWedgingTheWriter covers the
// degenerate wiring (a session with a writer but no store): the sink must be
// failed, not left unresolved, or the process-wide writer would stall on it.
func TestPGCapture_NoStoreFailsTheSinkInsteadOfWedgingTheWriter(t *testing.T) {
	t.Parallel()

	rowStore := &captureRowStore{}
	writer := shared.NewRowWriter(rowStore, slog.Default())

	defer writer.Close(context.Background())

	s := newTestSession("write")
	s.rowWriter = writer
	s.queryStorage.StoreResults = true
	s.queryStorage.MaxResultRows = 100
	s.queryStorage.MaxResultBytes = 1 << 20

	require.NoError(t, s.handleQuery(&pgproto3.Query{String: "SELECT id FROM t"}))
	s.captureRowDescription(&pgproto3.RowDescription{
		Fields: []pgproto3.FieldDescription{{Name: []byte("id"), DataTypeOID: 23}},
	})

	captureRows(s, 3)

	sink := s.currentQuery.rowSink
	require.NotNil(t, sink)

	_, ok := sink.QueryUID(context.Background())
	assert.False(t, ok, "a sink with no query record must resolve as failed, not hang")

	sink.Flush(context.Background())
	assert.Empty(t, rowStore.rows(), "no row may be inserted without a parent query")
	assert.True(t, sink.Dropped(), "rows with no parent are dropped, and the query says so")
}

// TestPersistAbortedQuery_CompletesARecordItAlreadyCreated covers the seam the
// eager parent insert opened: capture creates the queries row on its first row,
// so an abort that attributes no new bytes must still log the outcome. Skipping
// it would leave a row with no duration, no error and no capture flags — a
// query that reads as still running.
func TestPersistAbortedQuery_CompletesARecordItAlreadyCreated(t *testing.T) {
	t.Parallel()

	rowStore := &captureRowStore{}
	writer := shared.NewRowWriter(rowStore, slog.Default())

	t.Cleanup(func() { writer.Close(context.Background()) })

	newAbortSession := func(withSink bool) *Session {
		s := newTestSession("write")
		s.ctx = context.Background()
		s.bytesFromClient = &atomic.Int64{}
		s.bytesToClient = &atomic.Int64{}
		s.currentQuery = &pendingQuery{sql: "SELECT 1", startTime: time.Now()}

		if withSink {
			s.currentQuery.rowSink = writer.NewSinkFor(uuid.New())
		}

		return s
	}

	// No new bytes and a capture already in flight: the abort must be logged.
	withRecord := newAbortSession(true)
	withRecord.persistAbortedQuery(errAbortedForTest)

	assert.Equal(t, int64(1), withRecord.grant.QueryCount,
		"an abort on a query whose record already exists must still be completed")

	// No new bytes and nothing captured: nothing was ever inserted, so there
	// is still nothing to log.
	withoutRecord := newAbortSession(false)
	withoutRecord.persistAbortedQuery(errAbortedForTest)

	assert.Equal(t, int64(0), withoutRecord.grant.QueryCount,
		"an abort with no record and no bytes stays unlogged")
}
