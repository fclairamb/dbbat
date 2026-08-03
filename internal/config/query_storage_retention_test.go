package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestQueryStorageRetentionDefaultsToDisabled pins the safety-critical default:
// upgrading dbbat must not start deleting audit history.
func TestQueryStorageRetentionDefaultsToDisabled(t *testing.T) {
	t.Setenv("DBB_DSN", "postgres://x:x@localhost/x")
	t.Setenv("DBB_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")

	cfg, err := Load(LoadOptions{})
	require.NoError(t, err)

	assert.Equal(t, DefaultQueryStorageRetention, cfg.QueryStorage.Retention)
	assert.Equal(t, time.Duration(0), cfg.QueryStorage.RetentionDuration())
}

func TestQueryStorageRetentionEnvVar(t *testing.T) {
	t.Setenv("DBB_DSN", "postgres://x:x@localhost/x")
	t.Setenv("DBB_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	t.Setenv("DBB_QUERY_STORAGE_RETENTION", "720h")

	cfg, err := Load(LoadOptions{})
	require.NoError(t, err)

	assert.Equal(t, "720h", cfg.QueryStorage.Retention)
	assert.Equal(t, 720*time.Hour, cfg.QueryStorage.RetentionDuration())
}

func TestQueryStorageRetentionDuration(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		value string
		want  time.Duration
	}{
		{"empty keeps forever", "", 0},
		{"explicit zero keeps forever", "0", 0},
		{"malformed keeps forever", "30d", 0},
		{"negative keeps forever", "-1h", 0},
		{"valid duration", "24h", 24 * time.Hour},
		{"composite duration", "1h30m", 90 * time.Minute},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := QueryStorageConfig{Retention: tt.value}
			assert.Equal(t, tt.want, cfg.RetentionDuration())
		})
	}
}

func TestQueryStorageRetentionMisconfigured(t *testing.T) {
	t.Parallel()

	assert.False(t, QueryStorageConfig{Retention: ""}.RetentionMisconfigured())
	assert.False(t, QueryStorageConfig{Retention: "0"}.RetentionMisconfigured())
	assert.False(t, QueryStorageConfig{Retention: "720h"}.RetentionMisconfigured())
	assert.True(t, QueryStorageConfig{Retention: "30d"}.RetentionMisconfigured(),
		"Go durations have no day unit; the operator meant to enable retention")
	assert.True(t, QueryStorageConfig{Retention: "-1h"}.RetentionMisconfigured())
}
