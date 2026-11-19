package dump_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/dump"
	"github.com/fclairamb/dbbat/internal/dump/decode"
)

func newHeader() dump.Header {
	return dump.Header{
		SessionID: uuid.New().String(),
		Protocol:  dump.ProtocolPostgreSQL,
		StartTime: time.Now(),
		Connection: map[string]any{
			"upstream_addr": "10.0.0.1:5432",
		},
	}
}

func fileMode(t *testing.T, path string) os.FileMode {
	t.Helper()

	info, err := os.Stat(path)
	require.NoError(t, err)

	return info.Mode().Perm()
}

// TestNewWriter_PrivateMode checks a capture is created 0600: it holds
// plaintext statements and result rows, so a 0644 file (os.Create minus the
// usual umask) let any local account read every captured session.
func TestNewWriter_PrivateMode(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "session"+dump.FileExt)

	w, err := dump.NewWriter(path, newHeader(), 0)
	require.NoError(t, err)
	require.NoError(t, w.Close())

	assert.Equal(t, dump.FilePerm, fileMode(t, path))
	assert.Equal(t, dump.FilePerm, os.FileMode(0o600))
}

// TestNewWriter_RefusesExistingFile checks a live capture never writes over
// (or through a planted link to) a file already at its path.
func TestNewWriter_RefusesExistingFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "session"+dump.FileExt)
	require.NoError(t, os.WriteFile(path, []byte("keep me"), 0o644))

	_, err := dump.NewWriter(path, newHeader(), 0)
	require.ErrorIs(t, err, os.ErrExist)

	content, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "keep me", string(content))
}

// TestNewWriter_PrivateCaptureStillReadable is the positive control: a 0600
// capture still round-trips through the reader, anonymise and decode.
func TestNewWriter_PrivateCaptureStillReadable(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "session"+dump.FileExt)

	w, err := dump.NewWriter(path, newHeader(), 0)
	require.NoError(t, err)

	// A PostgreSQL StartupMessage (int32 length, protocol 3.0, user=x)...
	startup := append([]byte{0, 0, 0, 16, 0, 3, 0, 0}, []byte("user\x00x\x00\x00")...)
	require.NoError(t, w.WritePacket(dump.DirClientToServer, startup))

	// ...then a simple Query message: 'Q', int32 length, "SELECT 1\0".
	query := append([]byte{'Q', 0, 0, 0, 13}, []byte("SELECT 1\x00")...)
	require.NoError(t, w.WritePacket(dump.DirClientToServer, query))
	require.NoError(t, w.Close())

	r, err := dump.OpenReader(path)
	require.NoError(t, err)

	pkt, err := r.ReadPacket()
	require.NoError(t, err)
	assert.Equal(t, startup, pkt.Data)
	pkt, err = r.ReadPacket()
	require.NoError(t, err)
	assert.Equal(t, query, pkt.Data)
	require.NoError(t, r.Close())

	// Anonymise: the output is private too, including when it replaces an
	// earlier, world-readable output (re-running the CLI must still work).
	out := filepath.Join(dir, "session.anonymised"+dump.FileExt)
	require.NoError(t, os.WriteFile(out, []byte("stale output"), 0o644))
	require.NoError(t, dump.Anonymise(path, out, true))
	assert.Equal(t, dump.FilePerm, fileMode(t, out))

	ar, err := dump.OpenReader(out)
	require.NoError(t, err)

	apkt, err := ar.ReadPacket()
	require.NoError(t, err)
	assert.Equal(t, startup, apkt.Data)
	require.NoError(t, ar.Close())

	// Decode opens both.
	for _, p := range []string{path, out} {
		var buf bytes.Buffer
		require.NoError(t, decode.File(p, decode.Options{}, &buf))
		assert.Contains(t, buf.String(), "SELECT 1")
	}
}
