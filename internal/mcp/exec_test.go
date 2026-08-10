package mcp

import (
	"context"
	"database/sql/driver"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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

	// Still to come.
	assert.False(t, SupportedProtocol(store.ProtocolMongoDB))
	assert.False(t, SupportedProtocol(store.ProtocolMSSQL))

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
		store.ProtocolOracle,
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
