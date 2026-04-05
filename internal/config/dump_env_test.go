package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDumpEnvVars(t *testing.T) {
	t.Setenv("DBB_DSN", "postgres://x:x@localhost/x")
	t.Setenv("DBB_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	t.Setenv("DBB_DUMP_DIR", "/tmp/test-dumps")
	t.Setenv("DBB_DUMP_MAX_SIZE", "999")
	t.Setenv("DBB_DUMP_RETENTION", "48h")

	cfg, err := Load(LoadOptions{})
	require.NoError(t, err)

	assert.Equal(t, "/tmp/test-dumps", cfg.Dump.Dir)
	assert.Equal(t, int64(999), cfg.Dump.MaxSize)
	assert.Equal(t, "48h", cfg.Dump.Retention)
}
