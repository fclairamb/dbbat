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
	t.Setenv("DBB_DUMP_UPLOAD_URL", "s3://captures/dbbat")

	cfg, err := Load(LoadOptions{})
	require.NoError(t, err)

	assert.Equal(t, "/tmp/test-dumps", cfg.Dump.Dir)
	assert.Equal(t, int64(999), cfg.Dump.MaxSize)
	assert.Equal(t, "48h", cfg.Dump.Retention)
	assert.Equal(t, "s3://captures/dbbat", cfg.Dump.UploadURL)
}

// Uploading with no local spool would capture nothing at all: captures are
// always written to disk first and uploaded once complete, so the combination
// is a misconfiguration rather than a degraded mode.
func TestDumpUploadURLRequiresDumpDir(t *testing.T) {
	t.Setenv("DBB_DSN", "postgres://x:x@localhost/x")
	t.Setenv("DBB_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	t.Setenv("DBB_DUMP_UPLOAD_URL", "s3://captures/dbbat")

	_, err := Load(LoadOptions{})
	require.ErrorIs(t, err, ErrDumpUploadNeedsDir)
}

// The default is unchanged local-only capture storage.
func TestDumpUploadURLDefaultsToEmpty(t *testing.T) {
	t.Setenv("DBB_DSN", "postgres://x:x@localhost/x")
	t.Setenv("DBB_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")

	cfg, err := Load(LoadOptions{})
	require.NoError(t, err)

	assert.Empty(t, cfg.Dump.UploadURL)
}
