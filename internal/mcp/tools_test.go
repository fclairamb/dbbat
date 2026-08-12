package mcp

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/events"
	"github.com/fclairamb/dbbat/internal/store"
)

// fakeGrantStore stands in for the store. The MCP layer only reads grant
// metadata from it, so a map is a faithful substitute — and keeping these
// tests off a database is what lets them assert the row cap and the
// approval-pending dance in milliseconds.
//
// It is also why the grant windows in this package are stamped from
// time.Now() and left that way, deliberately: ListGrants here hands back
// whatever the test put in the slice, no window filter of any kind, and the
// only consumer of the bounds is the RFC3339 string the tool layer echoes
// back. There is one clock in this package, so the two-clock hazard the store
// fixed (a Go-stamped window judged by PostgreSQL's `starts_at <= NOW()`)
// cannot arise — no database is ever reached. The backdated minute is
// cosmetic, describing a live grant rather than gating anything. Anything in
// this package that *does* grow a real store must stamp from
// Store.Now/dbNow instead, as internal/proxy/testsupport does.
type fakeGrantStore struct {
	grants  []store.Grant
	servers map[uuid.UUID]*store.Server
	// groupMembers is the membership a group-bound grant expands through.
	groupMembers map[uuid.UUID][]uuid.UUID
	err          error
}

func (f *fakeGrantStore) ListGrants(_ context.Context, _ store.GrantFilter) ([]store.Grant, error) {
	return f.grants, f.err
}

func (f *fakeGrantStore) ListServerGroupMemberUIDs(_ context.Context, groupUID uuid.UUID) ([]uuid.UUID, error) {
	return f.groupMembers[groupUID], nil
}

func (f *fakeGrantStore) GetServerByUID(_ context.Context, uid uuid.UUID) (*store.Server, error) {
	srv, ok := f.servers[uid]
	if !ok {
		return nil, store.ErrServerNotFound
	}

	return srv, nil
}

// fakeExecutor records what the tool layer asked for and returns whatever the
// test wants. It is the seam that lets these tests prove the row cap is
// applied *before* the statement runs.
type fakeExecutor struct {
	calls chan ExecRequest
	run   func(ctx context.Context, req ExecRequest) (*QueryResult, error)
}

func (f *fakeExecutor) Execute(ctx context.Context, req ExecRequest) (*QueryResult, error) {
	select {
	case f.calls <- req:
	default:
	}

	if f.run != nil {
		return f.run(ctx, req)
	}

	return &QueryResult{Columns: []string{"n"}, Rows: []map[string]any{{"n": int64(1)}}}, nil
}

// errDeniedByApprover stands in for what a protocol client reports when a
// human denies a held statement: an opaque error carrying the approver's
// reason, which the tool layer must surface rather than swallow.
var errDeniedByApprover = errors.New("query denied by bob: not during the freeze")

const (
	testDBName   = "prod-pg"
	testUsername = "alice"
	testAPIKey   = "dbb_testkey0123456789012345678901"
)

type fixture struct {
	server   *Server
	caller   *Caller
	store    *fakeGrantStore
	executor *fakeExecutor
	broker   *events.Broker
	dbUID    uuid.UUID
}

func newFixture(t *testing.T, protocol string, def store.GrantDefinition, run func(context.Context, ExecRequest) (*QueryResult, error)) *fixture {
	t.Helper()

	dbUID := uuid.New()
	userUID := uuid.New()

	grant := store.Grant{
		UID:        uuid.New(),
		UserID:     userUID,
		DatabaseID: dbUID,
		// Process clock on purpose — nothing here reaches a database that
		// would judge this window. See fakeGrantStore.
		StartsAt:   time.Now().Add(-time.Minute),
		ExpiresAt:  time.Now().Add(time.Hour),
		Definition: &def,
	}

	st := &fakeGrantStore{
		grants: []store.Grant{grant},
		servers: map[uuid.UUID]*store.Server{
			dbUID: {UID: dbUID, Name: testDBName, Description: "the prod database", Protocol: protocol},
		},
	}

	exec := &fakeExecutor{calls: make(chan ExecRequest, 4), run: run}
	broker := events.New()

	s := New(Deps{
		Store:       st,
		Broker:      func() *events.Broker { return broker },
		Executor:    exec,
		GraceWindow: 50 * time.Millisecond,
		AwaitWindow: 50 * time.Millisecond,
	})

	return &fixture{
		server:   s,
		caller:   &Caller{User: &store.User{UID: userUID, Username: testUsername}, APIKey: testAPIKey},
		store:    st,
		executor: exec,
		broker:   broker,
		dbUID:    dbUID,
	}
}

func (f *fixture) query(t *testing.T, in QueryInput) (QueryOutput, error) {
	t.Helper()

	_, out, err := f.server.toolQuery(f.caller)(context.Background(), nil, in)

	return out, err
}

// TestQueryEnforcesRowCapServerSide is the important one: whatever the agent
// asks for, the executor is handed at most HardMaxRows. The cap is not a
// suggestion the model can talk its way past.
func TestQueryEnforcesRowCapServerSide(t *testing.T) {
	t.Parallel()

	f := newFixture(t, store.ProtocolPostgreSQL, store.GrantDefinition{}, nil)

	out, err := f.query(t, QueryInput{Database: testDBName, SQL: "SELECT 1", MaxRows: 1_000_000})
	require.NoError(t, err)
	assert.Equal(t, StatusOK, out.Status)
	assert.Equal(t, HardMaxRows, out.MaxRows)

	req := <-f.executor.calls
	assert.Equal(t, HardMaxRows, req.MaxRows, "the executor must never be asked for more than the hard cap")
	assert.Equal(t, testDBName, req.Database)
	assert.Equal(t, testUsername, req.Username)
	assert.Equal(t, testAPIKey, req.APIKey, "the API key doubles as the proxy password")
	assert.Equal(t, "SELECT 1", req.SQL, "the statement must reach the proxy untouched")
}

func TestClampMaxRows(t *testing.T) {
	t.Parallel()

	assert.Equal(t, DefaultMaxRows, clampMaxRows(0), "unset means the default")
	assert.Equal(t, DefaultMaxRows, clampMaxRows(-5), "a negative ask is not a way to disable the cap")
	assert.Equal(t, 7, clampMaxRows(7), "a smaller ask is honored")
	assert.Equal(t, HardMaxRows, clampMaxRows(HardMaxRows+1))
	assert.Equal(t, HardMaxRows, clampMaxRows(1<<30))
}

func TestQueryRejectsEmptySQL(t *testing.T) {
	t.Parallel()

	f := newFixture(t, store.ProtocolPostgreSQL, store.GrantDefinition{}, nil)

	_, err := f.query(t, QueryInput{Database: testDBName, SQL: "   \n\t "})
	require.ErrorIs(t, err, ErrSQLRequired)
}

// TestQueryRefusesDatabaseWithoutGrant pins that the MCP surface is scoped to
// the caller's grants and cannot be used to probe for databases.
func TestQueryRefusesDatabaseWithoutGrant(t *testing.T) {
	t.Parallel()

	f := newFixture(t, store.ProtocolPostgreSQL, store.GrantDefinition{}, nil)

	_, err := f.query(t, QueryInput{Database: "some-other-db", SQL: "SELECT 1"})
	require.ErrorIs(t, err, ErrNoGrant)

	select {
	case req := <-f.executor.calls:
		t.Fatalf("a statement was executed against an ungranted database: %+v", req)
	default:
	}
}

func TestQueryRefusesUnsupportedProtocol(t *testing.T) {
	t.Parallel()

	// An SSH-only entry is a bastion, not a database: there is no listener to
	// dial and no statement to run.
	f := newFixture(t, store.ProtocolSSH, store.GrantDefinition{}, nil)

	_, err := f.query(t, QueryInput{Database: testDBName, SQL: "SELECT 1"})
	require.ErrorIs(t, err, ErrProtocolUnsupported)
}

func TestQuerySurfacesDatabaseErrorsAsToolErrors(t *testing.T) {
	t.Parallel()

	f := newFixture(t, store.ProtocolPostgreSQL, store.GrantDefinition{},
		func(context.Context, ExecRequest) (*QueryResult, error) { return nil, errDeniedByApprover })

	_, err := f.query(t, QueryInput{Database: testDBName, SQL: "DELETE FROM users"})
	require.ErrorIs(t, err, errDeniedByApprover)
}

// TestApprovalHoldThenAwaitApproval walks the whole shape the spec asks for:
// a statement that does not come back inside the grace window is reported as
// approval_pending — naming the held query so a human can be pointed at it —
// and await_approval later returns the real result. At no point does the agent
// get a timeout with nothing to act on.
func TestApprovalHoldThenAwaitApproval(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	heldQueryUID := uuid.NewString()

	var f *fixture

	f = newFixture(t, store.ProtocolPostgreSQL,
		store.GrantDefinition{ApprovalPatterns: store.StringArray{`(?i)^\s*DELETE`}},
		func(ctx context.Context, req ExecRequest) (*QueryResult, error) {
			// Stand in for the proxy's approval gate: announce the hold on the
			// broker until somebody notices, then block like a parked session.
			stop := make(chan struct{})
			defer close(stop)

			go announceHold(f.broker, f.caller.User.UID, req, heldQueryUID, stop)

			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}

			return &QueryResult{RowsAffected: ptr(int64(3))}, nil
		})

	out, err := f.query(t, QueryInput{Database: testDBName, SQL: "DELETE FROM users"})
	require.NoError(t, err, "a hold is a status, never a tool error")
	assert.Equal(t, StatusApprovalPending, out.Status)
	assert.NotEmpty(t, out.ExecutionID, "the agent must be given something to poll")
	assert.Equal(t, heldQueryUID, out.QueryUID, "the held query must be named so a human can be pointed at it")
	assert.Equal(t, `(?i)^\s*DELETE`, out.ApprovalPattern)
	assert.NotEmpty(t, out.Message)

	await := f.server.toolAwaitApproval(f.caller)

	// Still parked: awaiting answers with the same status rather than hanging
	// or erroring. This is the "never silently time out" contract.
	_, pending, err := await(context.Background(), nil, AwaitApprovalInput{ExecutionID: out.ExecutionID})
	require.NoError(t, err)
	assert.Equal(t, StatusApprovalPending, pending.Status)
	assert.Equal(t, out.ExecutionID, pending.ExecutionID)

	close(release)

	_, final, err := await(context.Background(), nil, AwaitApprovalInput{
		ExecutionID:    out.ExecutionID,
		TimeoutSeconds: 5,
	})
	require.NoError(t, err)
	assert.Equal(t, StatusOK, final.Status)
	require.NotNil(t, final.RowsAffected)
	assert.Equal(t, int64(3), *final.RowsAffected)
}

// announceHold republishes a pending-approval event until the correlator picks
// it up or the execution ends, which removes the subscribe/publish race
// without the test having to reach into the broker's internals.
func announceHold(broker *events.Broker, userUID uuid.UUID, req ExecRequest, queryUID string, stop <-chan struct{}) {
	ticker := time.NewTicker(2 * time.Millisecond)
	defer ticker.Stop()

	data := map[string]any{
		"query_uid":        queryUID,
		"user_uid":         userUID.String(),
		"database_name":    req.Database,
		"sql_text":         req.SQL,
		"approval_status":  store.ApprovalPending,
		"approval_pattern": `(?i)^\s*DELETE`,
	}

	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			broker.Publish(events.TopicApprovalsPending, events.EventApprovalPending, data)
		}
	}
}

// TestSlowQueryIsStillRunningNotPending distinguishes a slow statement from a
// gated one: no hold was observed, so the agent is told to keep waiting rather
// than to go find an approver who does not exist.
func TestSlowQueryIsStillRunningNotPending(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	f := newFixture(t, store.ProtocolPostgreSQL, store.GrantDefinition{},
		func(ctx context.Context, _ ExecRequest) (*QueryResult, error) {
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}

			return &QueryResult{}, nil
		})

	defer close(release)

	out, err := f.query(t, QueryInput{Database: testDBName, SQL: "SELECT pg_sleep(60)"})
	require.NoError(t, err)
	assert.Equal(t, StatusStillRunning, out.Status)
	assert.NotEmpty(t, out.ExecutionID)
	assert.Empty(t, out.QueryUID)
}

// TestAwaitApprovalRejectsForeignExecution: an execution id is scoped to its
// owner, and a miss reads as unknown rather than forbidden so the id space
// cannot be probed.
func TestAwaitApprovalRejectsForeignExecution(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	defer close(release)

	f := newFixture(t, store.ProtocolPostgreSQL, store.GrantDefinition{},
		func(ctx context.Context, _ ExecRequest) (*QueryResult, error) {
			select {
			case <-release:
			case <-ctx.Done():
			}

			return &QueryResult{}, nil
		})

	out, err := f.query(t, QueryInput{Database: testDBName, SQL: "SELECT 1"})
	require.NoError(t, err)
	require.NotEmpty(t, out.ExecutionID)

	intruder := &Caller{User: &store.User{UID: uuid.New(), Username: "mallory"}, APIKey: "dbb_other"}

	_, _, err = f.server.toolAwaitApproval(intruder)(context.Background(), nil,
		AwaitApprovalInput{ExecutionID: out.ExecutionID})
	require.ErrorIs(t, err, ErrUnknownExecution)
}

func TestAwaitApprovalRejectsUnknownExecution(t *testing.T) {
	t.Parallel()

	f := newFixture(t, store.ProtocolPostgreSQL, store.GrantDefinition{}, nil)

	_, _, err := f.server.toolAwaitApproval(f.caller)(context.Background(), nil,
		AwaitApprovalInput{ExecutionID: uuid.NewString()})
	require.ErrorIs(t, err, ErrUnknownExecution)

	_, _, err = f.server.toolAwaitApproval(f.caller)(context.Background(), nil,
		AwaitApprovalInput{ExecutionID: "  "})
	require.ErrorIs(t, err, ErrUnknownExecution)
}

func TestAwaitWindowIsClamped(t *testing.T) {
	t.Parallel()

	s := New(Deps{AwaitWindow: 3 * time.Second})

	assert.Equal(t, 3*time.Second, s.awaitWindow(0))
	assert.Equal(t, 2*time.Second, s.awaitWindow(2))
	assert.Equal(t, MaxAwaitWindow, s.awaitWindow(100_000))
}

// TestListDatabasesReportsGrantShape covers what the agent plans against.
func TestListDatabasesReportsGrantShape(t *testing.T) {
	t.Parallel()

	maxQueries := int64(50)
	f := newFixture(t, store.ProtocolPostgreSQL, store.GrantDefinition{
		Controls:         store.StringArray{store.ControlReadOnly, store.ControlBlockDDL},
		ApprovalPatterns: store.StringArray{`(?i)^\s*DELETE`, `(?i)^\s*DROP`},
		MaxQueryCounts:   &maxQueries,
	}, nil)

	_, out, err := f.server.toolListDatabases(f.caller)(context.Background(), nil, ListDatabasesInput{})
	require.NoError(t, err)
	require.Len(t, out.Databases, 1)

	db := out.Databases[0]
	assert.Equal(t, testDBName, db.Name)
	assert.Equal(t, store.ProtocolPostgreSQL, db.Protocol)
	assert.True(t, db.Supported)
	assert.True(t, db.ReadOnly)
	assert.True(t, db.BlockDDL)
	assert.False(t, db.BlockCopy)
	assert.Equal(t, 2, db.ApprovalPatterns)
	require.NotNil(t, db.MaxQueries)
	assert.Equal(t, int64(50), *db.MaxQueries)
	assert.NotEmpty(t, db.GrantExpiresAt)
}

// TestListDatabasesFlagsUnsupportedProtocols keeps a database MCP cannot drive
// visible rather than invisible: an agent that cannot query it should be told
// that, not left believing the grant does not exist.
func TestListDatabasesFlagsUnsupportedProtocols(t *testing.T) {
	t.Parallel()

	f := newFixture(t, store.ProtocolSSH, store.GrantDefinition{}, nil)

	_, out, err := f.server.toolListDatabases(f.caller)(context.Background(), nil, ListDatabasesInput{})
	require.NoError(t, err)
	require.Len(t, out.Databases, 1)
	assert.False(t, out.Databases[0].Supported)
}

// TestDescribeUsesBoundParameters: table names come from a model, so the
// introspection helper must never concatenate one into SQL.
func TestDescribeUsesBoundParameters(t *testing.T) {
	t.Parallel()

	evil := "users; DROP TABLE accounts--"

	for _, protocol := range []string{
		store.ProtocolPostgreSQL, store.ProtocolMySQL, store.ProtocolOracle, store.ProtocolMSSQL,
	} {
		sqlText, params, err := introspectionSQL(protocol, evil, "")
		require.NoError(t, err)
		assert.NotContains(t, sqlText, "DROP TABLE accounts")
		require.Len(t, params, 1)
		assert.Equal(t, evil, params[0])
	}

	sqlText, params, err := introspectionSQL(store.ProtocolPostgreSQL, "users", "public")
	require.NoError(t, err)
	assert.Contains(t, sqlText, "$2")
	assert.Equal(t, []any{"users", "public"}, params)

	sqlText, params, err = introspectionSQL(store.ProtocolMySQL, "", "")
	require.NoError(t, err)
	assert.Contains(t, sqlText, "information_schema.tables")
	assert.Nil(t, params)

	sqlText, params, err = introspectionSQL(store.ProtocolMSSQL, "orders", "dbo")
	require.NoError(t, err)
	assert.Contains(t, sqlText, "INFORMATION_SCHEMA.COLUMNS")
	assert.Contains(t, sqlText, "@p2")
	assert.Equal(t, []any{"orders", "dbo"}, params)

	// MongoDB has no SQL and no bind parameters, so the same rule is kept the
	// only other way it can be: the collection name is a value handed to the
	// BSON encoder, never text concatenated into the command.
	sqlText, params, err = introspectionSQL(store.ProtocolMongoDB, `users","drop":"accounts`, "")
	require.NoError(t, err)
	assert.Nil(t, params)
	assert.NotContains(t, sqlText, `"drop":"accounts"`)
	assert.Contains(t, sqlText, `\"drop\":\"accounts`, "the name must be escaped inside the JSON string")

	sqlText, params, err = introspectionSQL(store.ProtocolMongoDB, "", "")
	require.NoError(t, err)
	assert.Nil(t, params)
	assert.Contains(t, sqlText, "listCollections")

	_, _, err = introspectionSQL(store.ProtocolSSH, "", "")
	require.ErrorIs(t, err, ErrProtocolUnsupported)
}

// TestOracleIntrospectionSQL pins the two Oracle-specific choices: the result
// columns must come back lower-cased (Oracle folds unquoted identifiers to
// upper case, and the renderers key on the lower-case names), and the table
// name must stay a bind parameter even though it is folded with UPPER().
func TestOracleIntrospectionSQL(t *testing.T) {
	t.Parallel()

	sqlText, params, err := introspectionSQL(store.ProtocolOracle, "", "")
	require.NoError(t, err)
	assert.Contains(t, sqlText, "all_tables")
	assert.Contains(t, sqlText, `AS "table_name"`)
	assert.Nil(t, params)

	sqlText, params, err = introspectionSQL(store.ProtocolOracle, "employees", "")
	require.NoError(t, err)
	assert.Contains(t, sqlText, "all_tab_columns")
	assert.Contains(t, sqlText, "UPPER(:1)")
	assert.Contains(t, sqlText, `AS "column_name"`)
	assert.Equal(t, []any{"employees"}, params)

	sqlText, params, err = introspectionSQL(store.ProtocolOracle, "employees", "hr")
	require.NoError(t, err)
	assert.Contains(t, sqlText, "UPPER(:2)")
	assert.Equal(t, []any{"employees", "hr"}, params)
}

func TestDescribeRendersColumns(t *testing.T) {
	t.Parallel()

	f := newFixture(t, store.ProtocolPostgreSQL, store.GrantDefinition{},
		func(context.Context, ExecRequest) (*QueryResult, error) {
			return &QueryResult{
				Columns: []string{"column_name", "data_type", "is_nullable", "column_default"},
				Rows: []map[string]any{
					{"column_name": "id", "data_type": "integer", "is_nullable": "NO", "column_default": nil},
					{"column_name": "email", "data_type": "text", "is_nullable": "YES", "column_default": "''"},
				},
			}, nil
		})

	_, out, err := f.server.toolDescribe(f.caller)(context.Background(), nil,
		DescribeInput{Database: testDBName, Table: "users"})
	require.NoError(t, err)
	assert.Equal(t, StatusOK, out.Status)
	require.Len(t, out.Columns, 2)
	assert.Equal(t, ColumnInfo{Name: "id", Type: "integer", Nullable: false}, out.Columns[0])
	assert.Equal(t, ColumnInfo{Name: "email", Type: "text", Nullable: true, Default: "''"}, out.Columns[1])
}

func ptr[T any](v T) *T { return &v }
