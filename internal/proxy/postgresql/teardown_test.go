package postgresql

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/store"
)

// hangUp plays a client that closes its socket, then runs the session's real
// teardown exactly as proxyMessages does once the first leg has ended.
func (h *loopback) hangUp(t *testing.T) {
	t.Helper()

	require.NoError(t, h.clientEnd.Close())

	var first error

	select {
	case first = <-h.relayErrs:
	case <-time.After(5 * time.Second):
		t.Fatal("no relay leg ended after the client hung up")
	}

	h.s.finishRelays(first, h.relayErrs)

	// cleanup does this before closing the connection row.
	require.True(t, h.s.persists.wait(10*time.Second), "the teardown's records never landed")
}

// connectionRow reads the session's connection row back from the store.
func (h *loopback) connectionRow(t *testing.T) *store.Connection {
	t.Helper()

	conn, err := h.s.store.GetConnectionByUID(context.Background(), h.s.connectionUID)
	require.NoError(t, err)

	return conn
}

func (h *loopback) wireTotal() int64 {
	return h.s.bytesFromClient.Load() + h.s.bytesToClient.Load()
}

// TestLoopback_ClientHangupMidResult_CompletesQueryAndFlushesBytes pins the
// fix: a client that reads part of a result and closes its socket used to
// leave the statement reading as still running and its streamed bytes out of
// the store — so a reconnect recomputed the grant's byte budget without them,
// and the cap could be exported any number of times over.
func TestLoopback_ClientHangupMidResult_CompletesQueryAndFlushesBytes(t *testing.T) {
	t.Parallel()

	h := newLoopback(t)

	const sql = "SELECT payload FROM big_table"

	h.sendForwarded(t, &pgproto3.Query{String: sql})

	h.answer(t, oneColumn("payload"))

	for range 20 {
		h.answer(t, textRow("0123456789abcdef0123456789abcdef"))
	}

	h.hangUp(t)

	rows := h.awaitConnectionQueries(t, 1)
	require.NotNil(t, rows[0].Error, "the cut-off statement must be completed with an error")
	assert.Equal(t, errClientDisconnectedMidResponse, *rows[0].Error)

	total := h.wireTotal()
	require.Positive(t, total)

	conn := h.connectionRow(t)
	assert.Equal(t, total, conn.BytesTransferred,
		"every byte streamed before the hangup must reach the connection row")
	assert.Equal(t, int64(1), conn.Queries)
	assert.Equal(t, total, h.s.grant.BytesTransferred)

	assert.Nil(t, h.s.currentQuery, "the teardown leaves nothing in flight")
	assert.False(t, h.s.statementClock.Running())
}

// TestLoopback_ClientHangupMidExecute_CompletesEveryPending is the extended
// protocol's shape of the same hangup: the Execute being answered and the one
// queued behind it both complete, and the streamed bytes go to the first.
func TestLoopback_ClientHangupMidExecute_CompletesEveryPending(t *testing.T) {
	t.Parallel()

	h := newLoopback(t)

	h.queueExecute(t, "s1", "SELECT a FROM t1")
	h.queueExecute(t, "s2", "SELECT b FROM t2")
	h.sendForwarded(t, &pgproto3.Sync{})
	h.awaitPending(t, 2)

	h.answer(t, &pgproto3.ParseComplete{}, &pgproto3.BindComplete{}, oneColumn("a"))

	for range 10 {
		h.answer(t, textRow("streamed-before-the-hangup"))
	}

	h.hangUp(t)

	rows := h.awaitConnectionQueries(t, 2)
	for _, sql := range []string{"SELECT a FROM t1", "SELECT b FROM t2"} {
		row := findRow(t, rows, sql)
		require.NotNilf(t, row.Error, "%q must be completed with the disconnect error", sql)
		assert.Equal(t, errClientDisconnectedMidResponse, *row.Error)
	}

	conn := h.connectionRow(t)
	assert.Equal(t, h.wireTotal(), conn.BytesTransferred)
	assert.Empty(t, h.s.extendedState.pendingQueries)
}

// TestLoopback_CleanTerminate_NoExtraRowsOrBytes is the positive control: a
// session whose statements all completed before it ended with Terminate gains
// no error row and no extra query, and its byte count is the wire total — the
// trailing ReadyForQuery and Terminate frames, not a second copy of anything
// already attributed.
func TestLoopback_CleanTerminate_NoExtraRowsOrBytes(t *testing.T) {
	t.Parallel()

	h := newLoopback(t)

	const sql = "SELECT 1"

	h.sendForwarded(t, &pgproto3.Query{String: sql})
	h.answer(t, oneColumn("one"), textRow("1"),
		&pgproto3.CommandComplete{CommandTag: []byte("SELECT 1")}, &pgproto3.ReadyForQuery{TxStatus: 'I'})

	rows := h.awaitConnectionQueries(t, 1)
	require.Nil(t, rows[0].Error)

	h.s.bookMu.Lock()
	attributed := h.s.lastBytesSnapshot
	h.s.bookMu.Unlock()

	h.sendForwarded(t, &pgproto3.Terminate{})
	h.hangUp(t)

	rows = h.awaitConnectionQueries(t, 1)
	assert.Nil(t, rows[0].Error, "a completed statement is not re-completed by the teardown")

	total := h.wireTotal()
	conn := h.connectionRow(t)
	assert.Equal(t, int64(1), conn.Queries, "the trailing flush is bytes only, never a query")
	assert.Equal(t, total, conn.BytesTransferred, "no byte is counted twice")
	assert.Equal(t, int64(len("Z\x00\x00\x00\x05I")+len("X\x00\x00\x00\x04")), total-attributed,
		"the flush carries only the trailing ReadyForQuery and Terminate frames")
}
