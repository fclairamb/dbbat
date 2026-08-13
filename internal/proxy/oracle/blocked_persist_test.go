package oracle

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/fclairamb/dbbat/internal/cache"
	"github.com/fclairamb/dbbat/internal/proxy/shared"
	"github.com/fclairamb/dbbat/internal/store"
)

// awaitCreated waits for the asynchronous insert of a blocked-statement row.
func (r *recordingCompletionStore) awaitCreated(t *testing.T) *store.Query {
	t.Helper()

	select {
	case got := <-r.created:
		return got
	case <-time.After(5 * time.Second):
		t.Fatal("the refused statement was never written to the store")

		return nil
	}
}

// assertNoFurtherCreate fails if a second row shows up for one refusal.
func (r *recordingCompletionStore) assertNoFurtherCreate(t *testing.T) {
	t.Helper()

	select {
	case extra := <-r.created:
		t.Fatalf("a refused statement must write exactly one row, got a second: %q", extra.SQLText)
	case <-time.After(250 * time.Millisecond):
	}
}

// TestBlockedStatements_ArePersisted is the Oracle half of spec
// 2026-08-09-log-blocked-statements-pg-oracle: a statement refused by
// read_only or block_ddl used to leave nothing behind but an slog WARN. It now
// writes a queries row carrying the refusal as `error`, with duration_ms and
// rows_affected at 0 because the statement never reached upstream.
//
// Every statement-carrying TTC op is covered, because they refuse
// independently: legacy OALL8, the v315+ piggyback exec, the JDBC thin
// driver's func=0x11 exec, and a re-execution re-gated against its cursor.
func TestBlockedStatements_ArePersisted(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		controls []string
		sql      string
		wantErr  string
		payload  func(sql string) []byte
	}{
		{
			name:     "OALL8 write under read_only",
			controls: []string{store.ControlReadOnly},
			sql:      "INSERT INTO emp (id) VALUES (1)",
			wantErr:  "read-only",
			payload:  func(sql string) []byte { return buildOALL8(sql, nil, 7) },
		},
		{
			name:     "OALL8 CREATE TABLE under block_ddl",
			controls: []string{store.ControlBlockDDL},
			sql:      "CREATE TABLE blocked_ddl (id NUMBER)",
			wantErr:  "DDL",
			payload:  func(sql string) []byte { return buildOALL8(sql, nil, 7) },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := newTestSession(&store.Grant{Definition: &store.GrantDefinition{Controls: tt.controls}})
			recorder := newRecordingCompletionStore()
			s.completionStore = recorder

			require.Error(t, s.handleOALL8(tt.payload(tt.sql)), "the statement must be refused")

			got := recorder.awaitCreated(t)
			assertBlockedOracleRow(t, got, tt.sql, tt.wantErr)
			assert.Equal(t, s.connectionUID, got.ConnectionID,
				"a refusal row must still be attributed to the connection")

			// A refused statement is not in flight: nothing is left pending
			// that a later response could complete into a second row.
			assert.Nil(t, s.tracker.pendingQuery)
			recorder.assertNoFurtherCreate(t)
		})
	}
}

// TestBlockedCursorReexecution_IsPersisted covers the path a client takes when
// it re-runs a statement it already parsed: the SQL rides no frame of its own,
// so the refusal has to be recorded against the SQL the cursor holds.
func TestBlockedCursorReexecution_IsPersisted(t *testing.T) {
	t.Parallel()

	const sql = "UPDATE emp SET id = 2"

	s := newTestSession(&store.Grant{Definition: &store.GrantDefinition{}})
	recorder := newRecordingCompletionStore()
	s.completionStore = recorder

	// Parse it while the grant still allows writes, so the cursor is tracked.
	require.NoError(t, s.handleOALL8(buildOALL8(sql, nil, 11)))

	// Drain the row the successful parse inserted (persistQueryRecord goes
	// through s.store, which is nil here, so nothing is queued — but be
	// explicit about starting from an empty channel).
	require.Empty(t, recorder.created)

	// Narrow the grant, then re-execute the very same cursor.
	s.grant = &store.Grant{Definition: &store.GrantDefinition{Controls: []string{store.ControlReadOnly}}}

	require.Error(t, s.handleCursorReexec(11), "the re-execution must be refused")

	got := recorder.awaitCreated(t)
	assertBlockedOracleRow(t, got, sql, "read-only")
	recorder.assertNoFurtherCreate(t)
}

// exhaustedGrant returns a grant whose max_query_counts is already spent.
func exhaustedGrant(controls ...string) *store.Grant {
	maxQueries := int64(3)

	return &store.Grant{
		QueryCount: 3,
		Definition: &store.GrantDefinition{MaxQueryCounts: &maxQueries, Controls: controls},
	}
}

// piggybackExecSelect1 is a real captured piggyback execute-with-SQL frame
// (func=0x03, sub=0x5e) carrying SELECT 1 FROM DUAL — the same fixture
// TestDecodePiggybackExecSQL decodes. There is no builder for this op, and a
// hand-rolled one would only prove the builder agrees with itself.
func piggybackExecSelect1() []byte {
	payload, _ := hexDecode("035e030280610001011201010d0000000102047fffffff000000000000000000000" +
		"0010000000000000000000000000000001253454c45435420312046524f4d204455414c0101000000000000010100")

	return payload
}

// TestQuotaRefusals_ArePersisted is spec
// 2026-08-10-09-oracle-quota-refusals-not-recorded: a statement Oracle refused
// because the grant's quota was exhausted used to leave nothing behind but a
// log line, while a statement refused by read_only wrote a row. The quota check
// ran in gateStatement, ahead of the decode, so there was no SQL to record it
// against; it now runs inside each handler and refuses through the same
// recorder every control refusal uses.
//
// Every gated op is covered, because they decode independently: the three that
// carry their own SQL, and the three re-execution frames that borrow it from
// the cursor.
func TestQuotaRefusals_ArePersisted(t *testing.T) {
	t.Parallel()

	const sql = "SELECT 1 FROM DUAL"

	tests := []struct {
		name   string
		refuse func(s *session) error
	}{
		{
			name:   "legacy OALL8",
			refuse: func(s *session) error { return s.handleOALL8(buildOALL8(sql, nil, 7)) },
		},
		{
			name:   "v315+ piggyback exec",
			refuse: func(s *session) error { return s.handlePiggybackExec(piggybackExecSelect1()) },
		},
		{
			name:   "JDBC thin exec",
			refuse: func(s *session) error { return s.handleJDBCExec(buildJDBCExec(sql)) },
		},
		{
			name: "piggyback re-execution",
			refuse: func(s *session) error {
				s.tracker.cursors[6] = &trackedCursor{cursorID: 6, sql: sql, parsedAt: time.Now()}

				return s.handlePiggybackReexec(buildPiggybackReexec(6))
			},
		},
		{
			name: "SQL-less OALL8 re-execution",
			refuse: func(s *session) error {
				s.tracker.cursors[9] = &trackedCursor{cursorID: 9, sql: sql, parsedAt: time.Now()}

				return s.handleOALL8(buildOALL8Reexec(9))
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := newTestSession(exhaustedGrant())
			recorder := newRecordingCompletionStore()
			s.completionStore = recorder

			require.ErrorIs(t, tt.refuse(s), ErrQueryLimitExceed, "the statement must be refused")

			got := recorder.awaitCreated(t)
			assertBlockedOracleRow(t, got, sql, "query limit exceeded")
			assert.Equal(t, s.connectionUID, got.ConnectionID,
				"a refusal row must still be attributed to the connection")

			assert.Nil(t, s.tracker.pendingQuery,
				"a statement refused over quota must not be tracked as in flight")
			recorder.assertNoFurtherCreate(t)
		})
	}
}

// TestQuotaRefusalReasons_ArePersisted covers the rest of what checkQuotas
// refuses: the byte cap, and the two reasons the goal statement calls out
// explicitly — a grant that expired or was revoked mid-session. All four write
// the same row shape, because all four travel the same path.
func TestQuotaRefusalReasons_ArePersisted(t *testing.T) {
	t.Parallel()

	const sql = "SELECT 1 FROM DUAL"

	maxBytes := int64(1000)

	tests := []struct {
		name    string
		session func() *session
		wantErr error
		wantMsg string
	}{
		{
			name:    "max_query_counts spent",
			session: func() *session { return newTestSession(exhaustedGrant()) },
			wantErr: ErrQueryLimitExceed,
			wantMsg: "query limit exceeded",
		},
		{
			name: "max_bytes_transferred spent",
			session: func() *session {
				return newTestSession(&store.Grant{
					BytesTransferred: 1000,
					Definition:       &store.GrantDefinition{MaxBytesTransferred: &maxBytes},
				})
			},
			wantErr: ErrDataLimitExceed,
			wantMsg: "data transfer limit exceeded",
		},
		{
			name: "the grant expired mid-session",
			session: func() *session {
				return newTestSession(&store.Grant{
					ExpiresAt:  time.Now().Add(-time.Minute),
					Definition: &store.GrantDefinition{},
				})
			},
			wantErr: shared.ErrGrantExpired,
			wantMsg: "grant expired",
		},
		{
			name: "the grant was revoked mid-session",
			session: func() *session {
				grant := &store.Grant{
					UID:        uuid.New(),
					ExpiresAt:  time.Now().Add(time.Hour),
					Definition: &store.GrantDefinition{},
				}
				registry := cache.NewRevocationRegistry()

				s := newTestSession(grant)
				s.revocation = registry.Register(grant.UID)
				registry.Revoke(grant.UID)

				return s
			},
			wantErr: shared.ErrGrantRevoked,
			wantMsg: "grant revoked",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := tt.session()
			recorder := newRecordingCompletionStore()
			s.completionStore = recorder

			require.ErrorIs(t, s.handleOALL8(buildOALL8(sql, nil, 7)), tt.wantErr)

			assertBlockedOracleRow(t, recorder.awaitCreated(t), sql, tt.wantMsg)
			recorder.assertNoFurtherCreate(t)
		})
	}
}

// TestQuotaRefusal_DecodeFailureIsStillForwarded is the first of the three
// invariants the move had to preserve. Oracle's fail-behavior on an
// undecodable payload is forward-don't-block (the caveat in docs/approvals.md),
// and the quota check now sits *behind* the decode — so a frame dbbat cannot
// read must still travel untouched, exhausted grant or not, rather than
// becoming a refusal the client never used to get.
func TestQuotaRefusal_DecodeFailureIsStillForwarded(t *testing.T) {
	t.Parallel()

	// A well-formed OALL8 truncated mid-SQL: everything up to the length parses,
	// the text does not.
	truncatedOALL8 := buildOALL8("SELECT 1 FROM DUAL", nil, 7)
	truncatedOALL8 = truncatedOALL8[:20]

	tests := []struct {
		name    string
		forward func(s *session) error
	}{
		{
			name:    "legacy OALL8",
			forward: func(s *session) error { return s.handleOALL8(truncatedOALL8) },
		},
		{
			name:    "v315+ piggyback exec",
			forward: func(s *session) error { return s.handlePiggybackExec([]byte{0x03, 0x5e, 0x01}) },
		},
		{
			name:    "JDBC thin exec",
			forward: func(s *session) error { return s.handleJDBCExec([]byte{0x11, 0x69}) },
		},
		{
			name:    "piggyback re-execution",
			forward: func(s *session) error { return s.handlePiggybackReexec([]byte{0x03}) },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := newTestSession(exhaustedGrant())
			recorder := newRecordingCompletionStore()
			s.completionStore = recorder

			require.NoError(t, tt.forward(s),
				"an undecodable frame is forwarded, never turned into a quota refusal")
			recorder.assertNoFurtherCreate(t)
		})
	}
}

// TestQuotaRefusal_IsNeverParkedOnAHuman is the second invariant: the quota
// check has to stay ahead of the approval hold. A statement that matches an
// approval pattern *and* exceeds the quota is refused outright — parking it
// would ask a human to release something the grant can no longer pay for, and
// block the client until they answered.
func TestQuotaRefusal_IsNeverParkedOnAHuman(t *testing.T) {
	t.Parallel()

	s, fake, _ := gatedTestSession([]string{"(?i)^select"})

	recorder := newRecordingCompletionStore()
	s.completionStore = recorder

	maxQueries := int64(1)
	s.grant.QueryCount = 1
	s.grant.Definition.MaxQueryCounts = &maxQueries

	const sql = "SELECT 1 FROM DUAL"

	require.ErrorIs(t, s.handleOALL8(buildOALL8(sql, nil, 7)), ErrQueryLimitExceed)

	assertBlockedOracleRow(t, recorder.awaitCreated(t), sql, "query limit exceeded")
	assert.Empty(t, fake.held(), "an over-quota statement must never be parked on a human")

	// The pattern still holds a statement the grant can pay for, so the test is
	// not passing because the gate is simply inert.
	s.grant.QueryCount = 0

	go func() { _ = s.handleOALL8(buildOALL8(sql, nil, 8)) }()

	awaitHeld(t, fake, 1)
}

// TestQuotaRefusal_UnknownCursorAnswersFirst pins the ordering the spec
// resolved deliberately: on a re-execution, the cursor is resolved before the
// quota is consulted.
//
// A cursor dbbat never saw parsed keeps answering refuseUnknownCursor even when
// the grant is also exhausted — it is the more specific, fail-closed answer,
// and hiding it behind a quota error would make it harder to diagnose. It
// records nothing, because the statement it would have run is unknown. The
// permissive half of refuseUnknownCursor forwards, and there the quota still
// applies: an execution dbbat cannot identify is still an execution.
func TestQuotaRefusal_UnknownCursorAnswersFirst(t *testing.T) {
	t.Parallel()

	t.Run("a restrictive grant answers with the unknown cursor, not the quota", func(t *testing.T) {
		t.Parallel()

		s := newTestSession(exhaustedGrant(store.ControlReadOnly))
		recorder := newRecordingCompletionStore()
		s.completionStore = recorder

		require.ErrorIs(t, s.handlePiggybackReexec(buildPiggybackReexec(404)), ErrUnknownCursor)
		recorder.assertNoFurtherCreate(t)
	})

	t.Run("a permissive grant over quota is still refused", func(t *testing.T) {
		t.Parallel()

		s := newTestSession(exhaustedGrant())
		recorder := newRecordingCompletionStore()
		s.completionStore = recorder

		require.ErrorIs(t, s.handlePiggybackReexec(buildPiggybackReexec(404)), ErrQueryLimitExceed,
			"an untracked cursor must not become a way past the quota")
		recorder.assertNoFurtherCreate(t)

		// Under the cap the same re-execution is forwarded, WARN and all.
		s.grant.QueryCount = 0
		require.NoError(t, s.handlePiggybackReexec(buildPiggybackReexec(404)))
	})
}

// TestBlockedStatement_IsAnOrdinaryChainAppend runs a refusal through the real
// store rather than a recorder. Two things only a real store can prove: the
// row lands in `queries` attributed to the session's connection, and — because
// `queries` is HMAC-chained per connection — the refusal row is an ordinary
// chain append that `dbbat audit verify --queries` still walks cleanly.
//
// Both refusal reasons are exercised, a control and the quota, because they
// reach the recorder from different places in the handler.
func TestBlockedStatement_IsAnOrdinaryChainAppend(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	dataStore := newOracleTestStore(t)
	require.True(t, dataStore.ChainEnabled(), "the query chain must be on for this test to mean anything")

	user, err := dataStore.CreateUser(ctx, "blocked-oracle", "hash", []string{store.RoleConnector})
	require.NoError(t, err)

	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}

	db, err := dataStore.CreateServer(ctx, &store.Server{
		Name:         "blocked-oracle-db",
		Host:         "localhost",
		Port:         1521,
		DatabaseName: "ORCL",
		Username:     "u",
		Password:     "p",
		Protocol:     store.ProtocolOracle,
	}, key)
	require.NoError(t, err)

	conn, err := dataStore.CreateConnection(ctx, user.UID, db.UID, "10.0.0.9")
	require.NoError(t, err)

	s := newTestSession(&store.Grant{Definition: &store.GrantDefinition{Controls: []string{store.ControlReadOnly}}})
	s.completionStore = dataStore
	s.connectionUID = conn.UID

	const sql = "DELETE FROM emp"

	require.Error(t, s.handleOALL8(buildOALL8(sql, nil, 3)))

	var persisted []store.Query

	require.Eventually(t, func() bool {
		queries, err := dataStore.ListQueries(ctx, store.QueryFilter{ConnectionID: &conn.UID, Limit: 10})
		if err != nil || len(queries) == 0 {
			return false
		}

		persisted = queries

		return true
	}, 10*time.Second, 50*time.Millisecond, "the refused statement never reached the store")

	require.Len(t, persisted, 1)
	assertBlockedOracleRow(t, &persisted[0], sql, "read-only")

	// Now a quota refusal, on top of the control refusal already chained.
	const overQuotaSQL = "SELECT 1 FROM DUAL"

	maxQueries := int64(1)
	s.grant.QueryCount = 1
	s.grant.Definition.MaxQueryCounts = &maxQueries

	require.ErrorIs(t, s.handleOALL8(buildOALL8(overQuotaSQL, nil, 4)), ErrQueryLimitExceed)

	require.Eventually(t, func() bool {
		queries, err := dataStore.ListQueries(ctx, store.QueryFilter{ConnectionID: &conn.UID, Limit: 10})
		if err != nil || len(queries) < 2 {
			return false
		}

		persisted = queries

		return true
	}, 10*time.Second, 50*time.Millisecond, "the over-quota statement never reached the store")

	require.Len(t, persisted, 2)

	var overQuota *store.Query

	for i := range persisted {
		if persisted[i].SQLText == overQuotaSQL {
			overQuota = &persisted[i]
		}
	}

	assertBlockedOracleRow(t, overQuota, overQuotaSQL, "query limit exceeded")

	result, err := dataStore.VerifyQueryChain(ctx, conn.UID)
	require.NoError(t, err)
	assert.Nil(t, result.Break, "a refusal row must be a valid link in the connection's query chain")
	assert.Equal(t, int64(2), result.Verified)
}

// newOracleTestStore spins up a throwaway PostgreSQL store with the query
// chain enabled. Only dbbat's own storage DB is involved — no Oracle container
// — so this stays out of the integration-tagged suite and runs under
// `make test`.
func newOracleTestStore(t *testing.T) *store.Store {
	t.Helper()

	ctx := context.Background()

	container, err := postgres.Run(ctx,
		"postgres:15-alpine",
		postgres.WithDatabase("dbbat_test"),
		postgres.WithUsername("test"),
		postgres.WithPassword("test"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = container.Terminate(context.Background()) })

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)

	masterKey := make([]byte, 32)
	for i := range masterKey {
		masterKey[i] = byte(0xA0 + i)
	}

	dataStore, err := store.New(ctx, dsn, store.Options{EncryptionKey: masterKey})
	require.NoError(t, err)
	t.Cleanup(func() { dataStore.Close() })

	require.NoError(t, dataStore.Migrate(ctx))

	return dataStore
}

// assertBlockedOracleRow checks the shape every refusal row shares across the
// five protocols.
func assertBlockedOracleRow(t *testing.T, row *store.Query, wantSQL, wantErrFragment string) {
	t.Helper()

	require.NotNil(t, row)
	assert.Equal(t, wantSQL, row.SQLText)
	require.NotNil(t, row.Error, "a refused statement must be persisted with its refusal as error")
	assert.Contains(t, *row.Error, wantErrFragment)
	require.NotNil(t, row.DurationMs)
	assert.InDelta(t, 0, *row.DurationMs, 0.001, "a refused statement never ran")
	require.NotNil(t, row.RowsAffected)
	assert.Equal(t, int64(0), *row.RowsAffected)
}

// TestBlockedStatement_ReachesTheClientAsAnORAError closes the gap the tests
// above leave: they drive handleOALL8 directly and never look at what goes back
// on the wire, which is how a refusal frame no Oracle client could parse
// survived for as long as it did. This one runs the same refusal through
// gateStatement — the one place every SQL-carrying op funnels into — over a
// real socket, and decodes what the client receives.
//
// The live-client half is in blocked_integration_test.go; this is the always-on
// half, and it is the one that fails fast if the frame regresses.
func TestBlockedStatement_ReachesTheClientAsAnORAError(t *testing.T) {
	t.Parallel()

	client, proxyEnd := net.Pipe()

	t.Cleanup(func() {
		_ = client.Close()
		_ = proxyEnd.Close()
	})

	s := newTestSession(&store.Grant{
		Definition: &store.GrantDefinition{Controls: []string{store.ControlReadOnly}},
	})
	s.clientConn = proxyEnd
	s.completionStore = newRecordingCompletionStore()

	const sql = "INSERT INTO emp (id) VALUES (1)"

	blocked := make(chan bool, 1)

	go func() { blocked <- s.gateStatement(s.handleOALL8, buildOALL8(sql, nil, 7)) }()

	pkt, err := readTNSPacket(client)
	require.NoError(t, err, "the refusal must reach the client")
	assert.Equal(t, TNSPacketTypeData, pkt.Type)

	fc, err := parseTTCFunctionCode(pkt.Payload)
	require.NoError(t, err)
	assert.Equal(t, TTCFuncOERR, fc,
		"a client only ends its call on the message type a server ends one with")

	info := decodeOERAt(extractTTCPayload(pkt.Payload), 0)
	require.NotNil(t, info, "the refusal must decode as an end-of-call OER")
	assert.Equal(t, 1031, info.ErrorCode)
	assert.Contains(t, info.ErrorMessage, "read-only")

	assert.True(t, <-blocked, "the statement must be reported as blocked")
}
