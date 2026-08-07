package mssql

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/approval"
	"github.com/fclairamb/dbbat/internal/config"
	"github.com/fclairamb/dbbat/internal/proxy/shared"
	"github.com/fclairamb/dbbat/internal/store"
)

// grantWithControls builds an in-memory grant carrying the given controls.
func grantWithControls(controls ...string) *store.Grant {
	return &store.Grant{
		Definition: &store.GrantDefinition{Controls: controls},
		ExpiresAt:  time.Now().Add(time.Hour),
	}
}

// sessionWithGrant builds a session complete enough to classify a request.
func sessionWithGrant(t *testing.T, grant *store.Grant) *session {
	t.Helper()

	return &session{
		server:   &Server{logger: testLogger()},
		logger:   testLogger(),
		grant:    grant,
		prepared: make(map[int64]string),
	}
}

// TestRPCEnforcementIsNotLogOnly is the regression test for the owner's binding
// decision: a write wrapped in sp_executesql must be enforced exactly like the
// same write sent as a plain SQLBatch. Log-only here would make read_only and
// block_ddl bypassable by any client.
func TestRPCEnforcementIsNotLogOnly(t *testing.T) {
	t.Parallel()

	session := sessionWithGrant(t, grantWithControls(store.ControlReadOnly))

	requests, err := parseRPC(rpcByID(spExecuteSQL, nvarcharMaxParam("", "DELETE FROM employés")))
	require.NoError(t, err)

	st := session.describeRPC(requests)
	require.NoError(t, st.refusal)

	require.ErrorIs(t, session.validate(st), shared.ErrReadOnlyViolation)
}

// TestRPCEnforcementDecisions walks the classification of every RPC shape, with
// the grant that makes the decision interesting.
func TestRPCEnforcementDecisions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		controls []string
		payload  []byte
		// prepared seeds a handle→statement mapping, as a prior sp_prepare on
		// this session would have.
		prepared map[int64]string
		wantErr  error
	}{
		{
			name:     "a read through sp_executesql is allowed under read_only",
			controls: []string{store.ControlReadOnly},
			payload:  rpcByID(spExecuteSQL, nvarcharMaxParam("", "SELECT 1")),
		},
		{
			name:     "DDL through sp_executesql is blocked by block_ddl",
			controls: []string{store.ControlBlockDDL},
			payload:  rpcByID(spExecuteSQL, nvarcharMaxParam("", "DROP TABLE t")),
			wantErr:  shared.ErrDDLBlocked,
		},
		{
			name:     "a prepared write is blocked at prepare time",
			controls: []string{store.ControlReadOnly},
			payload: rpcByID(spPrepExec,
				intNParam("@handle", 0, 0x01),
				nvarcharMaxParam("", ""),
				nvarcharMaxParam("", "UPDATE t SET x = 1")),
			wantErr: shared.ErrReadOnlyViolation,
		},
		{
			name:     "executing a handle prepared on this session is enforced on its statement",
			controls: []string{store.ControlReadOnly},
			payload:  rpcByID(spExecute, intNParam("@handle", 41, 0)),
			prepared: map[int64]string{41: "DELETE FROM t"},
			wantErr:  shared.ErrReadOnlyViolation,
		},
		{
			name:     "executing a handle prepared with a read is allowed",
			controls: []string{store.ControlReadOnly},
			payload:  rpcByID(spExecute, intNParam("@handle", 41, 0)),
			prepared: map[int64]string{41: "SELECT * FROM t"},
		},
		{
			name:     "a bulk insert is blocked by block_copy",
			controls: []string{store.ControlBlockCopy},
			payload:  rpcByID(spExecuteSQL, nvarcharMaxParam("", "BULK INSERT t FROM 'x.csv'")),
			wantErr:  ErrBulkCopyBlocked,
		},
		{
			name:     "cursor bookkeeping is allowed under a restrictive grant",
			controls: []string{store.ControlReadOnly},
			payload:  rpcByID(spCursorClose, intNParam("@cursor", 3, 0)),
		},
		{
			name:    "an unknown handle is forwarded when the grant restricts nothing",
			payload: rpcByID(spExecute, intNParam("@handle", 99, 0)),
		},
		{
			name:     "an unknown handle fails closed under a restrictive grant",
			controls: []string{store.ControlReadOnly},
			payload:  rpcByID(spExecute, intNParam("@handle", 99, 0)),
			wantErr:  ErrUnknownPreparedStatement,
		},
		{
			name:    "a stored procedure is forwarded when the grant restricts nothing",
			payload: rpcByName("dbo.Recalculer", intNParam("@year", 2026, 0)),
		},
		{
			name:     "a stored procedure fails closed under a restrictive grant",
			controls: []string{store.ControlReadOnly},
			payload:  rpcByName("dbo.Recalculer", intNParam("@year", 2026, 0)),
			wantErr:  ErrOpaqueProcedureBlocked,
		},
		{
			name:     "a stored procedure fails closed under block_ddl too",
			controls: []string{store.ControlBlockDDL},
			payload:  rpcByName("dbo.Recalculer"),
			wantErr:  ErrOpaqueProcedureBlocked,
		},
		{
			name:     "the second call of a batch is enforced as well as the first",
			controls: []string{store.ControlReadOnly},
			payload: func() []byte {
				first := rpcByID(spExecuteSQL, nvarcharMaxParam("", "SELECT 1"))
				second := rpcByID(spExecuteSQL, nvarcharMaxParam("", "DELETE FROM t"))

				out := append(append([]byte{}, first...), batchFlagTransaction)

				return append(out, second[len(testAllHeaders()):]...)
			}(),
			wantErr: shared.ErrReadOnlyViolation,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			session := sessionWithGrant(t, grantWithControls(tc.controls...))
			for handle, sql := range tc.prepared {
				session.rememberPrepared(handle, sql)
			}

			requests, err := parseRPC(tc.payload)
			require.NoError(t, err)

			st := session.describeRPC(requests)

			got := st.refusal
			if got == nil {
				got = session.validate(st)
			}

			if tc.wantErr == nil {
				require.NoError(t, got)

				return
			}

			require.ErrorIs(t, got, tc.wantErr)
		})
	}
}

// TestSQLBatchEnforcement covers the plain-statement path, including the
// non-ASCII case: a mangled decode is a pattern that stops matching.
func TestSQLBatchEnforcement(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		controls []string
		sql      string
		wantErr  error
	}{
		{name: "a read is allowed", controls: []string{store.ControlReadOnly}, sql: "SELECT 1"},
		{
			name: "a write is blocked", controls: []string{store.ControlReadOnly},
			sql: "DELETE FROM employés", wantErr: shared.ErrReadOnlyViolation,
		},
		{
			name: "DDL is blocked", controls: []string{store.ControlBlockDDL},
			sql: "CREATE TABLE [taille_données] (x int)", wantErr: shared.ErrDDLBlocked,
		},
		{
			name: "a bulk insert is blocked", controls: []string{store.ControlBlockCopy},
			sql: "BULK INSERT t FROM 'données.csv'", wantErr: ErrBulkCopyBlocked,
		},
		{
			name: "a password change is always blocked",
			sql:  "ALTER USER [bob] WITH PASSWORD = 'x'", wantErr: shared.ErrPasswordChangeBlocked,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			session := sessionWithGrant(t, grantWithControls(tc.controls...))

			sql, err := parseSQLBatch(sqlBatchPayload(tc.sql))
			require.NoError(t, err)
			require.Equal(t, tc.sql, sql)

			got := session.validate(statement{text: sql, enforce: []string{sql}})

			if tc.wantErr == nil {
				require.NoError(t, got)

				return
			}

			require.ErrorIs(t, got, tc.wantErr)
		})
	}
}

// readRefusal drives one message through the proxy and decodes the ERROR token
// it is answered with.
func (f *authFixture) sendAndRead(t *testing.T, client *testClient, msgType byte, payload []byte) []byte {
	t.Helper()

	require.NoError(t, client.pkt.WriteMessage(msgType, payload))

	return client.readReply(t)
}

// firstErrorToken decodes the first ERROR token of a response.
func firstErrorToken(t *testing.T, response []byte) tdsMessage {
	t.Helper()

	require.NotEmpty(t, response)
	require.Equal(t, tokenError, response[0], "the response must start with an ERROR token")

	length := int(response[1]) | int(response[2])<<8

	msg, err := parseInfoBody(response[3 : 3+length])
	require.NoError(t, err)

	return msg
}

// TestSessionBlocksAWriteOnBothPaths is the end-to-end regression test the spec
// asks for: the same write, once as a plain SQLBatch and once wrapped in
// sp_executesql, must be refused identically and must never reach the upstream.
func TestSessionBlocksAWriteOnBothPaths(t *testing.T) {
	t.Parallel()

	fixture := newAuthFixtureWith(t, fixtureOptions{
		upstreamEncryption: encryptNotSup,
		sslMode:            "disable",
		controls:           []string{store.ControlReadOnly},
	})

	client, outcome := fixture.login(t, fixtureUser, fixturePassword, fixtureDBEntry)
	require.True(t, outcome.Acked)

	const write = "DELETE FROM employés WHERE id = 1"

	batchReply := fixture.sendAndRead(t, client, packetTypeSQLBatch, sqlBatchPayload(write))
	batchError := firstErrorToken(t, batchReply)
	assert.Contains(t, batchError.Message, "read-only")

	rpcReply := fixture.sendAndRead(t, client, packetTypeRPC,
		rpcByID(spExecuteSQL, nvarcharMaxParam("", write)))
	rpcError := firstErrorToken(t, rpcReply)
	assert.Contains(t, rpcError.Message, "read-only",
		"sp_executesql must be enforced, not merely logged")

	assert.Equal(t, batchError.Message, rpcError.Message,
		"the two paths must refuse a write identically")

	assert.Empty(t, fixture.fake.receivedRequests(),
		"a blocked statement must never reach the upstream")

	// Both refusals are logged, with the statement text.
	require.Eventually(t, func() bool {
		queries, err := fixture.store.ListQueries(context.Background(), store.QueryFilter{Limit: 10})

		return err == nil && len(queries) == 2
	}, 10*time.Second, 50*time.Millisecond)

	queries, err := fixture.store.ListQueries(context.Background(), store.QueryFilter{Limit: 10})
	require.NoError(t, err)
	require.Len(t, queries, 2)

	for _, query := range queries {
		require.NotNil(t, query.Error)
		assert.Contains(t, *query.Error, "read-only")
		assert.Contains(t, query.SQLText, "employés", "the statement is stored as readable UCS-2 text")
	}
}

// TestSessionKeepsWorkingAfterARefusal proves the refusal is a statement-level
// error rather than a session-level one: the connection survives it.
func TestSessionKeepsWorkingAfterARefusal(t *testing.T) {
	t.Parallel()

	fixture := newAuthFixtureWith(t, fixtureOptions{
		upstreamEncryption: encryptNotSup,
		sslMode:            "disable",
		controls:           []string{store.ControlReadOnly},
	})

	client, outcome := fixture.login(t, fixtureUser, fixturePassword, fixtureDBEntry)
	require.True(t, outcome.Acked)

	fixture.sendAndRead(t, client, packetTypeSQLBatch, sqlBatchPayload("DROP TABLE t"))

	reply := fixture.sendAndRead(t, client, packetTypeSQLBatch, sqlBatchPayload("SELECT 1"))
	assert.Equal(t, buildDoneToken(0, 1), reply, "the next statement must go through untouched")

	require.Len(t, fixture.fake.receivedRequests(), 1)
}

// TestSessionLogsAndAccountsForAStatement covers the whole observation path:
// the statement lands in the query history with the row count the upstream
// reported and the rows it returned.
func TestSessionLogsAndAccountsForAStatement(t *testing.T) {
	t.Parallel()

	fixture := newAuthFixtureWith(t, fixtureOptions{
		upstreamEncryption: encryptNotSup,
		sslMode:            "disable",
		queryStorage:       config.QueryStorageConfig{StoreResults: true, MaxResultRows: 100},
	})

	// A two-row result set, so the accountant has something to walk.
	response := colMetadataToken(intColumn("id"), nvarcharColumn("nom"))
	response = append(response, rowToken(intValue(1), nvarcharValue("Zoé"))...)
	response = append(response, rowToken(intValue(2), nvarcharValue("Ünïcödé"))...)
	response = append(response, buildDoneToken(doneCount, 2)...)

	fixture.fake.mu.Lock()
	fixture.fake.batchResponse = response
	fixture.fake.mu.Unlock()

	client, outcome := fixture.login(t, fixtureUser, fixturePassword, fixtureDBEntry)
	require.True(t, outcome.Acked)

	const sql = "SELECT id, nom FROM employés"

	fixture.sendAndRead(t, client, packetTypeSQLBatch, sqlBatchPayload(sql))

	var query store.Query

	require.Eventually(t, func() bool {
		queries, err := fixture.store.ListQueries(context.Background(), store.QueryFilter{Limit: 10})
		if err != nil || len(queries) != 1 || queries[0].RowsAffected == nil {
			return false
		}

		query = queries[0]

		return true
	}, 15*time.Second, 50*time.Millisecond)

	assert.Equal(t, sql, query.SQLText)
	require.NotNil(t, query.RowsAffected)
	assert.Equal(t, int64(2), *query.RowsAffected)
	assert.Nil(t, query.Error)
	require.NotNil(t, query.DurationMs)

	rows, err := fixture.store.GetQueryRows(context.Background(), query.UID, "", 10)
	require.NoError(t, err)
	require.Len(t, rows.Rows, 2)
	assert.JSONEq(t, `[1,"Zoé"]`, string(rows.Rows[0].RowData))
	assert.JSONEq(t, `[2,"Ünïcödé"]`, string(rows.Rows[1].RowData))
}

// TestSessionRecordsAnUpstreamError proves an ERROR token becomes the query
// row's error as a real, sanitized diagnostic — never raw bytes.
func TestSessionRecordsAnUpstreamError(t *testing.T) {
	t.Parallel()

	fixture := newAuthFixtureWith(t, fixtureOptions{
		upstreamEncryption: encryptNotSup,
		sslMode:            "disable",
	})

	response := buildErrorToken(208, 1, 16, "Nom d'objet 'employés' non valide.", "", 1)
	response = append(response, buildDoneToken(doneError, 0)...)

	fixture.fake.mu.Lock()
	fixture.fake.batchResponse = response
	fixture.fake.mu.Unlock()

	client, outcome := fixture.login(t, fixtureUser, fixturePassword, fixtureDBEntry)
	require.True(t, outcome.Acked)

	fixture.sendAndRead(t, client, packetTypeSQLBatch, sqlBatchPayload("SELECT * FROM employés"))

	require.Eventually(t, func() bool {
		queries, err := fixture.store.ListQueries(context.Background(), store.QueryFilter{Limit: 10})

		return err == nil && len(queries) == 1 && queries[0].Error != nil
	}, 15*time.Second, 50*time.Millisecond)

	queries, err := fixture.store.ListQueries(context.Background(), store.QueryFilter{Limit: 10})
	require.NoError(t, err)
	require.Len(t, queries, 1)
	require.NotNil(t, queries[0].Error)
	assert.Contains(t, *queries[0].Error, "Nom d'objet 'employés' non valide.")
	assert.Contains(t, *queries[0].Error, "error 208")
}

// TestSessionParksAStatementOnAnApprovalHold is the behavior most likely to
// break subtly under a protocol the gate has never driven: the client must
// simply wait — no timeout, no torn-down connection — and get its answer once a
// human approves.
func TestSessionParksAStatementOnAnApprovalHold(t *testing.T) {
	t.Parallel()

	registry := approval.NewRegistry()

	fixture := newAuthFixtureWith(t, fixtureOptions{
		upstreamEncryption: encryptNotSup,
		sslMode:            "disable",
		approvalPatterns:   []string{`(?i)^DELETE`},
		approvalDepsFor: func(dataStore *store.Store) shared.ApprovalDeps {
			return shared.ApprovalDeps{
				Enabled:      true,
				Store:        dataStore,
				Registry:     registry,
				Logger:       testLogger(),
				PollInterval: 100 * time.Millisecond,
			}
		},
	})

	client, outcome := fixture.login(t, fixtureUser, fixturePassword, fixtureDBEntry)
	require.True(t, outcome.Acked)

	const sql = "DELETE FROM employés WHERE id = 1"

	require.NoError(t, client.pkt.WriteMessage(packetTypeSQLBatch, sqlBatchPayload(sql)))

	// The reply is read on its own goroutine, because the point of the test is
	// that the client simply waits: no timeout, no torn-down connection.
	replies := make(chan []byte, 1)

	go func() {
		_, payload, err := client.pkt.ReadMessage()
		if err != nil {
			close(replies)

			return
		}

		replies <- payload
	}()

	// The statement is persisted as pending while it hangs, and nothing has
	// reached the upstream.
	pendingUID := uuid.Nil

	require.Eventually(t, func() bool {
		queries, err := fixture.store.ListQueries(context.Background(), store.QueryFilter{Limit: 10})
		if err != nil || len(queries) != 1 || queries[0].ApprovalStatus == nil {
			return false
		}

		pendingUID = queries[0].UID

		return *queries[0].ApprovalStatus == store.ApprovalPending
	}, 15*time.Second, 50*time.Millisecond)

	assert.Empty(t, fixture.fake.receivedRequests(),
		"a parked statement must not reach the upstream before it is approved")

	// Still parked a moment later: a hold has no timeout, and the client has
	// not been disconnected.
	time.Sleep(500 * time.Millisecond)
	assert.Empty(t, fixture.fake.receivedRequests())

	select {
	case <-replies:
		t.Fatal("the client was answered while the statement was still parked")
	default:
	}

	// A human approves.
	approver := fixture.user.UID
	require.True(t, registry.Resolve(approval.Decision{
		QueryUID: pendingUID,
		Status:   store.ApprovalApproved,
		By:       &approver,
		ByName:   "an approver",
		At:       time.Now(),
	}))

	select {
	case reply, ok := <-replies:
		require.True(t, ok, "the client connection was torn down instead of being answered")
		assert.Equal(t, buildDoneToken(0, 1), reply, "the released statement's answer must reach the client")
	case <-time.After(15 * time.Second):
		t.Fatal("the released statement never reached the client")
	}

	require.Eventually(t, func() bool { return len(fixture.fake.receivedRequests()) == 1 },
		10*time.Second, 20*time.Millisecond)
}

// TestRPCEnforcesEveryStatementCandidate is the hardening for a disagreement
// dbbat must not be able to have with the server.
//
// SQL Server accepts these system procedures both positionally and by name. A
// client that could get dbbat to validate the *named* @stmt while the server
// ran the *positional* one (or the reverse) would have a read_only bypass, so
// every candidate is enforced when a request is anything but the plain
// positional form.
func TestRPCEnforcesEveryStatementCandidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		payload []byte
	}{
		{
			name: "a write in the positional slot, a read named @stmt",
			payload: rpcByID(spExecuteSQL,
				nvarcharMaxParam("", "DELETE FROM t"),
				nvarcharMaxParam("@stmt", "SELECT 1")),
		},
		{
			name: "a read in the positional slot, a write named @stmt",
			payload: rpcByID(spExecuteSQL,
				nvarcharMaxParam("", "SELECT 1"),
				nvarcharMaxParam("@stmt", "DELETE FROM t")),
		},
		{
			name: "a write named @tsql",
			payload: rpcByID(spExecuteSQL,
				nvarcharMaxParam("@tsql", "DROP TABLE t"),
				nvarcharMaxParam("", "SELECT 1")),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			session := sessionWithGrant(t, grantWithControls(store.ControlReadOnly, store.ControlBlockDDL))

			requests, err := parseRPC(tc.payload)
			require.NoError(t, err)

			st := session.describeRPC(requests)

			got := st.refusal
			if got == nil {
				got = session.validate(st)
			}

			require.Error(t, got, "whichever candidate the server would run, dbbat must have checked it")
		})
	}
}

// TestBulkLoadIsRefusedUnderRestrictiveGrants covers the message that carries
// bulk-copy rows. The INSERT BULK statement is the primary gate; this is the
// belt to those braces, so no ordering trick delivers rows to the upstream.
func TestBulkLoadIsRefusedUnderRestrictiveGrants(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		controls []string
		wantErr  error
	}{
		{name: "read_only", controls: []string{store.ControlReadOnly}, wantErr: shared.ErrReadOnlyViolation},
		{name: "block_copy", controls: []string{store.ControlBlockCopy}, wantErr: ErrBulkCopyBlocked},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fixture := newAuthFixtureWith(t, fixtureOptions{
				upstreamEncryption: encryptNotSup,
				sslMode:            "disable",
				controls:           tc.controls,
			})

			client, outcome := fixture.login(t, fixtureUser, fixturePassword, fixtureDBEntry)
			require.True(t, outcome.Acked)

			reply := fixture.sendAndRead(t, client, packetTypeBulkLoad, []byte{0x01, 0x02, 0x03})
			assert.Contains(t, firstErrorToken(t, reply).Message, tc.wantErr.Error())

			assert.Empty(t, fixture.fake.receivedRequests(),
				"bulk-copy rows must never reach the upstream under this grant")
		})
	}
}
