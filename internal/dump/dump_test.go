package dump

import (
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWriter_Header(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "test"+FileExt)

	hdr := Header{
		SessionID: uuid.New().String(),
		Protocol:  ProtocolOracle,
		StartTime: time.Now(),
		Connection: map[string]any{
			"service_name":  "ORCL",
			"upstream_addr": "10.0.0.1:1521",
		},
	}

	w, err := NewWriter(path, hdr, 0)
	require.NoError(t, err)
	require.NoError(t, w.Close())

	r, err := OpenReader(path)
	require.NoError(t, err)
	defer func() { _ = r.Close() }()

	got := r.Header()
	assert.Equal(t, hdr.SessionID, got.SessionID)
	assert.Equal(t, hdr.Protocol, got.Protocol)
	assert.Equal(t, hdr.StartTime.UnixNano(), got.StartTime.UnixNano())
	assert.Equal(t, "ORCL", got.Connection["service_name"])
	assert.Equal(t, "10.0.0.1:1521", got.Connection["upstream_addr"])
}

func TestWriter_WritePacket(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "test"+FileExt)

	w, err := NewWriter(path, Header{
		SessionID:  uuid.New().String(),
		Protocol:   ProtocolOracle,
		StartTime:  time.Now(),
		Connection: map[string]any{},
	}, 0)
	require.NoError(t, err)

	data := []byte{0x00, 0x10, 0x00, 0x00, 0x06, 0x00, 0x00, 0x00}
	require.NoError(t, w.WritePacket(DirClientToServer, data))
	require.NoError(t, w.Close())

	r, err := OpenReader(path)
	require.NoError(t, err)
	defer func() { _ = r.Close() }()

	pkt, err := r.ReadPacket()
	require.NoError(t, err)
	assert.Equal(t, DirClientToServer, pkt.Direction)
	assert.Equal(t, data, pkt.Data)
	assert.GreaterOrEqual(t, pkt.RelativeNs, int64(0))

	_, err = r.ReadPacket()
	assert.ErrorIs(t, err, io.EOF)
}

func TestWriter_MaxSize(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "test"+FileExt)

	w, err := NewWriter(path, Header{
		SessionID:  uuid.New().String(),
		Protocol:   ProtocolOracle,
		StartTime:  time.Now(),
		Connection: map[string]any{"service_name": "S"},
	}, 300)
	require.NoError(t, err)

	bigData := make([]byte, 100)
	require.NoError(t, w.WritePacket(0, bigData))

	// This should be silently dropped (would exceed max size)
	require.NoError(t, w.WritePacket(0, bigData))
	require.NoError(t, w.Close())

	r, err := OpenReader(path)
	require.NoError(t, err)
	defer func() { _ = r.Close() }()

	_, err = r.ReadPacket()
	require.NoError(t, err)

	_, err = r.ReadPacket()
	assert.ErrorIs(t, err, io.EOF)
}

func TestWriter_RoundTrip(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "test"+FileExt)
	sid := uuid.New().String()

	hdr := Header{
		SessionID: sid,
		Protocol:  ProtocolPostgreSQL,
		StartTime: time.Now(),
		Connection: map[string]any{
			"database":      "myapp",
			"user":          "readonly",
			"upstream_addr": "pg.internal:5432",
		},
	}

	w, err := NewWriter(path, hdr, 0)
	require.NoError(t, err)

	packets := []struct {
		dir  byte
		data []byte
	}{
		{DirClientToServer, []byte{1, 2, 3}},
		{DirServerToClient, []byte{4, 5, 6, 7, 8}},
		{DirClientToServer, []byte{9}},
		{DirServerToClient, []byte{10, 11}},
	}

	for _, p := range packets {
		require.NoError(t, w.WritePacket(p.dir, p.data))
	}

	require.NoError(t, w.Close())

	r, err := OpenReader(path)
	require.NoError(t, err)
	defer func() { _ = r.Close() }()

	got := r.Header()
	assert.Equal(t, sid, got.SessionID)
	assert.Equal(t, ProtocolPostgreSQL, got.Protocol)
	assert.Equal(t, "myapp", got.Connection["database"])

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

func TestReader_InvalidMagic(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "bad"+FileExt)
	require.NoError(t, os.WriteFile(path, []byte("this is not a dump file!!"), 0o644))

	_, err := OpenReader(path)
	assert.ErrorIs(t, err, ErrInvalidMagic)
}

func TestReader_UnsupportedVersion(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "badver"+FileExt)

	// Write valid magic + invalid version
	data := []byte(magic)
	data = append(data, 0x00, 0x63) // version 99

	require.NoError(t, os.WriteFile(path, data, 0o644))

	_, err := OpenReader(path)
	assert.ErrorIs(t, err, ErrUnsupportedVersion)
}

func TestReader_EmptyConnection(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "test"+FileExt)

	w, err := NewWriter(path, Header{
		SessionID:  uuid.New().String(),
		Protocol:   ProtocolOracle,
		StartTime:  time.Now(),
		Connection: map[string]any{},
	}, 0)
	require.NoError(t, err)
	require.NoError(t, w.Close())

	r, err := OpenReader(path)
	require.NoError(t, err)
	defer func() { _ = r.Close() }()

	assert.Empty(t, r.Header().Connection)
}

func TestReader_ExtraJSONFields(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "test"+FileExt)

	hdr := Header{
		SessionID: uuid.New().String(),
		Protocol:  ProtocolOracle,
		StartTime: time.Now(),
		Connection: map[string]any{
			"service_name":  "ORCL",
			"upstream_addr": "10.0.0.1:1521",
			"custom_field":  "custom_value",
			"numeric_field": float64(42),
		},
	}

	w, err := NewWriter(path, hdr, 0)
	require.NoError(t, err)
	require.NoError(t, w.Close())

	r, err := OpenReader(path)
	require.NoError(t, err)
	defer func() { _ = r.Close() }()

	got := r.Header()
	assert.Equal(t, "custom_value", got.Connection["custom_field"])
	assert.InDelta(t, float64(42), got.Connection["numeric_field"], 0)
}

func TestCleanupOldFiles(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	// Create some "old" dump files
	for range 3 {
		name := filepath.Join(dir, uuid.New().String()+FileExt)
		require.NoError(t, os.WriteFile(name, []byte("fake"), 0o644))
		old := time.Now().Add(-2 * time.Hour)
		require.NoError(t, os.Chtimes(name, old, old))
	}

	// Create a recent dump file
	recentPath := filepath.Join(dir, uuid.New().String()+FileExt)
	require.NoError(t, os.WriteFile(recentPath, []byte("recent"), 0o644))

	// Create a non-dump file (should be ignored)
	nonDump := filepath.Join(dir, "notes.txt")
	require.NoError(t, os.WriteFile(nonDump, []byte("note"), 0o644))

	deleted, err := CleanupOldFiles(dir, 1*time.Hour)
	require.NoError(t, err)
	assert.Equal(t, 3, deleted)

	entries, _ := os.ReadDir(dir)
	assert.Len(t, entries, 2)
}

func TestWriter_ConcurrentWrites(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "test"+FileExt)

	w, err := NewWriter(path, Header{
		SessionID:  uuid.New().String(),
		Protocol:   ProtocolOracle,
		StartTime:  time.Now(),
		Connection: map[string]any{},
	}, 0)
	require.NoError(t, err)

	const goroutines = 10
	const packetsPerGoroutine = 50

	var wg sync.WaitGroup
	wg.Add(goroutines)

	for range goroutines {
		go func() {
			defer wg.Done()
			for range packetsPerGoroutine {
				_ = w.WritePacket(DirClientToServer, []byte{1, 2, 3})
			}
		}()
	}

	wg.Wait()
	require.NoError(t, w.Close())

	// Verify all packets are readable
	r, err := OpenReader(path)
	require.NoError(t, err)
	defer func() { _ = r.Close() }()

	count := 0
	for {
		_, err := r.ReadPacket()
		if err != nil {
			require.ErrorIs(t, err, io.EOF)
			break
		}
		count++
	}

	assert.Equal(t, goroutines*packetsPerGoroutine, count)
}
