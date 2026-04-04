package oracle

import (
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDumpWriter_WritesHeader(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "test"+dumpFileExt)
	uid := uuid.New()

	w, err := NewDumpWriter(path, uid, "ORCL", "10.0.0.1:1521", 0)
	require.NoError(t, err)
	require.NoError(t, w.Close())

	r, err := OpenDump(path)
	require.NoError(t, err)
	defer func() { _ = r.Close() }()

	assert.Equal(t, uint16(dumpVersion), r.Header.Version)
	assert.Equal(t, uid, r.Header.SessionUID)
	assert.Equal(t, "ORCL", r.Header.ServiceName)
	assert.Equal(t, "10.0.0.1:1521", r.Header.UpstreamAddr)
	assert.False(t, r.Header.StartTime.IsZero())
}

func TestDumpWriter_WritePacket(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "test"+dumpFileExt)

	w, err := NewDumpWriter(path, uuid.New(), "SVC", "host:1521", 0)
	require.NoError(t, err)

	data := []byte{0x00, 0x10, 0x00, 0x00, 0x06, 0x00, 0x00, 0x00}
	require.NoError(t, w.WritePacket(DumpDirClientToServer, data))
	require.NoError(t, w.Close())

	r, err := OpenDump(path)
	require.NoError(t, err)
	defer func() { _ = r.Close() }()

	pkt, err := r.ReadPacket()
	require.NoError(t, err)
	assert.Equal(t, DumpDirClientToServer, pkt.Direction)
	assert.Equal(t, data, pkt.Data)
	assert.GreaterOrEqual(t, pkt.RelativeNs, int64(0))

	// Next read should hit EOF marker
	_, err = r.ReadPacket()
	assert.ErrorIs(t, err, io.EOF)
}

func TestDumpWriter_MaxSize(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "test"+dumpFileExt)

	// Use a very small max size so we can trigger the limit
	w, err := NewDumpWriter(path, uuid.New(), "S", "h:1", 200)
	require.NoError(t, err)

	bigData := make([]byte, 100)
	require.NoError(t, w.WritePacket(0, bigData))

	// This should be silently dropped (would exceed max size)
	require.NoError(t, w.WritePacket(0, bigData))
	require.NoError(t, w.Close())

	r, err := OpenDump(path)
	require.NoError(t, err)
	defer func() { _ = r.Close() }()

	// Should only get one packet
	_, err = r.ReadPacket()
	require.NoError(t, err)

	_, err = r.ReadPacket()
	assert.ErrorIs(t, err, io.EOF)
}

func TestDumpWriter_RoundTrip(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "test"+dumpFileExt)
	uid := uuid.New()

	w, err := NewDumpWriter(path, uid, "PROD_SVC", "db.internal:1521", 0)
	require.NoError(t, err)

	packets := []struct {
		dir  byte
		data []byte
	}{
		{DumpDirClientToServer, []byte{1, 2, 3}},
		{DumpDirServerToClient, []byte{4, 5, 6, 7, 8}},
		{DumpDirClientToServer, []byte{9}},
		{DumpDirServerToClient, []byte{10, 11}},
	}

	for _, p := range packets {
		require.NoError(t, w.WritePacket(p.dir, p.data))
	}

	require.NoError(t, w.Close())

	r, err := OpenDump(path)
	require.NoError(t, err)
	defer func() { _ = r.Close() }()

	assert.Equal(t, uid, r.Header.SessionUID)
	assert.Equal(t, "PROD_SVC", r.Header.ServiceName)
	assert.Equal(t, "db.internal:1521", r.Header.UpstreamAddr)

	var prevNs int64
	for i, expected := range packets {
		pkt, err := r.ReadPacket()
		require.NoError(t, err, "packet %d", i)
		assert.Equal(t, expected.dir, pkt.Direction, "packet %d direction", i)
		assert.Equal(t, expected.data, pkt.Data, "packet %d data", i)
		assert.GreaterOrEqual(t, pkt.RelativeNs, prevNs, "packet %d timestamp should be non-decreasing", i)
		prevNs = pkt.RelativeNs
	}

	_, err = r.ReadPacket()
	assert.ErrorIs(t, err, io.EOF)
}

func TestDumpReader_InvalidFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "bad"+dumpFileExt)
	require.NoError(t, os.WriteFile(path, []byte("this is not a dump file!!"), 0o644))

	_, err := OpenDump(path)
	assert.ErrorIs(t, err, ErrInvalidDumpMagic)
}

func TestCleanupOldDumps(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	// Create some "old" dump files
	for range 3 {
		name := filepath.Join(dir, uuid.New().String()+dumpFileExt)
		require.NoError(t, os.WriteFile(name, []byte("fake"), 0o644))
		// Set mod time to 2 hours ago
		old := time.Now().Add(-2 * time.Hour)
		require.NoError(t, os.Chtimes(name, old, old))
	}

	// Create a recent dump file
	recentPath := filepath.Join(dir, uuid.New().String()+dumpFileExt)
	require.NoError(t, os.WriteFile(recentPath, []byte("recent"), 0o644))

	// Create a non-dump file (should be ignored)
	nonDump := filepath.Join(dir, "notes.txt")
	require.NoError(t, os.WriteFile(nonDump, []byte("note"), 0o644))

	deleted, err := CleanupOldDumps(dir, 1*time.Hour)
	require.NoError(t, err)
	assert.Equal(t, 3, deleted)

	// Recent dump and non-dump should still exist
	entries, _ := os.ReadDir(dir)
	assert.Len(t, entries, 2)
}
