package oracle

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/approval"
	"github.com/fclairamb/dbbat/internal/proxy/shared"
	"github.com/fclairamb/dbbat/internal/store"
)

// buildJDBCExec builds a func=0x11 / sub-op 0x69 execute-with-SQL payload —
// the shape the JDBC thin driver (and DBeaver) sends instead of OALL8. The SQL
// sits length-prefixed at offset 50, inside the window decodeExecSQL scans.
//
// It opens with a close-cursors list that actually *walks* (`01` pointer flag,
// one compressed count, one compressed id), which every one of the 54 recorded
// `11 69` frames in testdata/dbeaver.pcapng does. That is not decoration: a
// close list dbbat can walk is what lets clientCallNumber find the call stapled
// behind it, and a frame whose list does *not* walk is unnameable and takes the
// teardown path instead of the OER one (gateUnnameableFrame). Building the
// nameable shape here is what keeps these tests on the path real clients use —
// buildUnwalkableJDBCExec covers the other one.
func buildJDBCExec(sql string) []byte {
	const sqlOffset = 50

	// 11 69 <seq> 00 | 01 pointer | 01 01 count=1 | 01 02 id=2
	header := []byte{byte(TTCFuncOFETCH), execSubOpJDBC, 0x07, 0x00, 0x01, 0x01, 0x01, 0x01, 0x02}

	buf := make([]byte, 0, sqlOffset+5+len(sql))
	buf = append(buf, header...)
	buf = append(buf, make([]byte, sqlOffset-len(header))...)
	buf = append(buf, encodeVarLen(uint32(len(sql)))...)
	buf = append(buf, []byte(sql)...)

	return buf
}

// buildUnwalkableJDBCExec is the same execute behind a close-cursors list dbbat
// cannot walk — no pointer flag, so decodeCloseCursors rejects it and the call
// stapled behind it is invisible.
func buildUnwalkableJDBCExec(sql string) []byte {
	frame := buildJDBCExec(sql)
	frame[4] = 0x00 // the pointer flag the walk requires

	return frame
}

// TestBuildJDBCExecFixturesAreTheTwoShapesTheyClaim guards both builders, so
// the enforcement tests below cannot quietly swap paths on each other: one must
// be nameable (the recorded shape, answered with an OER) and one must not be
// (answered by ending the session).
func TestBuildJDBCExecFixturesAreTheTwoShapesTheyClaim(t *testing.T) {
	t.Parallel()

	walkable := buildJDBCExec("DROP TABLE emp")
	require.True(t, IsExecSQL(walkable))

	ids, err := decodeCloseCursors(walkable)
	require.NoError(t, err, "the recorded JDBC shape opens with a close list that walks")
	assert.Equal(t, []uint16{2}, ids)

	call, ok := clientCallNumber(walkable)
	require.True(t, ok, "and is therefore nameable")
	assert.Equal(t, byte(0x07), call)

	unwalkable := buildUnwalkableJDBCExec("DROP TABLE emp")
	require.True(t, IsExecSQL(unwalkable), "still an exec, still carries its SQL")
	_, err = decodeCloseCursors(unwalkable)
	require.Error(t, err)

	_, ok = clientCallNumber(unwalkable)
	assert.False(t, ok, "so dbbat cannot name the call it would have to answer")
}

// TestInterceptClientMessageEndsTheSessionOnAnUnnameableRefusedExec is the leak
// this closes. `IsExecSQL` used to be tested *before* the nameability check, so
// a `11 69` exec whose close list did not walk was gated and answered — with a
// stale call number, because clientCallNumber had just declined to name it.
// That is the ORA-18745/hang mode
// specs/done/2026/08/2026-08-12-02-oracle-async-refusal-call-number.md is about,
// reintroduced by the fix for a different hang.
//
// The statement is still refused. It is refused by ending the session, which is
// the only honest answer for a call dbbat cannot name.
func TestInterceptClientMessageEndsTheSessionOnAnUnnameableRefusedExec(t *testing.T) {
	t.Parallel()

	s := newTestSession(&store.Grant{Definition: &store.GrantDefinition{Controls: []string{store.ControlBlockDDL}}})

	client, answered := recordingPipe(t)
	s.clientConn = client

	upstream, upstreamPeer := net.Pipe()
	t.Cleanup(func() { _ = upstreamPeer.Close() })
	s.upstreamConn = upstream

	pkt := &TNSPacket{
		Type:    TNSPacketTypeData,
		Payload: append([]byte{0x00, 0x00}, buildUnwalkableJDBCExec("DROP TABLE emp")...),
	}

	assert.True(t, s.interceptClientMessage(pkt),
		"a DDL refused by the grant must not travel upstream just because its frame is unnameable")

	assert.Empty(t, answered(),
		"and must not be answered with an OER: dbbat cannot name the call it would be ending")

	assert.Error(t, upstream.SetDeadline(time.Now()),
		"the session must be torn down, not left parked")
}

// TestBuildJDBCExecIsRecognizedAsAnExec guards the fixture itself: if the
// builder stopped producing something IsExecSQL/decodeExecSQL accept, every
// enforcement test below would pass vacuously.
func TestBuildJDBCExecIsRecognizedAsAnExec(t *testing.T) {
	t.Parallel()

	payload := buildJDBCExec("DELETE FROM emp WHERE id = 1")
	require.True(t, IsExecSQL(payload), "the fixture must be dispatched as an exec, not as a plain OFETCH")

	decoded, err := decodeExecSQL(payload)
	require.NoError(t, err)
	assert.Equal(t, "DELETE FROM emp WHERE id = 1", decoded.SQL)
}

// TestHandleJDBCExec_EnforcesStaticControls is the regression test for the
// hole: the JDBC thin driver's dedicated exec op used to record the statement
// and forward it, consulting neither read_only nor block_ddl nor the Oracle
// blocked patterns. Whatever OALL8 refuses, this must refuse identically.
func TestHandleJDBCExec_EnforcesStaticControls(t *testing.T) {
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
		{
			name: "a password change is always blocked",
			sql:  "ALTER USER bob PASSWORD 'secret'", wantErr: shared.ErrPasswordChangeBlocked,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			s := newTestSession(&store.Grant{Definition: &store.GrantDefinition{Controls: tc.controls}})

			err := s.handleJDBCExec(buildJDBCExec(tc.sql))

			if tc.wantErr == nil {
				require.NoError(t, err)
				require.NotNil(t, s.tracker.pendingQuery, "an allowed statement is still tracked")
				assert.Equal(t, tc.sql, s.tracker.pendingQuery.cursor.sql)

				return
			}

			require.ErrorIs(t, err, tc.wantErr)
			assert.Nil(t, s.tracker.pendingQuery,
				"a refused statement must not be tracked as in flight")
		})
	}
}

// TestInterceptClientMessageBlocksARefusedJDBCExec is the property the handler
// signature only stands for: the refusal has to reach the dispatcher, which is
// the code that decides whether the packet travels upstream. The handler used
// to return nothing at all, so the dispatcher forwarded unconditionally.
func TestInterceptClientMessageBlocksARefusedJDBCExec(t *testing.T) {
	t.Parallel()

	client, proxySide := net.Pipe()

	t.Cleanup(func() {
		_ = client.Close()
		_ = proxySide.Close()
	})

	// The refusal is written to the client leg, so somebody has to read it.
	go func() {
		buf := make([]byte, 4096)

		for {
			if _, err := client.Read(buf); err != nil {
				return
			}
		}
	}()

	s := newTestSession(&store.Grant{Definition: &store.GrantDefinition{Controls: []string{store.ControlBlockDDL}}})
	s.clientConn = proxySide

	pkt := &TNSPacket{
		Type:    TNSPacketTypeData,
		Payload: append([]byte{0x00, 0x00}, buildJDBCExec("DROP TABLE emp")...),
	}

	assert.True(t, s.interceptClientMessage(pkt),
		"a JDBC exec refused by the grant must be blocked, not forwarded upstream")

	// And an allowed one still travels.
	allowed := &TNSPacket{
		Type:    TNSPacketTypeData,
		Payload: append([]byte{0x00, 0x00}, buildJDBCExec("SELECT 1 FROM DUAL")...),
	}

	assert.False(t, s.interceptClientMessage(allowed))
}

// fakeApprovalStore is the slice of the store an approval hold writes through,
// in memory — enough to drive a hold end to end with no database.
type fakeApprovalStore struct {
	mu       sync.Mutex
	pending  []*store.Query
	resolved map[uuid.UUID]string
}

func newFakeApprovalStore() *fakeApprovalStore {
	return &fakeApprovalStore{resolved: make(map[uuid.UUID]string)}
}

func (f *fakeApprovalStore) CreatePendingQuery(
	_ context.Context, query *store.Query, pattern string,
) (*store.Query, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	status := store.ApprovalPending
	created := &store.Query{
		UID:             uuid.New(),
		ConnectionID:    query.ConnectionID,
		SQLText:         query.SQLText,
		ExecutedAt:      query.ExecutedAt,
		ApprovalStatus:  &status,
		ApprovalPattern: &pattern,
	}

	f.pending = append(f.pending, created)

	return created, nil
}

func (f *fakeApprovalStore) ResolveQueryApproval(
	_ context.Context, uid uuid.UUID, status string, _ *uuid.UUID, _ string,
) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.resolved[uid] = status

	return nil
}

func (f *fakeApprovalStore) NotifyEvent(_ context.Context, _ string, _ store.EventNotification) error {
	return nil
}

// held returns the statements parked so far.
func (f *fakeApprovalStore) held() []*store.Query {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]*store.Query(nil), f.pending...)
}

// gatedTestSession builds a session whose approval gate is armed with the given
// patterns, backed by an in-memory store and a real registry.
func gatedTestSession(patterns []string) (*session, *fakeApprovalStore, *approval.Registry) {
	fake := newFakeApprovalStore()
	registry := approval.NewRegistry()

	grant := &store.Grant{Definition: &store.GrantDefinition{ApprovalPatterns: patterns}}
	s := newTestSession(grant)

	s.approvalGate = shared.NewApprovalGate(shared.ApprovalDeps{
		Enabled:      true,
		Store:        fake,
		Registry:     registry,
		Logger:       testLogger(),
		PollInterval: 50 * time.Millisecond,
	}, grant, s.connectionUID, nil, "orcl")

	return s, fake, registry
}

// TestHandleJDBCExec_ParksOnAnApprovalPattern is the security half of the fix:
// a statement matching an approval pattern must hang on a human, and the
// dispatcher must not be allowed to forward it in the meantime. It used to sail
// straight through — the gate was never consulted on this op at all.
func TestHandleJDBCExec_ParksOnAnApprovalPattern(t *testing.T) {
	t.Parallel()

	s, fake, registry := gatedTestSession([]string{`(?i)^DELETE`})

	const sql = "DELETE FROM emp WHERE id = 1"

	done := make(chan error, 1)

	go func() { done <- s.handleJDBCExec(buildJDBCExec(sql)) }()

	// The statement is parked: persisted as pending, and the handler has not
	// returned, so nothing downstream can forward the packet.
	var pending *store.Query

	require.Eventually(t, func() bool {
		held := fake.held()
		if len(held) != 1 {
			return false
		}

		pending = held[0]

		return true
	}, 10*time.Second, 10*time.Millisecond)

	assert.Equal(t, sql, pending.SQLText)
	require.NotNil(t, pending.ApprovalPattern)
	assert.Equal(t, `(?i)^DELETE`, *pending.ApprovalPattern)

	select {
	case <-done:
		t.Fatal("the statement was released while it was still awaiting approval")
	case <-time.After(200 * time.Millisecond):
	}

	// A human approves, and only then does the handler let it through.
	approver := uuid.New()
	require.True(t, registry.Resolve(approval.Decision{
		QueryUID: pending.UID,
		Status:   store.ApprovalApproved,
		By:       &approver,
		ByName:   "an approver",
		At:       time.Now(),
	}))

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("the approved statement was never released")
	}

	// The hold's own row is reused rather than a second one being written for
	// the same statement.
	require.NotNil(t, s.tracker.pendingQuery)
	assert.Equal(t, pending.UID, s.tracker.pendingQuery.queryUID)
	assert.True(t, s.tracker.pendingQuery.queryPersisted)

	// And a statement the pattern does not match is not parked — which is what
	// proves the hold above was the pattern matching, not a pattern matching
	// everything.
	require.NoError(t, s.handleJDBCExec(buildJDBCExec("SELECT 1 FROM DUAL")))
	assert.Len(t, fake.held(), 1)
}

// TestHandleJDBCExec_DeniedIsRefused: a denial has to come back as an error the
// dispatcher turns into a TTC error, carrying the approver's reason.
func TestHandleJDBCExec_DeniedIsRefused(t *testing.T) {
	t.Parallel()

	s, fake, registry := gatedTestSession([]string{`(?i)^DROP`})

	done := make(chan error, 1)

	go func() { done <- s.handleJDBCExec(buildJDBCExec("DROP TABLE emp")) }()

	var pending *store.Query

	require.Eventually(t, func() bool {
		held := fake.held()
		if len(held) != 1 {
			return false
		}

		pending = held[0]

		return true
	}, 10*time.Second, 10*time.Millisecond)

	approver := uuid.New()
	require.True(t, registry.Resolve(approval.Decision{
		QueryUID: pending.UID,
		Status:   store.ApprovalDenied,
		By:       &approver,
		ByName:   "an approver",
		Reason:   "not on a Friday",
		At:       time.Now(),
	}))

	select {
	case err := <-done:
		require.ErrorIs(t, err, shared.ErrApprovalDenied)
		assert.Contains(t, err.Error(), "not on a Friday")
	case <-time.After(10 * time.Second):
		t.Fatal("the denied statement was never refused")
	}

	assert.Nil(t, s.tracker.pendingQuery, "a denied statement must not be tracked as in flight")
}

// TestHandleJDBCExec_FailsClosedWhenTheHoldCannotBeRegistered pins the
// fail-closed rule on the op that used to have no gate at all: a gate that
// cannot persist its pending row refuses the statement rather than forwarding
// it unheld.
func TestHandleJDBCExec_FailsClosedWhenTheHoldCannotBeRegistered(t *testing.T) {
	t.Parallel()

	grant := &store.Grant{Definition: &store.GrantDefinition{ApprovalPatterns: []string{`(?i)^DELETE`}}}
	s := newTestSession(grant)

	// Enabled with patterns, but no store behind it: Hold has nowhere to write.
	s.approvalGate = shared.NewApprovalGate(shared.ApprovalDeps{
		Enabled: true,
		Logger:  testLogger(),
	}, grant, s.connectionUID, nil, "orcl")

	err := s.handleJDBCExec(buildJDBCExec("DELETE FROM emp WHERE id = 1"))
	require.ErrorIs(t, err, shared.ErrApprovalUnavailable)
	assert.Nil(t, s.tracker.pendingQuery)
}
