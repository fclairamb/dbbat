package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestConnectionRetentionDefaultsToInheritingTheQueryWindow pins the
// backward-compatibility rule: an upgraded deployment that never heard of
// DBB_CONNECTION_RETENTION sweeps exactly what it swept before.
func TestConnectionRetentionDefaultsToInheritingTheQueryWindow(t *testing.T) {
	t.Setenv("DBB_DSN", "postgres://x:x@localhost/x")
	t.Setenv("DBB_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	t.Setenv("DBB_QUERY_STORAGE_RETENTION", "720h")

	cfg, err := Load(LoadOptions{})
	require.NoError(t, err)

	assert.Equal(t, DefaultConnectionRetention, cfg.Connection.Retention)
	assert.False(t, cfg.Connection.RetentionSet())

	windows := cfg.RetentionWindows()
	assert.Empty(t, windows.Misconfiguration)
	assert.Equal(t, 720*time.Hour, windows.Query)
	assert.Equal(t, 720*time.Hour, windows.Connection,
		"an unset connection window inherits the query window")
}

// TestConnectionRetentionEnvVarReachesItsKey is the test the naming decision
// asks for: DBB_CONNECTION_RETENTION deliberately breaks the `<table>_storage`
// shape, so it needs its own envTransform case. A silently unmapped value would
// read as "unset" and inherit the query window — the exact failure the split
// exists to prevent — with no error anywhere.
func TestConnectionRetentionEnvVarReachesItsKey(t *testing.T) {
	t.Setenv("DBB_DSN", "postgres://x:x@localhost/x")
	t.Setenv("DBB_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	t.Setenv("DBB_QUERY_STORAGE_RETENTION", "720h")
	t.Setenv("DBB_CONNECTION_RETENTION", "8760h")

	cfg, err := Load(LoadOptions{})
	require.NoError(t, err)

	assert.Equal(t, "8760h", cfg.Connection.Retention)
	assert.Equal(t, 8760*time.Hour, cfg.Connection.RetentionDuration())

	windows := cfg.RetentionWindows()
	assert.Empty(t, windows.Misconfiguration)
	assert.Equal(t, 720*time.Hour, windows.Query)
	assert.Equal(t, 8760*time.Hour, windows.Connection)
}

// TestConnectionRetentionEnvKeyMapping checks the transform directly, so a
// regression names the mapping rather than a downstream symptom.
func TestConnectionRetentionEnvKeyMapping(t *testing.T) {
	t.Parallel()

	key, value := envTransform("DBB_CONNECTION_RETENTION", "8760h")
	assert.Equal(t, "connection.retention", key)
	assert.Equal(t, "8760h", value)
}

func TestConnectionRetentionDuration(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		value  string
		want   time.Duration
		setVal bool
	}{
		{"unset inherits", "", 0, false},
		{"explicit zero keeps the ledger forever", "0", 0, true},
		{"malformed keeps everything", "365d", 0, true},
		{"negative keeps everything", "-1h", 0, true},
		{"valid duration", "8760h", 8760 * time.Hour, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := ConnectionConfig{Retention: tt.value}
			assert.Equal(t, tt.want, cfg.RetentionDuration())
			assert.Equal(t, tt.setVal, cfg.RetentionSet())
		})
	}
}

// TestRetentionWindows walks every combination the resolver has a rule for.
// The invariant under all of them: a misconfiguration disables *both* sweeps,
// never one, and never a guessed window.
func TestRetentionWindows(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		query          string
		connection     string
		wantQuery      time.Duration
		wantConnection time.Duration
		wantBad        bool
	}{
		{
			name:  "both unset keeps everything forever",
			query: "0", connection: "",
		},
		{
			name:  "unset connection inherits the query window",
			query: "720h", connection: "",
			wantQuery: 720 * time.Hour, wantConnection: 720 * time.Hour,
		},
		{
			name:  "equal windows reproduce the single-window behavior",
			query: "720h", connection: "720h",
			wantQuery: 720 * time.Hour, wantConnection: 720 * time.Hour,
		},
		{
			name:  "a longer ledger window is the point of the split",
			query: "720h", connection: "8760h",
			wantQuery: 720 * time.Hour, wantConnection: 8760 * time.Hour,
		},
		{
			name:  "explicit zero keeps the ledger forever while statements expire",
			query: "720h", connection: "0",
			wantQuery: 720 * time.Hour, wantConnection: 0,
		},
		{
			name:  "a shorter ledger window disables both sweeps",
			query: "720h", connection: "24h",
			wantBad: true,
		},
		{
			name:  "statements kept forever with an expiring ledger disables both",
			query: "0", connection: "8760h",
			wantBad: true,
		},
		{
			name:  "an unset query window with an expiring ledger disables both",
			query: "", connection: "8760h",
			wantBad: true,
		},
		{
			name:  "a malformed ledger window disables both",
			query: "720h", connection: "365d",
			wantBad: true,
		},
		{
			name:  "a malformed query window disables both",
			query: "30d", connection: "8760h",
			wantBad: true,
		},
		{
			name:  "a malformed query window disables both even with the ledger unset",
			query: "30d", connection: "",
			wantBad: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := &Config{
				QueryStorage: QueryStorageConfig{Retention: tt.query},
				Connection:   ConnectionConfig{Retention: tt.connection},
			}

			windows := cfg.RetentionWindows()

			if tt.wantBad {
				assert.NotEmpty(t, windows.Misconfiguration,
					"an incoherent pair must be reported, not silently applied")
				assert.Equal(t, time.Duration(0), windows.Query,
					"a misconfiguration disables the query sweep")
				assert.Equal(t, time.Duration(0), windows.Connection,
					"a misconfiguration disables the connection sweep too")
				assert.False(t, windows.Enabled())

				return
			}

			assert.Empty(t, windows.Misconfiguration)
			assert.Equal(t, tt.wantQuery, windows.Query)
			assert.Equal(t, tt.wantConnection, windows.Connection)
			assert.Equal(t, tt.wantQuery > 0 || tt.wantConnection > 0, windows.Enabled())
		})
	}
}

// TestRetentionWindowsMisconfigurationNamesBothValues keeps the WARN useful:
// an operator reading it has to be able to see which two settings disagree
// without going back to their deployment manifest.
func TestRetentionWindowsMisconfigurationNamesBothValues(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		QueryStorage: QueryStorageConfig{Retention: "720h"},
		Connection:   ConnectionConfig{Retention: "24h"},
	}

	msg := cfg.RetentionWindows().Misconfiguration
	require.NotEmpty(t, msg)
	assert.Contains(t, msg, "DBB_CONNECTION_RETENTION")
	assert.Contains(t, msg, "DBB_QUERY_STORAGE_RETENTION")
	assert.Contains(t, msg, "24h")
	assert.Contains(t, msg, "720h")
}
