package mssql

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

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

	// And a statement the pattern does *not* match sails straight through,
	// unheld. This is what proves the hold above was the pattern matching and
	// not a pattern that matches everything — which is what `(?i)^DELETE`
	// decayed into for as long as the store split it in two on read-back
	// (see internal/store/array.go).
	// The reader goroutine above is done (it reads exactly one message), so
	// this reads the reply inline — with a deadline, since being held would
	// otherwise hang the test rather than fail it.
	require.NoError(t, client.conn.SetDeadline(time.Now().Add(15*time.Second)))
	require.NoError(t, client.pkt.WriteMessage(packetTypeSQLBatch, sqlBatchPayload("SELECT 1")))

	_, unheld, err := client.pkt.ReadMessage()
	require.NoError(t, err, "a statement missing the approval pattern was held anyway")
	assert.Equal(t, buildDoneToken(0, 1), unheld)

	require.Eventually(t, func() bool { return len(fixture.fake.receivedRequests()) == 2 },
		10*time.Second, 20*time.Millisecond)
}

// TestAttentionCancelsAStatementParkedOnAnApprovalHold is the whole point of
// the split reader: a client that gives up on a statement waiting for a human
// gets its cancel honored immediately, instead of hanging until it drops the
// socket.
//
// Four things have to hold at once, and the third is a security property: the
// cancel must not be a way to get the statement past the gate.
func TestAttentionCancelsAStatementParkedOnAnApprovalHold(t *testing.T) {
	t.Parallel()

	registry := approval.NewRegistry()

	fixture := newAuthFixtureWith(t, fixtureOptions{
		upstreamEncryption: encryptNotSup,
		sslMode:            "disable",
		// `(?i)^DELETE`, like the tests around this one — step (d) below needs
		// the trailing SELECT to genuinely *miss* the pattern, which it only
		// does because the leading `(?i)` now survives the store round-trip
		// (it used to come back split as `(?i)` + `^DELETE`, and `(?i)` alone
		// matches everything). See internal/store/array.go.
		approvalPatterns: []string{`(?i)^DELETE`},
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

	require.NoError(t, client.pkt.WriteMessage(packetTypeSQLBatch,
		sqlBatchPayload("DELETE FROM employés WHERE id = 1")))

	replies := make(chan []byte, 2)

	go func() {
		for {
			_, payload, err := client.pkt.ReadMessage()
			if err != nil {
				close(replies)

				return
			}

			replies <- payload
		}
	}()

	pendingUID := waitForPendingHold(t, fixture, uuid.Nil)

	// The client's query timeout fires: a TDS cancel, on the session's own
	// socket, while the session goroutine is parked on a human.
	require.NoError(t, client.pkt.WriteMessage(packetTypeAttention, nil))

	// (a) The client is answered promptly, with the acknowledgement TDS says a
	// cancel gets — a DONE carrying DONE_ATTN. A driver reads for exactly this
	// before handing context.Canceled back to its caller.
	select {
	case reply, ok := <-replies:
		require.True(t, ok, "the connection was torn down instead of acknowledging the cancel")
		assert.Equal(t, buildAttentionAck(), reply)
	case <-time.After(15 * time.Second):
		t.Fatal("the cancel was never acknowledged")
	}

	// (b) The hold is settled, not left haunting /queries/pending, and it is
	// recorded as abandoned with the client cancel as its reason.
	require.Eventually(t, func() bool {
		queries, err := fixture.store.ListQueries(context.Background(), store.QueryFilter{Limit: 10})
		if err != nil || len(queries) != 1 || queries[0].ApprovalStatus == nil {
			return false
		}

		return *queries[0].ApprovalStatus == store.ApprovalAbandoned
	}, 15*time.Second, 50*time.Millisecond)

	queries, err := fixture.store.ListQueries(context.Background(), store.QueryFilter{Limit: 10})
	require.NoError(t, err)
	require.Len(t, queries, 1)
	assert.Equal(t, pendingUID, queries[0].UID, "the cancel must settle the row the hold created")
	require.NotNil(t, queries[0].ResolutionReason)
	assert.Equal(t, attentionCancelReason, *queries[0].ResolutionReason)

	// (c) The security property: a canceled statement is canceled, never
	// released. Neither it nor the ATTENTION itself reached the upstream — the
	// latter matters because a stray cancel would abort whatever ran next.
	assert.Empty(t, fixture.fake.receivedRequests(),
		"a canceled statement must never reach the upstream")

	// (d) The connection survives it, which is what separates a cancel from a
	// refusal in TDS.
	require.NoError(t, client.pkt.WriteMessage(packetTypeSQLBatch, sqlBatchPayload("SELECT 1")))

	select {
	case reply, ok := <-replies:
		require.True(t, ok, "the connection did not survive the cancel")
		assert.Equal(t, buildDoneToken(0, 1), reply)
	case <-time.After(15 * time.Second):
		t.Fatal("the session stopped serving after the cancel")
	}

	require.Eventually(t, func() bool { return len(fixture.fake.receivedRequests()) == 1 },
		10*time.Second, 20*time.Millisecond)
	assert.Equal(t, packetTypeSQLBatch, fixture.fake.receivedRequests()[0].msgType)
}

// TestAttentionCancelsAHoldOnAnEncryptedClientLeg is the case that decided the
// design. A byte-level watcher parked below TLS — what the other four proxies
// use — sees TLS records and could never recognize the cancel inside them, so
// the SQL Server proxy reads its client leg above TLS instead. This asserts the
// cancel really does work when the whole session is inside the encapsulated TLS
// handshake, not just on a plaintext link.
func TestAttentionCancelsAHoldOnAnEncryptedClientLeg(t *testing.T) {
	t.Parallel()

	registry := approval.NewRegistry()

	fixture := newAuthFixtureWith(t, fixtureOptions{
		upstreamEncryption: encryptNotSup,
		sslMode:            "disable",
		clientTLS:          true,
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

	client, outcome := fixture.loginEncrypted(t, fixtureUser, fixturePassword, fixtureDBEntry)
	require.True(t, outcome.Acked)

	// The hold has no timeout, so the client must be allowed to sit on it for
	// longer than dialTestClient's default deadline.
	require.NoError(t, client.conn.SetDeadline(time.Now().Add(60*time.Second)))

	require.NoError(t, client.pkt.WriteMessage(packetTypeSQLBatch,
		sqlBatchPayload("DELETE FROM employés WHERE id = 1")))

	replies := make(chan []byte, 1)

	go func() {
		_, payload, err := client.pkt.ReadMessage()
		if err != nil {
			close(replies)

			return
		}

		replies <- payload
	}()

	pendingUID := waitForPendingHold(t, fixture, uuid.Nil)

	require.NoError(t, client.pkt.WriteMessage(packetTypeAttention, nil))

	select {
	case reply, ok := <-replies:
		require.True(t, ok, "the connection was torn down instead of acknowledging the cancel")
		assert.Equal(t, buildAttentionAck(), reply)
	case <-time.After(15 * time.Second):
		t.Fatal("the cancel was never acknowledged through TLS")
	}

	require.Eventually(t, func() bool {
		query, err := fixture.store.GetQuery(context.Background(), pendingUID)

		return err == nil && query.ApprovalStatus != nil && *query.ApprovalStatus == store.ApprovalAbandoned
	}, 15*time.Second, 50*time.Millisecond)

	assert.Empty(t, fixture.fake.receivedRequests(),
		"a canceled statement must never reach the upstream")
}

// TestAttentionLosingToAnApproverTravelsUpstream pins the other side of the
// race. The registry arbitrates; the cancel that loses must not pretend it won,
// or the statement would be answered as canceled while it runs upstream anyway.
func TestAttentionLosingToAnApproverTravelsUpstream(t *testing.T) {
	t.Parallel()

	registry := approval.NewRegistry()
	sess := &session{server: &Server{approvalDeps: shared.ApprovalDeps{Registry: registry}}}

	uid := uuid.New()
	_, release := registry.Register(uid)

	defer release()

	sess.setHeldQuery(uid)

	// An approver gets there first.
	require.True(t, registry.Resolve(approval.Decision{QueryUID: uid, Status: store.ApprovalApproved}))

	assert.False(t, sess.cancelHeldStatement(),
		"a cancel that lost the race must let the ATTENTION travel upstream")
	assert.False(t, sess.takeAttentionAck(),
		"and must not leave the client owed a cancel acknowledgement")
}

// TestAttentionRacingTheHoldRegistration covers the window the reader goroutine
// cannot see into on its own: the gate has decided to park the statement but
// has not published its uid yet. A cancel landing there must still be applied,
// not forwarded upstream against a statement that never ran.
func TestAttentionRacingTheHoldRegistration(t *testing.T) {
	t.Parallel()

	registry := approval.NewRegistry()
	sess := &session{server: &Server{approvalDeps: shared.ApprovalDeps{Registry: registry}}}

	sess.armHoldCancel()

	require.True(t, sess.cancelHeldStatement(),
		"a cancel arriving while the hold is being registered must be consumed, not forwarded")

	// The gate now publishes the uid, and the cancel catches up with it.
	uid := uuid.New()
	decisions, release := registry.Register(uid)

	defer release()

	sess.setHeldQuery(uid)

	select {
	case d := <-decisions:
		assert.Equal(t, store.ApprovalAbandoned, d.Status)
		assert.Equal(t, attentionCancelReason, d.Reason)
	default:
		t.Fatal("the hold was never canceled")
	}

	assert.True(t, sess.takeAttentionAck(), "the client is owed a cancel acknowledgement")
	assert.False(t, sess.takeAttentionAck(), "and only one")
}

// TestAttentionAfterTheHoldEndsTravelsUpstream closes the sliver between the
// gate clearing the held uid and holdIfNeeded disarming: a cancel landing there
// must not be swallowed against a statement that was approved and is about to
// run, or the client would wait forever for an acknowledgement nothing owes it.
func TestAttentionAfterTheHoldEndsTravelsUpstream(t *testing.T) {
	t.Parallel()

	sess := &session{server: &Server{approvalDeps: shared.ApprovalDeps{Registry: approval.NewRegistry()}}}

	sess.armHoldCancel()
	sess.setHeldQuery(uuid.New())

	// The gate resolves the hold and clears the uid, which is what ends the
	// window — holdIfNeeded's own defer has not run yet.
	sess.setHeldQuery(uuid.Nil)

	assert.False(t, sess.cancelHeldStatement(),
		"a cancel arriving after the hold ended must travel upstream")
	assert.False(t, sess.takeAttentionAck())
}

// TestAttentionLosingToAnApproverInsideTheArmWindow is the deterministic half of
// the regression: the cancel is consumed in the arm window — before any uid
// exists to arbitrate it — and *then* loses to a human approver.
//
// The arm window used to mark the client as owed a DONE_ATTN the moment it
// swallowed the ATTENTION, before the catch-up Resolve had said who won. When
// that Resolve lost, the statement ran and the session kept a spurious owed
// acknowledgement, which the next refused statement would take — answering it
// as "cancelled" instead of with its real error.
func TestAttentionLosingToAnApproverInsideTheArmWindow(t *testing.T) {
	t.Parallel()

	registry := approval.NewRegistry()
	sess := &session{server: &Server{approvalDeps: shared.ApprovalDeps{Registry: registry}}}

	uid := uuid.New()
	_, release := registry.Register(uid)

	defer release()

	// The gate has registered the hold but has not published its uid yet, and
	// the client's cancel lands in exactly that sliver.
	sess.armHoldCancel()
	require.True(t, sess.cancelHeldStatement(),
		"a cancel arriving while the hold is being registered must be consumed, not forwarded")

	// A human approves inside the window: the statement is going to run.
	require.True(t, registry.Resolve(approval.Decision{QueryUID: uid, Status: store.ApprovalApproved}))

	// The gate publishes the uid and the deferred cancel catches up — too late.
	sess.setHeldQuery(uid)

	assert.False(t, sess.takeAttentionAck(),
		"a cancel that lost must not leave the client owed an acknowledgement the next refusal would take")
	assert.True(t, sess.takeAttentionUpstream(),
		"the consumed ATTENTION must be put back on the upstream leg, behind the statement it lost to")
	assert.False(t, sess.takeAttentionUpstream(), "and only once")
}

// TestArmWindowCancelWithNoUidIsAcknowledged covers the path where the gate
// never publishes a uid at all — a failed pending insert, which fails closed.
// Nothing arbitrated the cancel and nothing ran, so the ATTENTION the reader
// consumed still owes the client its acknowledgement.
func TestArmWindowCancelWithNoUidIsAcknowledged(t *testing.T) {
	t.Parallel()

	sess := &session{server: &Server{approvalDeps: shared.ApprovalDeps{Registry: approval.NewRegistry()}}}

	sess.armHoldCancel()
	require.True(t, sess.cancelHeldStatement())

	sess.disarmHoldCancel()

	assert.True(t, sess.takeAttentionAck(), "the swallowed cancel must still be acknowledged")
	assert.False(t, sess.takeAttentionUpstream(), "nothing ran, so nothing goes upstream")
}

// TestArmWindowCancelLosingLeavesTheNextRefusalIntact is the same race end to
// end, over a real client socket, and asserts the consequence the internal
// flags only stand for: the *next* statement the grant refuses must be answered
// with its real error, not with the bare DONE_ATTN a stale acknowledgement
// would turn it into.
//
// The window is a handful of instructions wide, so the gate's HoldRegistered
// seam holds it open while the client's cancel arrives and a human approves.
func TestArmWindowCancelLosingLeavesTheNextRefusalIntact(t *testing.T) {
	t.Parallel()

	registry := approval.NewRegistry()
	armed := make(chan uuid.UUID, 1)
	proceed := make(chan struct{})

	fixture := newAuthFixtureWith(t, fixtureOptions{
		upstreamEncryption: encryptNotSup,
		sslMode:            "disable",
		// block_ddl is what makes the trailing DROP refusable; it must not be
		// read_only, or the DELETE would never reach the approval gate.
		controls:         []string{store.ControlBlockDDL},
		approvalPatterns: []string{`(?i)^DELETE`},
		approvalDepsFor: func(dataStore *store.Store) shared.ApprovalDeps {
			return shared.ApprovalDeps{
				Enabled:      true,
				Store:        dataStore,
				Registry:     registry,
				Logger:       testLogger(),
				PollInterval: 100 * time.Millisecond,
				HoldRegistered: func(uid uuid.UUID) {
					armed <- uid
					<-proceed
				},
			}
		},
	})

	client, outcome := fixture.login(t, fixtureUser, fixturePassword, fixtureDBEntry)
	require.True(t, outcome.Acked)

	require.NoError(t, client.conn.SetDeadline(time.Now().Add(60*time.Second)))

	require.NoError(t, client.pkt.WriteMessage(packetTypeSQLBatch,
		sqlBatchPayload("DELETE FROM employés WHERE id = 1")))

	// The hold is registered and the session goroutine is parked inside the arm
	// window: the uid exists in the registry but has not been published to the
	// cancel path yet.
	var pendingUID uuid.UUID

	select {
	case pendingUID = <-armed:
	case <-time.After(30 * time.Second):
		t.Fatal("the hold was never registered")
	}

	// The client's query timeout fires inside that window. The reader goroutine
	// is the only part of the session still running, and it is blocked on this
	// very socket, so it picks the cancel up while the pump stays parked; the
	// settle below is slack for the scheduler, not for the protocol.
	require.NoError(t, client.pkt.WriteMessage(packetTypeAttention, nil))
	time.Sleep(500 * time.Millisecond)

	// And a human approves in the same sliver, which is what the cancel loses to.
	approver := fixture.user.UID
	require.True(t, registry.Resolve(approval.Decision{
		QueryUID: pendingUID,
		Status:   store.ApprovalApproved,
		By:       &approver,
		ByName:   "an approver",
		At:       time.Now(),
	}))

	close(proceed)

	// The approved statement runs, and the cancel that lost is put back on the
	// upstream leg behind it — the fake answers both.
	assert.Equal(t, buildDoneToken(0, 1), client.readReply(t), "the released statement's answer must reach the client")
	assert.Equal(t, buildDoneToken(0, 1), client.readReply(t), "the re-emitted cancel is answered too")

	require.Eventually(t, func() bool { return len(fixture.fake.receivedRequests()) == 2 },
		15*time.Second, 20*time.Millisecond)

	upstreamSaw := fixture.fake.receivedRequests()
	assert.Equal(t, packetTypeSQLBatch, upstreamSaw[0].msgType, "the approved statement must reach the upstream")
	assert.Equal(t, packetTypeAttention, upstreamSaw[1].msgType,
		"a cancel that lost its race must still reach the upstream, behind the statement it lost to")

	// The point of the whole fix: nothing owes the client a DONE_ATTN anymore,
	// so the next refusal is answered as a refusal. firstErrorToken fails hard
	// on a bare DONE_ATTN.
	refusal := fixture.sendAndRead(t, client, packetTypeSQLBatch, sqlBatchPayload("DROP TABLE t"))
	assert.Contains(t, firstErrorToken(t, refusal).Message, "DDL",
		"a stale cancel acknowledgement must not answer the next refused statement")

	assert.Len(t, fixture.fake.receivedRequests(), 2, "the refused DDL must never reach the upstream")
}

// waitForPendingHold waits until a statement other than skip is parked, and
// returns its uid.
func waitForPendingHold(t *testing.T, fixture *authFixture, skip uuid.UUID) uuid.UUID {
	t.Helper()

	pendingUID := uuid.Nil

	require.Eventually(t, func() bool {
		queries, err := fixture.store.ListQueries(context.Background(), store.QueryFilter{Limit: 10})
		if err != nil {
			return false
		}

		for _, query := range queries {
			if query.UID == skip || query.ApprovalStatus == nil {
				continue
			}

			if *query.ApprovalStatus == store.ApprovalPending {
				pendingUID = query.UID

				return true
			}
		}

		return false
	}, 15*time.Second, 50*time.Millisecond)

	return pendingUID
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

// TestSQLBatchRefusesAPrefixEvasion is the same fail-closed rule from the
// enforcement side: a malformed ALL_HEADERS block must refuse the statement
// rather than let its bytes become a prefix that a prefix-based control no
// longer matches.
func TestSQLBatchRefusesAPrefixEvasion(t *testing.T) {
	t.Parallel()

	// A well-formed-looking block with a header type MS-TDS does not define,
	// followed by a real write.
	payload := append(
		[]byte{0x16, 0x00, 0x00, 0x00, 0x12, 0x00, 0x00, 0x00, 0x63, 0x00},
		make([]byte, 12)...)
	payload = append(payload, stringToUCS2("DELETE FROM t")...)

	_, err := parseSQLBatch(payload)
	require.ErrorIs(t, err, ErrMalformedRequest)

	// And, had it been skipped rather than refused, the prefix really would
	// have defeated the control — which is why this is fail-closed.
	assert.False(t, shared.IsWriteQuery(ucs2ToString(payload)),
		"the garbage prefix is exactly what stops read_only from matching")
}

// TestCursorExecuteResolvesItsHandle covers the parameter order of
// sp_cursorexecute: the handle is the first parameter, the cursor OUT the
// second.
func TestCursorExecuteResolvesItsHandle(t *testing.T) {
	t.Parallel()

	session := sessionWithGrant(t, grantWithControls(store.ControlReadOnly))
	session.rememberPrepared(41, "SELECT * FROM t")

	requests, err := parseRPC(rpcByID(spCursorExecute,
		intNParam("@handle", 41, 0), intNParam("@cursor", 0, 0x01)))
	require.NoError(t, err)

	st := session.describeRPC(requests)
	require.NoError(t, st.refusal, "a cursor re-execution of a known handle must not fail closed")
	require.NoError(t, session.validate(st))
	assert.Equal(t, "SELECT * FROM t", st.text)
}

// TestPreparedExecuteIsLoggedAsItsStatement is the observability half of the
// prepared-statement path, and the reason it matters is the approval gate:
// holds are matched against this text, so logging "EXEC sp_execute 41" would
// make the four-eyes rule silently stop applying to any client that uses
// prepared statements.
func TestPreparedExecuteIsLoggedAsItsStatement(t *testing.T) {
	t.Parallel()

	session := sessionWithGrant(t, grantWithControls())
	session.rememberPrepared(41, "DELETE FROM employés WHERE id = @id")

	requests, err := parseRPC(rpcByID(spExecute, intNParam("@handle", 41, 0), intNParam("@id", 2, 0)))
	require.NoError(t, err)

	st := session.describeRPC(requests)

	assert.Equal(t, "DELETE FROM employés WHERE id = @id", st.text)
	assert.Equal(t, []string{"DELETE FROM employés WHERE id = @id"}, st.enforce)
	require.NotNil(t, st.params)
	assert.Equal(t, []string{"@handle=41", "@id=2"}, st.params.Values)
}

// TestUnprepareIsNotLoggedAsItsStatement: releasing a handle is not running the
// statement again, so it must not read like it.
func TestUnprepareIsNotLoggedAsItsStatement(t *testing.T) {
	t.Parallel()

	session := sessionWithGrant(t, grantWithControls(store.ControlReadOnly))
	session.rememberPrepared(41, "SELECT 1")

	requests, err := parseRPC(rpcByID(spUnprepare, intNParam("@handle", 41, 0)))
	require.NoError(t, err)

	st := session.describeRPC(requests)
	require.NoError(t, st.refusal)
	assert.Equal(t, "EXEC sp_unprepare @handle = 41", st.text)
	assert.Empty(t, st.enforce)
}

// TestApprovalHoldMatchesAPreparedStatement is the end-to-end version of the
// same property: a pattern that fires on the statement must still fire when the
// client runs it through a prepared handle.
func TestApprovalHoldMatchesAPreparedStatement(t *testing.T) {
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

	// The upstream answers a prepare with the handle it allocated.
	handleReturn := appendUint16([]byte{tokenReturnValue}, 1)
	handleReturn = writeBVarchar(handleReturn, "@handle")
	handleReturn = append(handleReturn, 0x01)
	handleReturn = appendUint32(handleReturn, 0)
	handleReturn = appendUint16(handleReturn, 0)
	handleReturn = append(handleReturn, typeIntN, 4, 4)
	handleReturn = appendUint32(handleReturn, 41)
	handleReturn = append(handleReturn, buildDoneToken(doneCount, 0)...)

	fixture.fake.mu.Lock()
	fixture.fake.batchResponse = handleReturn
	fixture.fake.mu.Unlock()

	client, outcome := fixture.login(t, fixtureUser, fixturePassword, fixtureDBEntry)
	require.True(t, outcome.Acked)

	const sql = "DELETE FROM employés WHERE id = @id"

	// Preparing it holds too, so release that one first.
	preparing := make(chan []byte, 1)

	require.NoError(t, client.pkt.WriteMessage(packetTypeRPC, rpcByID(spPrepare,
		intNParam("@handle", 0, 0x01),
		nvarcharMaxParam("", "@id int"),
		nvarcharMaxParam("", sql),
		intNParam("", 1, 0))))

	go func() {
		_, payload, err := client.pkt.ReadMessage()
		if err != nil {
			close(preparing)

			return
		}

		preparing <- payload
	}()

	approver := fixture.user.UID
	prepared := releaseHold(t, fixture, registry, approver, uuid.Nil)
	assert.Equal(t, sql, prepared.SQLText)

	select {
	case _, ok := <-preparing:
		require.True(t, ok, "the prepare was never answered")
	case <-time.After(15 * time.Second):
		t.Fatal("the released prepare never reached the client")
	}

	// Now execute the handle. Nothing in *this* message says DELETE — only the
	// statement it resolves to does.
	executing := make(chan []byte, 1)

	require.NoError(t, client.pkt.WriteMessage(packetTypeRPC,
		rpcByID(spExecute, intNParam("@handle", 41, 0), intNParam("@id", 2, 0))))

	go func() {
		_, payload, err := client.pkt.ReadMessage()
		if err != nil {
			close(executing)

			return
		}

		executing <- payload
	}()

	held := releaseHold(t, fixture, registry, approver, prepared.UID)
	assert.Equal(t, sql, held.SQLText,
		"the hold must be recorded against the statement, not against EXEC sp_execute")

	select {
	case _, ok := <-executing:
		require.True(t, ok, "the execute was never answered")
	case <-time.After(15 * time.Second):
		t.Fatal("the released execute never reached the client")
	}

	// A statement the pattern does *not* match is answered without a hold.
	// That is what separates "the pattern matched" from "the pattern matches
	// everything" — which is what `(?i)^DELETE` decayed into for as long as
	// the store split it on read-back (see internal/store/array.go).
	require.NoError(t, client.conn.SetDeadline(time.Now().Add(15*time.Second)))
	require.NoError(t, client.pkt.WriteMessage(packetTypeRPC,
		rpcByID(spExecuteSQL, nvarcharMaxParam("", "SELECT 1"))))

	_, _, err := client.pkt.ReadMessage()
	require.NoError(t, err, "a statement missing the approval pattern was held anyway")
}

// releaseHold waits for a statement other than skip to be parked, approves it,
// and returns the row it was parked as. skip is the previously released hold,
// whose row can still read as pending for a moment after it resolves.
func releaseHold(
	t *testing.T, fixture *authFixture, registry *approval.Registry, approver, skip uuid.UUID,
) store.Query {
	t.Helper()

	var pending store.Query

	require.Eventually(t, func() bool {
		queries, err := fixture.store.ListQueries(context.Background(), store.QueryFilter{Limit: 50})
		if err != nil {
			return false
		}

		for _, query := range queries {
			if query.UID == skip || query.ApprovalStatus == nil {
				continue
			}

			if *query.ApprovalStatus == store.ApprovalPending {
				pending = query

				return true
			}
		}

		return false
	}, 15*time.Second, 50*time.Millisecond)

	require.True(t, registry.Resolve(approval.Decision{
		QueryUID: pending.UID,
		Status:   store.ApprovalApproved,
		By:       &approver,
		ByName:   "an approver",
		At:       time.Now(),
	}))

	return pending
}

// TestTruncateSQLCutsOnARuneBoundary: the limit is in bytes but the text is
// UTF-8 decoded from UCS-2, so a blind cut can land inside a rune — and the
// invalid string that results fails the query insert outright, losing the audit
// row for exactly the statement somebody would want to see.
func TestTruncateSQLCutsOnARuneBoundary(t *testing.T) {
	t.Parallel()

	for pad := range 8 {
		sql := strings.Repeat("a", maxSQLTextLen-pad) + strings.Repeat("é", 16)

		truncated := truncateSQL(sql)

		assert.True(t, utf8.ValidString(truncated), "padding %d produced invalid UTF-8", pad)
		assert.LessOrEqual(t, len(truncated), maxSQLTextLen)
	}

	assert.Equal(t, "SELECT 1", truncateSQL("SELECT 1"))
}
