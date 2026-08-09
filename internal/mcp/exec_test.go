package mcp

import (
	"context"
	"database/sql/driver"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/store"
)

func TestSupportedProtocol(t *testing.T) {
	t.Parallel()

	assert.True(t, SupportedProtocol(store.ProtocolPostgreSQL))
	assert.True(t, SupportedProtocol(store.ProtocolMySQL))
	assert.True(t, SupportedProtocol(store.ProtocolMariaDB))

	// Phase 2. Listed explicitly so adding one is a deliberate change here
	// rather than a silent side effect elsewhere.
	assert.False(t, SupportedProtocol(store.ProtocolOracle))
	assert.False(t, SupportedProtocol(store.ProtocolMongoDB))
	assert.False(t, SupportedProtocol(store.ProtocolMSSQL))
	assert.False(t, SupportedProtocol(store.ProtocolSSH))
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

	e := NewLoopbackExecutor("", "")

	_, err := e.Execute(context.Background(), ExecRequest{Protocol: store.ProtocolPostgreSQL})
	require.ErrorIs(t, err, ErrListenerDisabled)

	_, err = e.Execute(context.Background(), ExecRequest{Protocol: store.ProtocolMySQL})
	require.ErrorIs(t, err, ErrListenerDisabled)

	_, err = e.Execute(context.Background(), ExecRequest{Protocol: store.ProtocolOracle})
	require.ErrorIs(t, err, ErrProtocolUnsupported)
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
