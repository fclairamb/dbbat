package postgresql

import (
	"context"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/proxy/shared"
	"github.com/fclairamb/dbbat/internal/safe"
	"github.com/fclairamb/dbbat/internal/store"
)

// capturingHandler collects every slog record, so a test can assert a WARN was
// (or was not) emitted by the bookkeeping paths.
type capturingHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *capturingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *capturingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.records = append(h.records, r)

	return nil
}

func (h *capturingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }

func (h *capturingHandler) WithGroup(string) slog.Handler { return h }

// warnings returns every WARN record, optionally filtered by message
// substring.
func (h *capturingHandler) warnings(substr string) []slog.Record {
	h.mu.Lock()
	defer h.mu.Unlock()

	var found []slog.Record

	for _, r := range h.records {
		if r.Level != slog.LevelWarn {
			continue
		}

		if substr == "" || strings.Contains(r.Message, substr) {
			found = append(found, r)
		}
	}

	return found
}

// loopback is the bookkeeping harness: a store-backed session whose two relay
// legs run over net.Pipe exactly as proxyMessages wires them, with the test
// playing both ends — the client (pgproto3 frames in) and the upstream server
// (client frames decoded, backend messages answered). Nothing is stubbed: the
// queueing, the epoch counting and the pop/reconcile bookkeeping all run on
// their real goroutines, which is what makes the timing assertions mean
// something.
type loopback struct {
	s *Session

	// client speaks client frames to the proxy's client leg and decodes what
	// the upstream leg forwarded back (backend frames).
	client *pgproto3.Frontend
	// upstream plays the upstream server over the upstream pipe: it receives
	// the frames the client leg forwarded (as frontend frames) and answers
	// with backend messages.
	upstream *pgproto3.Backend

	logs *capturingHandler
}

func newLoopback(t *testing.T) *loopback {
	t.Helper()

	dataStore := newCopyTestStore(t)

	clientProxyEnd, clientTestEnd := net.Pipe()
	upstreamProxyEnd, upstreamTestEnd := net.Pipe()

	var fromClient, toClient atomic.Int64

	countedClient := shared.NewCountingConn(clientProxyEnd, &fromClient, &toClient)

	logs := &capturingHandler{}

	s := newPersistingSession(t, dataStore, "book"+uuid.NewString()[:8])
	s.grant = &store.Grant{
		UID:        uuid.New(),
		ExpiresAt:  time.Now().Add(time.Hour),
		Definition: &store.GrantDefinition{},
	}
	s.clientConn = countedClient
	s.clientBackend = pgproto3.NewBackend(countedClient, countedClient)
	s.upstreamConn = upstreamProxyEnd
	s.upstreamFrontend = pgproto3.NewFrontend(upstreamProxyEnd, upstreamProxyEnd)
	s.bytesFromClient = &fromClient
	s.bytesToClient = &toClient
	s.guard = shared.NewLimitGuard(s.grant, &fromClient, &toClient)
	s.logger = slog.New(logs)

	// Result capture far above anything these scenarios stream, so the only
	// WARN in the captured log can be the reconcile backstop's.
	s.queryStorage.MaxResultRows = 10000
	s.queryStorage.MaxResultBytes = 10 << 20

	h := &loopback{
		s:        s,
		client:   pgproto3.NewFrontend(clientTestEnd, clientTestEnd),
		upstream: pgproto3.NewBackend(upstreamTestEnd, upstreamTestEnd),
		logs:     logs,
	}

	// Both legs, wired as proxyMessages wires them. Their exit errors are the
	// pipes closing, which pgproto3 surfaces as io.ErrUnexpectedEOF.
	go func() { _ = safe.RunRelay(s.ctx, s.logger, relayNameClientToUpstream, s.proxyClientToUpstream) }()
	go func() { _ = safe.RunRelay(s.ctx, s.logger, relayNameUpstreamToClient, s.proxyUpstreamToClient) }()

	t.Cleanup(func() {
		_ = clientTestEnd.Close()
		_ = clientProxyEnd.Close()
		_ = upstreamTestEnd.Close()
		_ = upstreamProxyEnd.Close()
	})

	return h
}

// sendClient sends client frames to the proxy and flushes.
func (h *loopback) sendClient(t *testing.T, msgs ...pgproto3.FrontendMessage) {
	t.Helper()

	for _, m := range msgs {
		h.client.Send(m)
	}

	require.NoError(t, h.client.Flush())
}

// recvUpstream receives one frame the client leg forwarded upstream.
func (h *loopback) recvUpstream(t *testing.T) pgproto3.FrontendMessage {
	t.Helper()

	msg, err := h.upstream.Receive()
	require.NoError(t, err)

	return msg
}

// sendUpstream answers the client leg like a backend would.
func (h *loopback) sendUpstream(t *testing.T, msgs ...pgproto3.BackendMessage) {
	t.Helper()

	for _, m := range msgs {
		h.upstream.Send(m)
	}

	require.NoError(t, h.upstream.Flush())
}

// recvClient receives one message the upstream leg forwarded to the client.
func (h *loopback) recvClient(t *testing.T) pgproto3.BackendMessage {
	t.Helper()

	msg, err := h.client.Receive()
	require.NoError(t, err)

	return msg
}

// awaitPending waits until the client leg has queued n pending queries — the
// seam between the client frames landing and the upstream answer being written.
func (h *loopback) awaitPending(t *testing.T, n int) {
	t.Helper()

	require.Eventually(t, func() bool {
		h.s.bookMu.Lock()
		defer h.s.bookMu.Unlock()

		return len(h.s.extendedState.pendingQueries) == n
	}, 5*time.Second, 2*time.Millisecond)
}

// awaitConnectionQueries polls until the connection has exactly `want` query
// rows. Persistence is asynchronous by design.
func (h *loopback) awaitConnectionQueries(t *testing.T, want int) []store.Query {
	t.Helper()

	ctx := context.Background()
	connUID := h.s.connectionUID

	require.Eventually(t, func() bool {
		queries, err := h.s.store.ListQueries(ctx, store.QueryFilter{ConnectionID: &connUID, Limit: 50})

		return err == nil && len(queries) == want
	}, 10*time.Second, 50*time.Millisecond, "expected %d persisted query row(s)", want)

	// A stray extra row must not show up later either.
	time.Sleep(250 * time.Millisecond)

	queries, err := h.s.store.ListQueries(ctx, store.QueryFilter{ConnectionID: &connUID, Limit: 50})
	require.NoError(t, err)
	require.Len(t, queries, want)

	return queries
}

// findRow returns the persisted row carrying the given statement text.
func findRow(t *testing.T, rows []store.Query, sql string) store.Query {
	t.Helper()

	for _, row := range rows {
		if row.SQLText == sql {
			return row
		}
	}

	t.Fatalf("no query row for %q among %d row(s)", sql, len(rows))

	return store.Query{}
}

func assertCompletedOwnDuration(t *testing.T, rows []store.Query, sql string) {
	t.Helper()

	row := findRow(t, rows, sql)
	assert.Nilf(t, row.Error, "%q must complete without an error", sql)
	assert.NotNilf(t, row.DurationMs, "%q must log its own duration", sql)
}

// sendForwarded sends one client frame and immediately drains the copy the
// client leg forwarded upstream. The pipe is unbuffered and the client leg
// blocks forwarding each frame until its copy is read, so a batch can only be
// pipelined by draining as it goes.
func (h *loopback) sendForwarded(t *testing.T, msg pgproto3.FrontendMessage) {
	t.Helper()

	h.sendClient(t, msg)
	h.recvUpstream(t)
}

// ownDurationSlackMs is the only slop allowed between a row's logged duration
// and the span driveStatement bracketed around that statement's own exchange.
// The span already contains whatever a loaded CI box charged this statement, so
// the slack exists purely for clock granularity: it must stay far below the
// pause every statement here sleeps before its terminator, which is the
// smallest amount a *later* statement can add to a duration whose terminator
// popped the wrong entry.
const ownDurationSlackMs = 10

// driveStatement runs one extended-protocol batch end to end: the client
// frames go in (each drained upstream as it is forwarded), the harness waits
// until the Execute is queued, the simulated upstream answers after `pause`
// (which is what makes the statement's own round trip `pause` long), and the
// forwarded replies are drained off the client socket.
//
// It returns the wall-clock span it bracketed, from the first client frame
// until the ReadyForQuery is drained off the client socket. The session stamps
// the row's duration strictly inside that span — startTime when the Execute is
// queued (intercept.go handleExecute), logQuery before the ReadyForQuery is
// forwarded to the client (session.go's upstream leg) — so the span is a
// per-statement budget for the row's own duration that cannot be broken by
// scheduler jitter: every millisecond a slow box charges this exchange is
// charged to the budget too. What it does not absorb is work the statement
// never did, i.e. the pause and relay of a *later* statement — which is
// exactly what a terminator popping the wrong pending entry records.
func (h *loopback) driveStatement(t *testing.T, sql string, maxRows uint32, term []pgproto3.BackendMessage, pause time.Duration) time.Duration {
	t.Helper()

	start := time.Now()

	h.sendForwarded(t, &pgproto3.Parse{Name: "", Query: sql})
	h.sendForwarded(t, &pgproto3.Bind{DestinationPortal: "", PreparedStatement: ""})
	h.sendForwarded(t, &pgproto3.Execute{Portal: "", MaxRows: maxRows})

	// handleExecute queues the entry before forwarding the Execute, so it is
	// here by the time the forwarded copy has been drained.
	h.awaitPending(t, 1)

	time.Sleep(pause)

	h.sendForwarded(t, &pgproto3.Sync{})

	h.answer(t, append([]pgproto3.BackendMessage{}, term...)...)
	h.answer(t, &pgproto3.ReadyForQuery{})

	return time.Since(start)
}

// answer writes the given backend messages to the simulated upstream one at a
// time, draining each one's forwarded copy off the client socket as it is
// forwarded. The pipes are unbuffered and every leg blocks on its peer's read,
// so the only deadlock-free pace is strictly interleaved.
func (h *loopback) answer(t *testing.T, msgs ...pgproto3.BackendMessage) {
	t.Helper()

	for _, m := range msgs {
		h.sendUpstream(t, m)
		h.recvClient(t)
	}
}

// TestLoopback_EmptyQueryResponse pins rule 1 for the empty statement: the
// production capture showed DataGrip sending the empty statement right after
// it set its application_name, and not popping it shifted every later pop one
// place back. The entry must pop, the row must complete at the Sync boundary
// with no rows affected and no error, and the clock must stop.
func TestLoopback_EmptyQueryResponse(t *testing.T) {
	t.Parallel()

	h := newLoopback(t)

	const sql = ""

	span := h.driveStatement(t, sql, 0, []pgproto3.BackendMessage{&pgproto3.EmptyQueryResponse{}}, 50*time.Millisecond)

	assert.Empty(t, h.s.extendedState.pendingQueries)
	assert.False(t, h.s.statementClock.Running(), "the backend is idle once the exchange closed")
	assert.Equal(t, uint64(1), h.s.clientSyncEpoch, "the forwarded Sync earned one epoch")

	rows := h.awaitConnectionQueries(t, 1)
	assert.Equal(t, sql, rows[0].SQLText)
	require.Nil(t, rows[0].Error, "EmptyQueryResponse is not an error")
	require.NotNil(t, rows[0].DurationMs, "the row completes at the Sync boundary")
	assert.LessOrEqualf(t, *rows[0].DurationMs, float64(span/time.Millisecond)+ownDurationSlackMs,
		"its own duration (%vms), not a shifted one", *rows[0].DurationMs)
	require.Nil(t, rows[0].RowsAffected, "EmptyQueryResponse carries no command tag")

	assert.Empty(t, h.logs.warnings(""),
		"rule 1 handles the normal flows: the reconcile backstop must not fire")
}

// TestLoopback_PortalSuspended pins rule 1 for the paged grid: an Execute with
// a row limit ends with PortalSuspended, not CommandComplete. The entry must
// pop, the backend must read as idle, and the row must log its own duration —
// DataGrip pages every result grid, so missing this parked every opened table
// on a stale entry.
func TestLoopback_PortalSuspended(t *testing.T) {
	t.Parallel()

	h := newLoopback(t)

	const sql = "SELECT t.* FROM public.widgets t LIMIT 501"

	span := h.driveStatement(t, sql, 501, []pgproto3.BackendMessage{
		&pgproto3.RowDescription{Fields: []pgproto3.FieldDescription{{Name: []byte("id")}}},
		&pgproto3.DataRow{Values: [][]byte{[]byte("1")}},
		&pgproto3.DataRow{Values: [][]byte{[]byte("2")}},
		&pgproto3.DataRow{Values: [][]byte{[]byte("3")}},
		&pgproto3.PortalSuspended{},
	}, 50*time.Millisecond)

	assert.Empty(t, h.s.extendedState.pendingQueries)
	assert.False(t, h.s.statementClock.Running(),
		"a suspended portal is idle, not executing — the clock must stop there")

	rows := h.awaitConnectionQueries(t, 1)
	assert.Equal(t, sql, rows[0].SQLText)
	require.NotNil(t, rows[0].DurationMs, "the row logs its own duration")
	assert.LessOrEqualf(t, *rows[0].DurationMs, float64(span/time.Millisecond)+ownDurationSlackMs,
		"its own duration (%vms), not a shifted one", *rows[0].DurationMs)
	require.Nil(t, rows[0].Error, "PortalSuspended is not an error")
	require.Nil(t, rows[0].RowsAffected, "PortalSuspended carries no command tag")

	assert.Empty(t, h.logs.warnings(""),
		"rule 1 handles the normal flows: the reconcile backstop must not fire")
}

// TestLoopback_DataGripCaptureSequence replays the production capture as one
// sequence: eleven statements over the extended protocol, including the empty
// statement and the 501-row paged grid. Every entry must pop on its own
// terminator, so every logged duration is the statement's own and the clock is
// clear at the end. Before the fix this sequence left two stale entries, never
// completed the last statements, and armed the watchdog on an idle backend —
// which is how an idle DataGrip session was killed a full hour later.
func TestLoopback_DataGripCaptureSequence(t *testing.T) {
	t.Parallel()

	h := newLoopback(t)

	const phase = 50 * time.Millisecond

	type scenario struct {
		name    string
		sql     string
		maxRows uint32
		term    []pgproto3.BackendMessage
	}

	scenarios := []scenario{
		{name: "application_name", sql: "SET application_name = 'IntelliJ IDEA 2026.2.3'", maxRows: 0, term: []pgproto3.BackendMessage{&pgproto3.CommandComplete{CommandTag: []byte("SET")}}},
		{name: "extra_float_digits", sql: "SET extra_float_digits = 3", maxRows: 0, term: []pgproto3.BackendMessage{&pgproto3.CommandComplete{CommandTag: []byte("SET")}}},
		{name: "version", sql: "select version()", maxRows: 0, term: []pgproto3.BackendMessage{&pgproto3.CommandComplete{CommandTag: []byte("SELECT 1")}}},
		{name: "catalog probe", sql: "SELECT n.nspname FROM pg_catalog.pg_class c", maxRows: 0, term: []pgproto3.BackendMessage{&pgproto3.CommandComplete{CommandTag: []byte("SELECT 3")}}},
		// The empty statement, replied by the fourth terminator.
		{name: "empty statement", sql: "", maxRows: 0, term: []pgproto3.BackendMessage{&pgproto3.EmptyQueryResponse{}}},
		{name: "current_database", sql: "select current_database(), current_user", maxRows: 0, term: []pgproto3.BackendMessage{&pgproto3.CommandComplete{CommandTag: []byte("SELECT 1")}}},
		// The paged grid: 501 rows, then PortalSuspended.
		{name: "paged grid", sql: "SELECT t.* FROM public.big_table t LIMIT 501", maxRows: 501, term: pagedGridTerminator(501)},
		{name: "type lookup", sql: "SELECT n.nspname, t.typname FROM pg_type t", maxRows: 0, term: []pgproto3.BackendMessage{&pgproto3.CommandComplete{CommandTag: []byte("SELECT 12")}}},
		{name: "isolation level", sql: "SHOW TRANSACTION ISOLATION LEVEL", maxRows: 0, term: []pgproto3.BackendMessage{&pgproto3.CommandComplete{CommandTag: []byte("SHOW")}}},
	}

	// One span per statement, each bracketed around that statement's own
	// exchange alone.
	spans := make(map[string]time.Duration, len(scenarios))

	for _, sc := range scenarios {
		spans[sc.sql] = h.driveStatement(t, sc.sql, sc.maxRows, sc.term, phase)
	}

	assert.Empty(t, h.s.extendedState.pendingQueries, "no stale entry may survive the capture sequence")
	assert.Equal(t, uint64(9), h.s.clientSyncEpoch)
	assert.False(t, h.s.statementClock.Running(),
		"the idle session must not leave the statement clock armed — that is what killed it an hour later")

	rows := h.awaitConnectionQueries(t, len(scenarios))

	for _, sc := range scenarios {
		row := findRow(t, rows, sc.sql)
		require.NotNilf(t, row.DurationMs, "%q must log a duration", sc.name)

		// Own duration: it must fit inside the span the harness bracketed
		// around this statement's own batch — a budget that carries whatever a
		// loaded CI box charged the exchange, so scheduler jitter cannot break
		// it. The earlier absolute thresholds could: the 501-row grid relayed
		// its page through unbuffered pipes for longer than a 500ms budget on
		// a busy runner (measured 549ms), with nothing shifted. A shifted one —
		// the time to a later statement's completion, as the queries page
		// logged during the incident — can never fit, because it also carries
		// at least one later statement's full `phase` pause plus its relay,
		// work that happens outside this span.
		assert.LessOrEqualf(t, *row.DurationMs,
			float64(spans[sc.sql]/time.Millisecond)+ownDurationSlackMs,
			"%q logged a shifted duration (%vms): its terminator popped the wrong entry", sc.name, *row.DurationMs)

		assert.Nilf(t, row.Error, "%q must complete without an error", sc.name)
	}

	assert.Empty(t, h.logs.warnings(""),
		"the capture sequence is all normal flows: the reconcile backstop must not fire")
}

// pagedGridTerminator is a page of rows plus the PortalSuspended.
func pagedGridTerminator(n int) []pgproto3.BackendMessage {
	msgs := []pgproto3.BackendMessage{
		&pgproto3.RowDescription{Fields: []pgproto3.FieldDescription{{Name: []byte("id")}}},
	}
	for i := 0; i < n; i++ {
		msgs = append(msgs, &pgproto3.DataRow{Values: [][]byte{[]byte("0123456789ABCDEF")}})
	}
	msgs = append(msgs, &pgproto3.PortalSuspended{})

	return msgs
}

// TestLoopback_BackstopReconcile pins rule 2: an upstream ErrorResponse
// mid-batch makes the server discard the rest of the batch, so a second
// Execute the client had already sent gets no terminator. Its entry must be
// finished at the ReadyForQuery — with the backstop's error text — and
// dropped, with one WARN naming it.
func TestLoopback_BackstopReconcile(t *testing.T) {
	t.Parallel()

	h := newLoopback(t)

	const (
		first  = "SELECT 1"
		second = "SELECT 2"
	)

	// One batch, two Executes queued before its single Sync.
	h.sendForwarded(t, &pgproto3.Parse{Name: "st1", Query: first})
	h.sendForwarded(t, &pgproto3.Bind{DestinationPortal: "p1", PreparedStatement: "st1"})
	h.sendForwarded(t, &pgproto3.Execute{Portal: "p1"})
	h.sendForwarded(t, &pgproto3.Parse{Name: "st2", Query: second})
	h.sendForwarded(t, &pgproto3.Bind{DestinationPortal: "p2", PreparedStatement: "st2"})
	h.sendForwarded(t, &pgproto3.Execute{Portal: "p2"})

	h.awaitPending(t, 2)

	h.sendForwarded(t, &pgproto3.Sync{})
	h.awaitSyncEpoch(t, 1)

	// The server answers the first Execute and discards the rest of the batch.
	h.answer(t,
		&pgproto3.ErrorResponse{Severity: "ERROR", Code: "42601", Message: "syntax error"},
		&pgproto3.ReadyForQuery{},
	)

	assert.Empty(t, h.s.extendedState.pendingQueries, "the discarded Execute must not stay queued")
	assert.False(t, h.s.statementClock.Running(), "nothing is executing upstream once the batch closed")

	rows := h.awaitConnectionQueries(t, 2)

	failed := findRow(t, rows, first)
	require.NotNil(t, failed.Error, "the ErrorResponse must complete the first Execute")
	assert.Equal(t, "syntax error", *failed.Error)

	backstopped := findRow(t, rows, second)
	require.NotNil(t, backstopped.Error, "the discarded Execute must be finished at the RFQ")
	assert.Equal(t, "no completion message from upstream", *backstopped.Error)
	require.NotNil(t, backstopped.DurationMs, "its end is the ReadyForQuery's time")

	warns := h.logs.warnings("terminator never completed")
	require.Len(t, warns, 1, "exactly one WARN for the backstopped statement")
	require.Len(t, h.logs.warnings(""), 1)
}

// TestLoopback_PipelinedBatchSurvives pins the counting semantics that keep
// the backstop from over-firing: a pipelined client sends batch 2's
// Parse/Bind/Execute/Sync before batch 1's ReadyForQuery arrives. Batch 2's
// entries carry the higher epoch and must survive the first RFQ, which is why
// the reconcile counts rather than drains.
func TestLoopback_PipelinedBatchSurvives(t *testing.T) {
	t.Parallel()

	h := newLoopback(t)

	const (
		first  = "SELECT 1"
		second = "SELECT 2"
	)

	// Both batches, back to back, before any response is read.
	h.sendForwarded(t, &pgproto3.Parse{Name: "st1", Query: first})
	h.sendForwarded(t, &pgproto3.Bind{DestinationPortal: "p1", PreparedStatement: "st1"})
	h.sendForwarded(t, &pgproto3.Execute{Portal: "p1"})
	h.sendForwarded(t, &pgproto3.Sync{})

	h.sendForwarded(t, &pgproto3.Parse{Name: "st2", Query: second})
	h.sendForwarded(t, &pgproto3.Bind{DestinationPortal: "p2", PreparedStatement: "st2"})
	h.sendForwarded(t, &pgproto3.Execute{Portal: "p2"})
	h.sendForwarded(t, &pgproto3.Sync{})

	h.awaitPending(t, 2)
	h.awaitSyncEpoch(t, 2)

	// The upstream answers batch 1, then batch 2.
	h.answer(t,
		&pgproto3.CommandComplete{CommandTag: []byte("SELECT 1")},
		&pgproto3.ReadyForQuery{},
		&pgproto3.CommandComplete{CommandTag: []byte("SELECT 2")},
		&pgproto3.ReadyForQuery{},
	)

	assert.Empty(t, h.s.extendedState.pendingQueries)
	assert.False(t, h.s.statementClock.Running())

	rows := h.awaitConnectionQueries(t, 2)
	assertCompletedOwnDuration(t, rows, first)
	assertCompletedOwnDuration(t, rows, second)

	assert.Empty(t, h.logs.warnings(""),
		"a pipelined batch must survive the first RFQ: the backstop counts, it does not drain")
}

// awaitSyncEpoch waits until the client leg has counted n earned messages.
func (h *loopback) awaitSyncEpoch(t *testing.T, n uint64) {
	t.Helper()

	require.Eventually(t, func() bool {
		h.s.bookMu.Lock()
		defer h.s.bookMu.Unlock()

		return h.s.clientSyncEpoch == n
	}, 5*time.Second, 2*time.Millisecond)
}

// TestSession_TerminationNamesOldestInFlight pins rule 3: the watchdog
// measures the *oldest* pending entry, so the termination record and the rows
// the teardown completes must point at it — not at the newest, which is what
// the production incident blamed. The other pending entries complete as
// aborted, without the limit text, so nothing reads as still running.
func TestSession_TerminationNamesOldestInFlight(t *testing.T) {
	t.Parallel()

	h := newLoopback(t)
	s := h.s

	// A real statement-timeout watchdog: one-hour limit, clock armed two hours
	// ago on the oldest entry.
	s.guard = s.guard.WithStatementTimeout(time.Hour, shared.StatementTimeoutGrace, &s.statementClock)
	s.statementClock.StartAt(time.Now().Add(-2 * time.Hour))

	// The incident's shape: the typinput lookup queued first, the SHOW queued
	// later, neither ever popped. Both already have persisted rows (a started
	// result capture does that), which is what lets the termination record and
	// the teardown point at uids.
	const (
		oldestSQL = "SELECT typname, typinput FROM pg_type t"
		newestSQL = "SHOW TRANSACTION ISOLATION LEVEL"
	)

	oldUID := insertRow(t, s, oldestSQL, -2*time.Hour)
	newUID := insertRow(t, s, newestSQL, -time.Second)

	s.extendedState.pendingQueries = []*pendingQuery{
		{sql: oldestSQL, startTime: time.Now().Add(-2 * time.Hour), approvalUID: oldUID},
		{sql: newestSQL, startTime: time.Now().Add(-time.Second), approvalUID: newUID},
	}

	// The termination record must name the oldest entry — this is what the
	// audit entry and the Slack payload are built from.
	s.noteTermination(shared.ErrStatementTimeout)

	tm := s.recordedTermination()
	require.Equal(t, store.TerminationStatementTimeout, tm.Reason)
	assert.Equal(t, oldUID, tm.QueryUID, "the termination must name the oldest in-flight statement, not the newest")

	// And the teardown must complete every in-flight entry: the oldest with
	// the limit text, the others as aborted.
	s.persistTerminatedQuery(s.recordedTermination())

	rows := h.awaitConnectionQueries(t, 2)

	oldest := findRow(t, rows, oldestSQL)
	require.NotNil(t, oldest.Error, "the measured statement must be completed with the limit text")
	assert.Contains(t, *oldest.Error, "statement timeout")
	assert.Contains(t, *oldest.Error, "terminated by dbbat")

	aborted := findRow(t, rows, newestSQL)
	require.NotNil(t, aborted.Error, "the other in-flight statement must be completed too")
	assert.NotContains(t, *aborted.Error, "statement timeout", "the aborted entry carries no limit text")

	assert.Empty(t, s.extendedState.pendingQueries, "the teardown leaves no queued row behind")
	assert.False(t, s.statementClock.Running())
}

// insertRow inserts a bare queries row, the way a started result capture does
// before its query ever completes, and returns its uid.
func insertRow(t *testing.T, s *Session, sql string, age time.Duration) uuid.UUID {
	t.Helper()

	created, err := s.store.CreateQuery(context.Background(), &store.Query{
		ConnectionID: s.connectionUID,
		SQLText:      sql,
		ExecutedAt:   time.Now().Add(age),
	})
	require.NoError(t, err)

	return created.UID
}
