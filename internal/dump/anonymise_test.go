package dump

import (
	"errors"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gopacket/gopacket/layers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAnonymise_StripsConnectionMetadata(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	inputPath := filepath.Join(dir, "input"+FileExt)
	outputPath := filepath.Join(dir, "output"+FileExt)

	sid := uuid.New().String()

	// Write a dump with connection metadata
	w, err := NewWriter(inputPath, Header{
		SessionID: sid,
		Protocol:  ProtocolPostgreSQL,
		StartTime: time.Now(),
		Connection: map[string]any{
			"database":      "myapp",
			"user":          "admin",
			"upstream_addr": "db.internal:5432",
			"password":      "supersecret",
		},
	}, 0)
	require.NoError(t, err)

	require.NoError(t, w.WritePacket(DirClientToServer, []byte{1, 2, 3}))
	require.NoError(t, w.WritePacket(DirServerToClient, []byte{4, 5, 6, 7}))
	require.NoError(t, w.Close())

	// Anonymise
	require.NoError(t, Anonymise(inputPath, outputPath, true))

	// Verify anonymised dump
	r, err := OpenReader(outputPath)
	require.NoError(t, err)
	defer func() { _ = r.Close() }()

	hdr := r.Header()
	assert.Equal(t, sid, hdr.SessionID)
	assert.Equal(t, ProtocolPostgreSQL, hdr.Protocol)
	assert.Empty(t, hdr.Connection)
	assert.True(t, hdr.StartTime.Equal(time.Unix(0, 0)), "capture should be rebased onto the Unix epoch")

	// Verify packets are preserved
	pkt1, err := r.ReadPacket()
	require.NoError(t, err)
	assert.Equal(t, DirClientToServer, pkt1.Direction)
	assert.Equal(t, []byte{1, 2, 3}, pkt1.Data)

	pkt2, err := r.ReadPacket()
	require.NoError(t, err)
	assert.Equal(t, DirServerToClient, pkt2.Direction)
	assert.Equal(t, []byte{4, 5, 6, 7}, pkt2.Data)

	_, err = r.ReadPacket()
	assert.ErrorIs(t, err, io.EOF)
}

func TestAnonymise_PreservesTimestampOrder(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	inputPath := filepath.Join(dir, "input"+FileExt)
	outputPath := filepath.Join(dir, "output"+FileExt)

	w, err := NewWriter(inputPath, Header{
		SessionID:  uuid.New().String(),
		Protocol:   ProtocolOracle,
		StartTime:  time.Now(),
		Connection: map[string]any{"service_name": "ORCL"},
	}, 0)
	require.NoError(t, err)

	for range 5 {
		require.NoError(t, w.WritePacket(DirClientToServer, []byte{0xAA}))
	}

	require.NoError(t, w.Close())

	require.NoError(t, Anonymise(inputPath, outputPath, true))

	r, err := OpenReader(outputPath)
	require.NoError(t, err)
	defer func() { _ = r.Close() }()

	var prevNs int64

	for i := range 5 {
		pkt, err := r.ReadPacket()
		require.NoError(t, err, "packet %d", i)
		assert.GreaterOrEqual(t, pkt.RelativeNs, prevNs, "timestamps should be non-decreasing")

		prevNs = pkt.RelativeNs
	}
}

func TestAnonymise_OracleTestdata(t *testing.T) {
	t.Parallel()

	testFiles := []string{
		"../../internal/proxy/oracle/testdata/go_ora" + FileExt,
		"../../internal/proxy/oracle/testdata/python_thin" + FileExt,
	}

	for _, tf := range testFiles {
		// Check if testdata exists (skip if not)
		absPath, err := filepath.Abs(tf)
		if err != nil {
			t.Skipf("cannot resolve path %s: %v", tf, err)
		}

		t.Run(filepath.Base(tf), func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			outputPath := filepath.Join(dir, "anonymised"+FileExt)

			err := Anonymise(absPath, outputPath, true)
			if err != nil {
				t.Skipf("testdata not available: %v", err)
			}

			// Verify header is stripped
			r, err := OpenReader(outputPath)
			require.NoError(t, err)
			defer func() { _ = r.Close() }()

			hdr := r.Header()
			assert.NotEmpty(t, hdr.SessionID)
			assert.Equal(t, ProtocolOracle, hdr.Protocol)
			assert.Empty(t, hdr.Connection)

			// Verify packets are readable
			count := 0
			for {
				_, err := r.ReadPacket()
				if errors.Is(err, io.EOF) {
					break
				}

				require.NoError(t, err)

				count++
			}

			assert.Positive(t, count, "should have at least one packet")
		})
	}
}

// TestAnonymise_RewritesSynthesizedAddresses proves the default path scrubs the
// upstream host and port that the synthesized IP/TCP headers otherwise encode.
func TestAnonymise_RewritesSynthesizedAddresses(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	inputPath := filepath.Join(dir, "input"+FileExt)
	outputPath := filepath.Join(dir, "output"+FileExt)

	w, err := NewWriter(inputPath, Header{
		SessionID: uuid.New().String(),
		Protocol:  ProtocolPostgreSQL,
		StartTime: time.Now(),
		Connection: map[string]any{
			"upstream_addr": "203.0.113.9:6543",
		},
	}, 0)
	require.NoError(t, err)
	require.NoError(t, w.WritePacket(DirClientToServer, []byte{1, 2, 3}))
	require.NoError(t, w.WritePacket(DirServerToClient, []byte{4, 5}))
	require.NoError(t, w.Close())

	// The original capture leaks the upstream endpoint through the headers.
	before := readRawFrames(t, inputPath)
	require.Len(t, before, 2)
	assert.Equal(t, "203.0.113.9", before[0].ip.DstIP.String())
	assert.Equal(t, layers.TCPPort(6543), before[0].tcp.DstPort)

	require.NoError(t, Anonymise(inputPath, outputPath, true))

	after := readRawFrames(t, outputPath)
	require.Len(t, after, 2)
	assert.Equal(t, "10.77.0.1", after[0].ip.SrcIP.String())
	assert.Equal(t, "10.77.0.2", after[0].ip.DstIP.String())
	assert.Equal(t, layers.TCPPort(5432), after[0].tcp.DstPort, "falls back to the protocol default port")
	assert.Equal(t, "10.77.0.2", after[1].ip.SrcIP.String())
	assert.Equal(t, layers.TCPPort(5432), after[1].tcp.SrcPort)
}

// TestAnonymise_KeepAddresses covers the opt-out: metadata is still stripped but
// the synthesized addressing is carried over untouched.
func TestAnonymise_KeepAddresses(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	inputPath := filepath.Join(dir, "input"+FileExt)
	outputPath := filepath.Join(dir, "output"+FileExt)

	w, err := NewWriter(inputPath, Header{
		SessionID: uuid.New().String(),
		Protocol:  ProtocolPostgreSQL,
		StartTime: time.Now(),
		Connection: map[string]any{
			"upstream_addr": "203.0.113.9:6543",
		},
	}, 0)
	require.NoError(t, err)
	require.NoError(t, w.WritePacket(DirClientToServer, []byte{1, 2, 3}))
	require.NoError(t, w.Close())

	require.NoError(t, Anonymise(inputPath, outputPath, false))

	after := readRawFrames(t, outputPath)
	require.Len(t, after, 1)
	assert.Equal(t, "203.0.113.9", after[0].ip.DstIP.String())
	assert.Equal(t, layers.TCPPort(6543), after[0].tcp.DstPort)

	r, err := OpenReader(outputPath)
	require.NoError(t, err)

	defer func() { _ = r.Close() }()

	assert.Empty(t, r.Header().Connection, "metadata is stripped either way")
}
