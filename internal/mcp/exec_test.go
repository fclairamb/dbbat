package mcp

import (
	"context"
	"database/sql/driver"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/fclairamb/dbbat/internal/store"
)

func TestSupportedProtocol(t *testing.T) {
	t.Parallel()

	// Every protocol is listed explicitly so adding or removing one is a
	// deliberate change here rather than a silent side effect elsewhere.
	assert.True(t, SupportedProtocol(store.ProtocolPostgreSQL))
	assert.True(t, SupportedProtocol(store.ProtocolMySQL))
	assert.True(t, SupportedProtocol(store.ProtocolMariaDB))
	assert.True(t, SupportedProtocol(store.ProtocolOracle))
	assert.True(t, SupportedProtocol(store.ProtocolMSSQL))
	assert.True(t, SupportedProtocol(store.ProtocolMongoDB))

	// An SSH-only entry is a bastion, not a database: there is no listener to
	// dial and no statement to run.
	assert.False(t, SupportedProtocol(store.ProtocolSSH))
	assert.False(t, SupportedProtocol("redis"))
}

func TestLoopbackAddr(t *testing.T) {
	t.Parallel()

	cases := []struct {
		listen string
		want   string
		err    bool
	}{
		{listen: ":5433", want: "127.0.0.1:5433"},
		{listen: "0.0.0.0:5433", want: "127.0.0.1:5433"},
		{listen: "[::]:5433", want: "127.0.0.1:5433"},
		{listen: "127.0.0.1:5433", want: "127.0.0.1:5433"},
		{listen: "10.0.0.5:3307", want: "10.0.0.5:3307"},
		{listen: "", err: true},
		{listen: ":0", err: true},
		{listen: "nonsense", err: true},
	}

	for _, tc := range cases {
		got, err := loopbackAddr(tc.listen)
		if tc.err {
			require.ErrorIs(t, err, ErrListenerDisabled, "listen %q", tc.listen)

			continue
		}

		require.NoError(t, err, "listen %q", tc.listen)
		assert.Equal(t, tc.want, got, "listen %q", tc.listen)
	}
}

// TestLoopbackExecutorRefusesDisabledListener: with a protocol's listener off,
// there is nothing of ours to dial, and the executor must refuse rather than
// find another way to the database.
func TestLoopbackExecutorRefusesDisabledListener(t *testing.T) {
	t.Parallel()

	e := NewLoopbackExecutor(LoopbackListeners{})

	for _, protocol := range []string{
		store.ProtocolPostgreSQL, store.ProtocolMySQL, store.ProtocolMariaDB,
		store.ProtocolOracle, store.ProtocolMongoDB, store.ProtocolMSSQL,
	} {
		_, err := e.Execute(context.Background(), ExecRequest{Protocol: protocol})
		require.ErrorIsf(t, err, ErrListenerDisabled, "protocol %s", protocol)
	}

	_, err := e.Execute(context.Background(), ExecRequest{Protocol: store.ProtocolSSH})
	require.ErrorIs(t, err, ErrProtocolUnsupported)
}

// TestLoopbackExecutorDialsTheRightListener: each protocol must reach its own
// listener. A copy-paste that pointed Oracle at the MySQL address would
// otherwise only show up against a live proxy.
func TestLoopbackExecutorDialsTheRightListener(t *testing.T) {
	t.Parallel()

	// Only one listener is configured per case, and it is an address nothing
	// runs on — so a dispatch that read the wrong field would report
	// ErrListenerDisabled instead of failing to connect. No database is
	// involved: every dial is refused.
	const dead = "127.0.0.1:59987"

	cases := []struct {
		protocol  string
		listeners LoopbackListeners
		sqlText   string
	}{
		{store.ProtocolPostgreSQL, LoopbackListeners{PostgreSQL: dead}, "SELECT 1"},
		{store.ProtocolMySQL, LoopbackListeners{MySQL: dead}, "SELECT 1"},
		{store.ProtocolMariaDB, LoopbackListeners{MySQL: dead}, "SELECT 1"},
		{store.ProtocolOracle, LoopbackListeners{Oracle: dead}, "SELECT 1 FROM dual"},
		{store.ProtocolMongoDB, LoopbackListeners{MongoDB: dead}, `ping {"ping":1}`},
		{store.ProtocolMSSQL, LoopbackListeners{MSSQL: dead}, "SELECT 1"},
	}

	for _, tc := range cases {
		e := NewLoopbackExecutor(tc.listeners)

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)

		_, err := e.Execute(ctx, ExecRequest{
			Protocol: tc.protocol,
			Database: "db",
			Username: "agent",
			APIKey:   "dbb_key",
			SQL:      tc.sqlText,
			MaxRows:  10,
		})

		cancel()

		require.Error(t, err, "protocol %s", tc.protocol)
		require.NotErrorIs(t, err, ErrListenerDisabled, "protocol %s reads the wrong listen address", tc.protocol)
		require.NotErrorIs(t, err, ErrProtocolUnsupported, "protocol %s", tc.protocol)
	}
}

func TestDedupeColumns(t *testing.T) {
	t.Parallel()

	// Rows are keyed by column name, so a repeated name would otherwise drop a
	// column the agent can still see in Columns.
	assert.Equal(t, []string{"id", "id_2", "id_3"}, dedupeColumns([]string{"id", "id", "id"}))
	assert.Equal(t, []string{"a", "b"}, dedupeColumns([]string{"a", "b"}))
	assert.Equal(t, []string{"column_1", "x"}, dedupeColumns([]string{"", "x"}))
}

func TestPGConnConfigIgnoresEnvironment(t *testing.T) {
	// Not parallel: it sets process environment.
	t.Setenv("PGHOST", "attacker.example.com")
	t.Setenv("PGPORT", "9999")
	t.Setenv("PGDATABASE", "elsewhere")
	t.Setenv("PGOPTIONS", "-c statement_timeout=0")
	t.Setenv("PGAPPNAME", "not-dbbat")

	cfg, err := pgConnConfig("127.0.0.1:5433", ExecRequest{
		Username: "alice",
		APIKey:   "dbb_key",
		Database: "prod-pg",
	})
	require.NoError(t, err)

	assert.Equal(t, "127.0.0.1", cfg.Host)
	assert.Equal(t, uint16(5433), cfg.Port)
	assert.Equal(t, "prod-pg", cfg.Database)
	assert.Equal(t, "alice", cfg.User)
	assert.Equal(t, "dbb_key", cfg.Password)
	assert.Nil(t, cfg.TLSConfig, "the loopback leg is plaintext by design")
	assert.Empty(t, cfg.Fallbacks)
	assert.Equal(t, map[string]string{"application_name": mcpApplicationName}, cfg.RuntimeParams)
}

func TestPGConnConfigRejectsBadAddress(t *testing.T) {
	t.Parallel()

	_, err := pgConnConfig("nonsense", ExecRequest{})
	require.ErrorIs(t, err, ErrListenerDisabled)

	_, err = pgConnConfig("127.0.0.1:70000", ExecRequest{})
	require.ErrorIs(t, err, ErrListenerDisabled)
}

func TestNormalizePGValue(t *testing.T) {
	t.Parallel()

	assert.Nil(t, normalizePGValue(nil))
	assert.Equal(t, "x", normalizePGValue("x"))
	assert.Equal(t, int64(3), normalizePGValue(int64(3)))
	assert.Equal(t, []byte{1, 2}, normalizePGValue([]byte{1, 2}))
	assert.Equal(t, "42", normalizePGValue(stringValuer{"42"}))
}

// stringValuer stands in for the pgtype values (numeric, interval, …) that
// render themselves through database/sql/driver.
type stringValuer struct{ s string }

func (v stringValuer) Value() (driver.Value, error) { return v.s, nil }

func TestLeadingKeyword(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "select", leadingKeyword("  SELECT 1 FROM dual"))
	assert.Equal(t, "select", leadingKeyword("(SELECT 1)"))
	assert.Equal(t, "insert", leadingKeyword("insert into t values (1)"))
	assert.Empty(t, leadingKeyword("-- a comment\nDELETE FROM t"),
		"a statement that opens with a comment must fall to the query path")
	assert.Empty(t, leadingKeyword(""))
}

// TestOracleIsExecStatement pins the one place a protocol client decides
// between Exec and Query. Being wrong can only drop `rows_affected`, never a
// row — but a SELECT sent to Exec would drop the whole result set.
func TestOracleIsExecStatement(t *testing.T) {
	t.Parallel()

	for _, sqlText := range []string{
		"INSERT INTO t VALUES (1)",
		"  update t set a = 1",
		"MERGE INTO t USING s ON (1=1)",
		"BEGIN pkg.proc(); END;",
		"create table t (id number)",
	} {
		assert.True(t, oracleIsExecStatement(sqlText), sqlText)
	}

	for _, sqlText := range []string{
		"SELECT * FROM t",
		"  with x as (select 1 from dual) select * from x",
		"-- comment\nSELECT 1 FROM dual",
		"",
	} {
		assert.False(t, oracleIsExecStatement(sqlText), sqlText)
	}
}

func TestNormalizeSQLValue(t *testing.T) {
	t.Parallel()

	assert.Nil(t, normalizeSQLValue(nil))
	assert.Equal(t, int64(3), normalizeSQLValue(int64(3)))
	assert.Equal(t, "x", normalizeSQLValue("x"))
	// Oracle and SQL Server both hand text back as bytes; base64 would make
	// every VARCHAR2 unreadable to a model.
	assert.Equal(t, "étiquette", normalizeSQLValue([]byte("étiquette")))
	assert.Equal(t, []byte{0xff, 0xfe}, normalizeSQLValue([]byte{0xff, 0xfe}))
	assert.Equal(t, "2026-08-10T09:00:00Z",
		normalizeSQLValue(time.Date(2026, 8, 10, 9, 0, 0, 0, time.UTC)))
}

// TestMSSQLIsExecStatement covers the one dialect difference from Oracle: a
// T-SQL write can return rows through an OUTPUT clause, so those must not be
// sent to Exec, which would silently drop the result set.
func TestMSSQLIsExecStatement(t *testing.T) {
	t.Parallel()

	for _, sqlText := range []string{
		"INSERT INTO t (id) VALUES (1)",
		"  update t set a = 1 where id = @p1",
		"DELETE FROM dbbat_stage3",
		"TRUNCATE TABLE t",
	} {
		assert.True(t, mssqlIsExecStatement(sqlText), sqlText)
	}

	for _, sqlText := range []string{
		"SELECT TOP 10 * FROM t",
		"DELETE FROM t OUTPUT deleted.id",
		"INSERT INTO t OUTPUT INSERTED.id VALUES (1)",
		"EXEC sp_who",
		"-- comment\nDELETE FROM t",
	} {
		assert.False(t, mssqlIsExecStatement(sqlText), sqlText)
	}
}

func TestContainsIdentifier(t *testing.T) {
	t.Parallel()

	assert.True(t, containsIdentifier("DELETE FROM t OUTPUT deleted.id", "output"))
	assert.True(t, containsIdentifier("output x", "output"))
	assert.False(t, containsIdentifier("SELECT outputs FROM t", "output"))
	assert.False(t, containsIdentifier("SELECT my_output FROM t", "output"))
	assert.False(t, containsIdentifier("SELECT 1", "output"))
}

// TestParseMongoStatement pins the statement shape the spec chose over a second
// `run_command` tool: `<command> <extended JSON>`, which is byte-for-byte what
// the MongoDB proxy renders into /queries and matches approval patterns
// against.
func TestParseMongoStatement(t *testing.T) {
	t.Parallel()

	command, doc, err := parseMongoStatement(`find {"find":"users","filter":{"active":true},"limit":10}`)
	require.NoError(t, err)
	assert.Equal(t, "find", command)
	require.NotEmpty(t, doc)
	assert.Equal(t, "find", doc[0].Key)
	assert.Equal(t, "users", doc[0].Value)

	// The leading word is a readability affordance: MongoDB names a command by
	// the document's first key, so a bare document works too.
	command, _, err = parseMongoStatement(`  {"ping": 1}  `)
	require.NoError(t, err)
	assert.Equal(t, "ping", command)

	// $db comes from the connection; a second one in the body would be a
	// duplicate field on the wire.
	_, doc, err = parseMongoStatement(`find {"find":"users","$db":"elsewhere"}`)
	require.NoError(t, err)

	for _, element := range doc {
		assert.NotEqual(t, "$db", element.Key)
	}
}

func TestParseMongoStatementRejectsBadInput(t *testing.T) {
	t.Parallel()

	_, _, err := parseMongoStatement("SELECT 1")
	require.ErrorIs(t, err, ErrMongoCommandSyntax)

	_, _, err = parseMongoStatement(`find {"find":`)
	require.ErrorIs(t, err, ErrMongoCommandSyntax)

	_, _, err = parseMongoStatement(`{}`)
	require.ErrorIs(t, err, ErrMongoCommandEmpty)

	// A mismatch is a real bug in the agent's statement — the document's first
	// key is what MongoDB would run — so it is refused rather than guessed at.
	_, _, err = parseMongoStatement(`find {"insert":"users","documents":[{}]}`)
	require.ErrorIs(t, err, ErrMongoCommandMismatch)
}

func TestMongoCommandReturnsCursor(t *testing.T) {
	t.Parallel()

	assert.True(t, mongoCommandReturnsCursor("find"))
	assert.True(t, mongoCommandReturnsCursor("aggregate"))
	assert.True(t, mongoCommandReturnsCursor("listCollections"))
	assert.True(t, mongoCommandReturnsCursor("listIndexes"))

	// Running a write through the cursor API would execute it and *then* fail
	// to parse the reply, which is the one outcome an agent must never see.
	assert.False(t, mongoCommandReturnsCursor("insert"))
	assert.False(t, mongoCommandReturnsCursor("update"))
	assert.False(t, mongoCommandReturnsCursor("ping"))
	assert.False(t, mongoCommandReturnsCursor("getMore"))
}

func TestJSONSafeBSON(t *testing.T) {
	t.Parallel()

	oid := bson.NewObjectID()
	assert.Equal(t, oid.Hex(), jsonSafeBSON(oid), "an ObjectID must be quotable back into a filter")

	when := time.Date(2026, 8, 10, 9, 0, 0, 0, time.UTC)
	assert.Equal(t, "2026-08-10T09:00:00Z", jsonSafeBSON(bson.NewDateTimeFromTime(when)))

	assert.Equal(t, map[string]any{"a": int32(1)},
		jsonSafeBSON(bson.D{{Key: "a", Value: int32(1)}}))
	assert.Equal(t, []any{"x", int32(2)}, jsonSafeBSON(bson.A{"x", int32(2)}))
	assert.Equal(t, "text", jsonSafeBSON("text"))
}

// TestMongoDocument proves the wire order of a document survives into Columns,
// which is what lets an agent read a result the way MongoDB laid it out.
func TestMongoDocument(t *testing.T) {
	t.Parallel()

	raw, err := bson.Marshal(bson.D{
		{Key: "_id", Value: int32(1)},
		{Key: "name", Value: "gadget"},
		{Key: "qty", Value: int32(7)},
	})
	require.NoError(t, err)

	keys, values, err := mongoDocument(raw)
	require.NoError(t, err)
	assert.Equal(t, []string{"_id", "name", "qty"}, keys)
	assert.Equal(t, "gadget", values["name"])
}

func TestRenderSampledFields(t *testing.T) {
	t.Parallel()

	columns := renderSampledFields(&QueryResult{
		Columns: []string{"_id", "name", "tags", "meta", "missing"},
		Rows: []map[string]any{{
			"_id":  "68b0…",
			"name": "gadget",
			"tags": []any{"a"},
			"meta": map[string]any{"k": "v"},
		}},
	})

	require.Len(t, columns, 5)
	assert.Equal(t, "string", columns[1].Type)
	assert.Equal(t, "array", columns[2].Type)
	assert.Equal(t, "object", columns[3].Type)
	assert.Equal(t, "null", columns[4].Type)
	assert.True(t, columns[4].Nullable)
}
