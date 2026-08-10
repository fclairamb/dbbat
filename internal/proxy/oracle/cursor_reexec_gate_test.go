package oracle

import (
	"net"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/approval"
	"github.com/fclairamb/dbbat/internal/proxy/shared"
	"github.com/fclairamb/dbbat/internal/store"
)

// buildOALL8Reexec builds the frame this whole file is about: a well-formed
// OALL8 naming a cursor, with a zero-length SQL field — a re-execution in the
// legacy (pre-v315) framing, where the statement text is never resent, only the
// cursor id.
//
// Hand-built on purpose, and it stays that way: captures from go-ora,
// python-oracledb thin, JDBC thin and sqlplus show that no modern client emits
// this shape — they all use the piggyback re-execution instead, which
// cursor_reexec_replay_test.go covers from real recordings. This file keeps the
// legacy shape honest for the older clients that would still send it.
func buildOALL8Reexec(cursorID uint16) []byte {
	return buildOALL8("", nil, cursorID)
}

// TestBuildOALL8ReexecIsDecodedAsARexecution guards the fixture itself: if the
// builder stopped producing something decodeOALL8 reports as "no SQL, cursor
// N", every enforcement test below would pass vacuously — the handler would be
// refusing a frame for the wrong reason, or not seeing it at all.
func TestBuildOALL8ReexecIsDecodedAsARexecution(t *testing.T) {
	t.Parallel()

	_, err := decodeOALL8(buildOALL8Reexec(7))
	require.ErrorIs(t, err, ErrOALL8NoSQL, "the fixture must decode as a re-execution, not as a decode failure")

	var noSQL *OALL8NoSQLError

	require.ErrorAs(t, err, &noSQL)
	assert.Equal(t, uint16(7), noSQL.CursorID, "the cursor id must survive into the error")

	// And the ordinary SQL-carrying frame is still decoded as such, so the
	// two paths below are genuinely different frames.
	decoded, err := decodeOALL8(buildOALL8("SELECT 1 FROM DUAL", nil, 7))
	require.NoError(t, err)
	assert.Equal(t, "SELECT 1 FROM DUAL", decoded.SQL)
}

// TestCursorReexec_EnforcesStaticControls: the controls are re-evaluated on
// each execution, not just on the parse that created the cursor. The cursor is
// planted directly because a statement read_only refuses would never get a
// cursor through the parse path — which is exactly the residual exposure the
// spec names (a control that becomes relevant mid-session).
func TestCursorReexec_EnforcesStaticControls(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		controls []string
		sql      string
		wantErr  error
	}{
		{name: "a read is allowed under read_only", controls: []string{store.ControlReadOnly}, sql: "SELECT 1 FROM DUAL"},
		{
			name: "a write is blocked by read_only", controls: []string{store.ControlReadOnly},
			sql: "DELETE FROM emp WHERE id = 1", wantErr: shared.ErrReadOnlyViolation,
		},
		{
			name: "DDL is blocked by block_ddl", controls: []string{store.ControlBlockDDL},
			sql: "DROP TABLE emp", wantErr: shared.ErrDDLBlocked,
		},
		{
			name: "a write is allowed under block_ddl", controls: []string{store.ControlBlockDDL},
			sql: "INSERT INTO emp VALUES (1)",
		},
		{
			name: "an Oracle blocked pattern applies with no controls at all",
			sql:  "ALTER SYSTEM KILL SESSION '123,456'", wantErr: shared.ErrOraclePatternBlocked,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			s := newTestSession(&store.Grant{Definition: &store.GrantDefinition{Controls: tc.controls}})
			s.tracker.cursors[3] = &trackedCursor{cursorID: 3, sql: tc.sql, parsedAt: time.Now()}

			err := s.handleOALL8(buildOALL8Reexec(3))

			if tc.wantErr == nil {
				require.NoError(t, err)
				require.NotNil(t, s.tracker.pendingQuery, "an allowed re-execution is still tracked")
				assert.Equal(t, tc.sql, s.tracker.pendingQuery.cursor.sql)

				return
			}

			require.ErrorIs(t, err, tc.wantErr)
			assert.Nil(t, s.tracker.pendingQuery, "a refused re-execution must not be tracked as in flight")
		})
	}
}

// TestCursorReexec_IsHeldAgainAfterTheFirstHoldIsReleased is the security core
// of the fix: parse a statement an approval pattern matches, let a human
// release it, then re-execute the very same cursor with a SQL-less OALL8. It
// must hang on a human a second time. It used to sail through — the frame read
// as undecodable and was forwarded without the gate ever seeing it.
func TestCursorReexec_IsHeldAgainAfterTheFirstHoldIsReleased(t *testing.T) {
	t.Parallel()

	s, fake, registry := gatedTestSession([]string{`(?i)^DELETE`})

	const sql = "DELETE FROM emp WHERE id = 1"

	// First execution: the parse carries the SQL and is held.
	firstDone := make(chan error, 1)

	go func() { firstDone <- s.handleOALL8(buildOALL8(sql, nil, 4)) }()

	first := awaitHeld(t, fake, 1)
	assert.Equal(t, sql, first.SQLText)
	releaseHold(t, registry, first.UID, store.ApprovalApproved, "")
	requireReleased(t, firstDone)

	// The response came back and the query finished, so nothing is in flight.
	s.tracker.pendingQuery = nil

	// Second execution: same cursor, no SQL on the wire at all.
	secondDone := make(chan error, 1)

	go func() { secondDone <- s.handleOALL8(buildOALL8Reexec(4)) }()

	second := awaitHeld(t, fake, 2)
	assert.Equal(t, sql, second.SQLText, "the hold must name the SQL the cursor was parsed with")
	require.NotNil(t, second.ApprovalPattern)
	assert.Equal(t, `(?i)^DELETE`, *second.ApprovalPattern)

	// It really is parked: nothing may be forwarded while a human has not answered.
	select {
	case <-secondDone:
		t.Fatal("the re-execution was released while it was still awaiting approval")
	case <-time.After(200 * time.Millisecond):
	}

	releaseHold(t, registry, second.UID, store.ApprovalApproved, "")
	requireReleased(t, secondDone)

	require.NotNil(t, s.tracker.pendingQuery)
	assert.Equal(t, second.UID, s.tracker.pendingQuery.queryUID, "the hold's own row is reused")
}

// TestCursorReexec_DeniedIsRefused: a denial on the re-execution comes back as
// an error the dispatcher turns into a TTC error, carrying the reason — the
// second execution is a decision of its own, not an echo of the first.
func TestCursorReexec_DeniedIsRefused(t *testing.T) {
	t.Parallel()

	s, fake, registry := gatedTestSession([]string{`(?i)^DROP`})
	s.tracker.cursors[9] = &trackedCursor{cursorID: 9, sql: "DROP TABLE emp", parsedAt: time.Now()}

	done := make(chan error, 1)

	go func() { done <- s.handleOALL8(buildOALL8Reexec(9)) }()

	pending := awaitHeld(t, fake, 1)
	releaseHold(t, registry, pending.UID, store.ApprovalDenied, "not on a Friday")

	select {
	case err := <-done:
		require.ErrorIs(t, err, shared.ErrApprovalDenied)
		assert.Contains(t, err.Error(), "not on a Friday")
	case <-time.After(10 * time.Second):
		t.Fatal("the denied re-execution was never refused")
	}

	assert.Nil(t, s.tracker.pendingQuery, "a denied re-execution must not be tracked as in flight")
}

// buildPiggybackReexec assembles the frame a modern thin client sends to re-run
// a statement it already parsed: func 0x03, sub-op 0x4e, the v315+ zero pad,
// then the cursor id and the three execution fields decodeCursorReexec walks.
func buildPiggybackReexec(cursorID uint16) []byte {
	out := []byte{byte(TTCFuncPiggyback), PiggybackSubReexecSel, 0x01, 0x00}
	out = append(out, ttcCompressedUint(uint64(cursorID))...)

	// rowsToFetch, execOptions, execFlags.
	for _, v := range []uint64{1, 0x20, 0} {
		out = append(out, ttcCompressedUint(v)...)
	}

	return out
}

// TestBuildPiggybackReexecIsDecodedAsARexecution guards that fixture the same
// way TestBuildOALL8ReexecIsDecodedAsARexecution guards the legacy one: a
// builder that stopped decoding would make every assertion below pass without
// the handler ever seeing a re-execution.
func TestBuildPiggybackReexecIsDecodedAsARexecution(t *testing.T) {
	t.Parallel()

	frame := buildPiggybackReexec(7)

	require.True(t, IsPiggybackCursorReexec(frame), "the fixture must be recognized as a re-execution")

	cursorID, err := decodeCursorReexec(frame)
	require.NoError(t, err)
	assert.Equal(t, uint16(7), cursorID)
}

// reexecOps are the three frames that name a cursor and carry nothing else: the
// legacy SQL-less OALL8, an OFETCH arriving with no query in flight, and the
// piggyback re-execution every modern thin client sends. They must answer an
// untracked cursor identically — otherwise the wire op a client picks is a
// cheaper way past the same grant.
var reexecOps = []struct {
	name string
	run  func(s *session, cursorID uint16) error
}{
	{name: "a SQL-less OALL8", run: func(s *session, cursorID uint16) error {
		return s.handleOALL8(buildOALL8Reexec(cursorID))
	}},
	{name: "an OFETCH", run: func(s *session, cursorID uint16) error {
		return s.handleOFETCH(buildOFETCH(cursorID, 100))
	}},
	{name: "a piggyback re-execution", run: func(s *session, cursorID uint16) error {
		return s.handlePiggybackReexec(buildPiggybackReexec(cursorID))
	}},
}

// TestCursorReexec_UnknownCursorFailsClosedOnlyUnderAStatementControl pins the
// deliberate asymmetry: an execution dbbat cannot identify is refused where
// there is a statement-shaped control to bypass, and forwarded where there is
// none. See docs/approvals.md.
func TestCursorReexec_UnknownCursorFailsClosedOnlyUnderAStatementControl(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		grant   *store.Grant
		refused bool
	}{
		{
			name:    "read_only refuses",
			grant:   &store.Grant{Definition: &store.GrantDefinition{Controls: []string{store.ControlReadOnly}}},
			refused: true,
		},
		{
			name:    "block_ddl refuses",
			grant:   &store.Grant{Definition: &store.GrantDefinition{Controls: []string{store.ControlBlockDDL}}},
			refused: true,
		},
		{
			name:    "an approval pattern refuses",
			grant:   &store.Grant{Definition: &store.GrantDefinition{ApprovalPatterns: []string{`(?i)^DELETE`}}},
			refused: true,
		},
		{
			name:  "a full-write grant forwards",
			grant: &store.Grant{Definition: &store.GrantDefinition{}},
		},
		{
			name:  "block_copy alone forwards",
			grant: &store.Grant{Definition: &store.GrantDefinition{Controls: []string{store.ControlBlockCopy}}},
		},
		{name: "no grant at all forwards"},
	}

	for _, tc := range tests {
		for _, op := range reexecOps {
			t.Run(tc.name+"/"+op.name, func(t *testing.T) {
				t.Parallel()

				s := newTestSession(tc.grant)

				// Cursor 42 was never parsed on this session.
				err := op.run(s, 42)

				if tc.refused {
					require.ErrorIs(t, err, ErrUnknownCursor)
					assert.Nil(t, s.tracker.pendingQuery)

					return
				}

				require.NoError(t, err, "a permissive grant keeps forwarding an unidentifiable re-execution")
				assert.Nil(t, s.tracker.pendingQuery, "a forwarded but unidentified execution is not tracked")
			})
		}
	}
}

// TestInterceptClientMessageBlocksARefusedCursorReexec is the property the
// handler signature only stands for: the refusal has to reach the dispatcher,
// which is the code that decides whether the packet travels upstream.
func TestInterceptClientMessageBlocksARefusedCursorReexec(t *testing.T) {
	t.Parallel()

	s := newTestSession(&store.Grant{Definition: &store.GrantDefinition{Controls: []string{store.ControlReadOnly}}})
	s.clientConn = drainedPipe(t)
	s.tracker.cursors[6] = &trackedCursor{cursorID: 6, sql: "DELETE FROM emp", parsedAt: time.Now()}

	blocked := &TNSPacket{
		Type:    TNSPacketTypeData,
		Payload: append([]byte{0x00, 0x00}, buildOALL8Reexec(6)...),
	}
	assert.True(t, s.interceptClientMessage(blocked),
		"a cursor re-execution refused by the grant must be blocked, not forwarded upstream")

	// And a re-execution of a cursor the grant allows still travels.
	s.tracker.cursors[8] = &trackedCursor{cursorID: 8, sql: "SELECT 1 FROM DUAL", parsedAt: time.Now()}
	allowed := &TNSPacket{
		Type:    TNSPacketTypeData,
		Payload: append([]byte{0x00, 0x00}, buildOALL8Reexec(8)...),
	}
	assert.False(t, s.interceptClientMessage(allowed))
}

// TestOFETCH_ReexecutionIsGated covers the second path: an OFETCH arriving with
// no query in flight starts a fresh pending query — persisted as its own row in
// /queries — and so must be gated as its own query.
func TestOFETCH_ReexecutionIsGated(t *testing.T) {
	t.Parallel()

	s := newTestSession(&store.Grant{Definition: &store.GrantDefinition{Controls: []string{store.ControlReadOnly}}})
	s.tracker.cursors[5] = &trackedCursor{cursorID: 5, sql: "DELETE FROM emp", parsedAt: time.Now()}

	require.ErrorIs(t, s.handleOFETCH(buildOFETCH(5, 100)), shared.ErrReadOnlyViolation)
	assert.Nil(t, s.tracker.pendingQuery, "a refused re-execution must not be tracked as in flight")
}

// TestOFETCH_ReexecutionIsHeld: the approval gate, not just the static
// controls, applies to a fetch that re-executes a cursor.
func TestOFETCH_ReexecutionIsHeld(t *testing.T) {
	t.Parallel()

	s, fake, registry := gatedTestSession([]string{`(?i)^DELETE`})
	s.tracker.cursors[5] = &trackedCursor{cursorID: 5, sql: "DELETE FROM emp", parsedAt: time.Now()}

	done := make(chan error, 1)

	go func() { done <- s.handleOFETCH(buildOFETCH(5, 100)) }()

	pending := awaitHeld(t, fake, 1)
	assert.Equal(t, "DELETE FROM emp", pending.SQLText)

	releaseHold(t, registry, pending.UID, store.ApprovalApproved, "")
	requireReleased(t, done)
}

// TestOFETCH_ContinuationIsNotReGated is the other half of the OFETCH decision,
// and the one that keeps the fix safe: more rows of a statement already in
// flight must never be re-validated, or a long result set could park on a human
// halfway through. The grant here would refuse the statement outright.
func TestOFETCH_ContinuationIsNotReGated(t *testing.T) {
	t.Parallel()

	s, fake, _ := gatedTestSession([]string{`(?i)^DELETE`})

	cursor := &trackedCursor{cursorID: 5, sql: "DELETE FROM emp", parsedAt: time.Now()}
	s.tracker.cursors[5] = cursor
	// A query is already in flight — it went through the gate when it started.
	s.tracker.pendingQuery = &pendingOracleQuery{cursor: cursor, startTime: time.Now()}

	require.NoError(t, s.handleOFETCH(buildOFETCH(5, 100)))
	assert.Empty(t, fake.held(), "a fetch continuing a query in flight must not be held")
	assert.Equal(t, cursor, s.tracker.pendingQuery.cursor, "the in-flight query is untouched")

	// Every refusal the re-execution branch can raise must stay off this path,
	// not just the hold: a result set already streaming must never be cut short.
	// The quota is the one added last and the easiest to leak onto it.
	maxQueries := int64(1)
	s.grant.QueryCount = 99
	s.grant.Definition.MaxQueryCounts = &maxQueries
	s.grant.Definition.Controls = []string{store.ControlReadOnly}

	require.NoError(t, s.handleOFETCH(buildOFETCH(5, 100)),
		"a fetch continuing a query in flight is never re-gated, quota included")

	// Not even a fetch naming a cursor that is no longer tracked: what decides
	// is that a query is in flight, and it went through the gate when it started.
	delete(s.tracker.cursors, 5)
	require.NoError(t, s.handleOFETCH(buildOFETCH(5, 100)))
	assert.Equal(t, cursor, s.tracker.pendingQuery.cursor, "the in-flight query is still untouched")
}

// TestOFETCH_ReexecutionCountsAgainstTheQueryQuota: a fetch that starts a fresh
// query persists its own /queries row, so it must also be counted against
// max_query_counts. It was not: handleOFETCH bypasses gateStatement (which
// would refuse continuation fetches mid-result-set), and MaxQueryCounts is not
// one of the limits the response leg's LimitGuard re-checks — so a client that
// parsed once and looped re-executions sailed past its cap.
func TestOFETCH_ReexecutionCountsAgainstTheQueryQuota(t *testing.T) {
	t.Parallel()

	maxQueries := int64(3)
	s := newTestSession(&store.Grant{
		QueryCount: 3,
		Definition: &store.GrantDefinition{MaxQueryCounts: &maxQueries},
	})
	s.tracker.cursors[5] = &trackedCursor{cursorID: 5, sql: "SELECT 1 FROM DUAL", parsedAt: time.Now()}

	require.ErrorIs(t, s.handleOFETCH(buildOFETCH(5, 100)), ErrQueryLimitExceed)
	assert.Nil(t, s.tracker.pendingQuery, "a re-execution over quota must not be tracked as in flight")

	// One under the cap and the very same fetch is allowed through.
	s.grant.QueryCount = 2
	require.NoError(t, s.handleOFETCH(buildOFETCH(5, 100)))
	require.NotNil(t, s.tracker.pendingQuery)
	assert.Equal(t, "SELECT 1 FROM DUAL", s.tracker.pendingQuery.cursor.sql)
}

// TestCursorReexec_SQLlessOALL8CountsAgainstTheQueryQuota pins the other half of
// the same guarantee, which was already true by construction: the SQL-less
// OALL8 reaches its handler through gateStatement, so checkQuotas has always
// run on it. Cheap to assert, and it is what keeps the two re-execution frames
// answering alike — the point of routing the OFETCH through the same rules.
func TestCursorReexec_SQLlessOALL8CountsAgainstTheQueryQuota(t *testing.T) {
	t.Parallel()

	maxQueries := int64(3)
	s := newTestSession(&store.Grant{
		QueryCount: 3,
		Definition: &store.GrantDefinition{MaxQueryCounts: &maxQueries},
	})
	s.clientConn = drainedPipe(t)
	s.tracker.cursors[6] = &trackedCursor{cursorID: 6, sql: "SELECT 1 FROM DUAL", parsedAt: time.Now()}

	overQuota := &TNSPacket{
		Type:    TNSPacketTypeData,
		Payload: append([]byte{0x00, 0x00}, buildOALL8Reexec(6)...),
	}
	assert.True(t, s.interceptClientMessage(overQuota),
		"a cursor re-execution over max_query_counts must be blocked, not forwarded upstream")
	assert.Nil(t, s.tracker.pendingQuery)

	s.grant.QueryCount = 2
	assert.False(t, s.interceptClientMessage(overQuota), "under the cap the same frame travels")
	require.NotNil(t, s.tracker.pendingQuery)
}

// TestInterceptClientMessageBlocksAnOverQuotaOFETCHReexecution is the property
// the handler signature only stands for: the refusal has to reach the
// dispatcher, which is what decides whether the packet travels upstream. The
// OFETCH branch answers the client itself rather than through gateStatement, so
// it is worth pinning separately.
func TestInterceptClientMessageBlocksAnOverQuotaOFETCHReexecution(t *testing.T) {
	t.Parallel()

	maxQueries := int64(1)
	s := newTestSession(&store.Grant{
		QueryCount: 1,
		Definition: &store.GrantDefinition{MaxQueryCounts: &maxQueries},
	})
	s.clientConn = drainedPipe(t)
	s.tracker.cursors[5] = &trackedCursor{cursorID: 5, sql: "SELECT 1 FROM DUAL", parsedAt: time.Now()}

	fetch := &TNSPacket{
		Type:    TNSPacketTypeData,
		Payload: append([]byte{0x00, 0x00}, buildOFETCH(5, 100)...),
	}
	assert.True(t, s.interceptClientMessage(fetch),
		"a fetch re-executing a cursor over max_query_counts must be blocked, not forwarded upstream")

	s.grant.QueryCount = 0
	assert.False(t, s.interceptClientMessage(fetch), "under the cap the same fetch travels")
}

// awaitHeld waits until want statements have been parked and returns the last.
func awaitHeld(t *testing.T, fake *fakeApprovalStore, want int) *store.Query {
	t.Helper()

	var last *store.Query

	require.Eventually(t, func() bool {
		held := fake.held()
		if len(held) < want {
			return false
		}

		last = held[want-1]

		return true
	}, 10*time.Second, 10*time.Millisecond)

	return last
}

// releaseHold answers a parked statement as a human would.
func releaseHold(t *testing.T, registry *approval.Registry, uid uuid.UUID, status, reason string) {
	t.Helper()

	approver := uuid.New()
	require.True(t, registry.Resolve(approval.Decision{
		QueryUID: uid,
		Status:   status,
		By:       &approver,
		ByName:   "an approver",
		Reason:   reason,
		At:       time.Now(),
	}))
}

// requireReleased asserts the handler returned, without error, once released.
func requireReleased(t *testing.T, done <-chan error) {
	t.Helper()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("the approved statement was never released")
	}
}

// drainedPipe returns the proxy end of a net.Pipe whose client end is read and
// discarded — the refusal is written to the client leg, so somebody has to read
// it or the handler blocks.
func drainedPipe(t *testing.T) net.Conn {
	t.Helper()

	client, proxySide := net.Pipe()

	t.Cleanup(func() {
		_ = client.Close()
		_ = proxySide.Close()
	})

	go func() {
		buf := make([]byte, 4096)

		for {
			if _, err := client.Read(buf); err != nil {
				return
			}
		}
	}()

	return proxySide
}
