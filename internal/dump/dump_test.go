package dump

import (
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gopacket/gopacket"
	"github.com/gopacket/gopacket/layers"
	"github.com/gopacket/gopacket/pcapgo"
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

	hdr := Header{
		SessionID:  uuid.New().String(),
		Protocol:   ProtocolOracle,
		StartTime:  time.Date(2026, 8, 2, 12, 0, 0, 123456789, time.UTC),
		Connection: map[string]any{"service_name": "S"},
	}

	// Measure the pcapng section/interface header so the cap can be expressed
	// as "header + room for exactly one packet".
	probe, err := NewWriter(path, hdr, 0)
	require.NoError(t, err)
	require.NoError(t, probe.Close())

	info, err := os.Stat(path)
	require.NoError(t, err)

	// A 100-byte payload becomes a 154-byte frame in a 200-byte packet block.
	w, err := NewWriter(path, hdr, info.Size()+250)
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

func TestReader_NotAPcapng(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "bad"+FileExt)
	require.NoError(t, os.WriteFile(path, []byte("this is not a capture file!!"), 0o644))

	_, err := OpenReader(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "read pcapng header")
}

func TestReader_MissingMetadata(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "nometa"+FileExt)

	// A structurally valid pcapng with no dbbat metadata in its SHB comment.
	f, err := os.Create(path)
	require.NoError(t, err)

	ng, err := pcapgo.NewNgWriter(f, layers.LinkTypeEthernet)
	require.NoError(t, err)
	require.NoError(t, ng.Flush())
	require.NoError(t, f.Close())

	_, err = OpenReader(path)
	assert.ErrorIs(t, err, ErrMissingMetadata)
}

func TestReader_CorruptMetadata(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "badmeta"+FileExt)

	f, err := os.Create(path)
	require.NoError(t, err)

	ng, err := pcapgo.NewNgWriterInterface(f, pcapgo.NgInterface{
		LinkType:            layers.LinkTypeEthernet,
		SnapLength:          snapLength,
		TimestampResolution: 9,
	}, pcapgo.NgWriterOptions{SectionInfo: pcapgo.NgSectionInfo{Comment: "not json"}})
	require.NoError(t, err)
	require.NoError(t, ng.Flush())
	require.NoError(t, f.Close())

	_, err = OpenReader(path)
	assert.ErrorIs(t, err, ErrMissingMetadata)
}

// TestWriter_SynthesizedHeaders locks in the wire-header synthesis rules that
// make the capture readable by tcpdump/Wireshark: real upstream addressing,
// direction-dependent endpoints, and monotonically advancing TCP sequences.
func TestWriter_SynthesizedHeaders(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "synth"+FileExt)

	w, err := NewWriter(path, Header{
		SessionID: uuid.New().String(),
		Protocol:  ProtocolPostgreSQL,
		StartTime: time.Now(),
		Connection: map[string]any{
			"upstream_addr": "192.0.2.44:6543",
		},
	}, 0)
	require.NoError(t, err)

	require.NoError(t, w.WritePacket(DirClientToServer, []byte("hello")))
	require.NoError(t, w.WritePacket(DirServerToClient, []byte("world!")))
	require.NoError(t, w.WritePacket(DirClientToServer, []byte("again")))
	require.NoError(t, w.Close())

	frames := readRawFrames(t, path)
	require.Len(t, frames, 3)

	// client -> server carries the real upstream IP/port as the destination
	assert.Equal(t, "10.77.0.1", frames[0].ip.SrcIP.String())
	assert.Equal(t, "192.0.2.44", frames[0].ip.DstIP.String())
	assert.Equal(t, layers.TCPPort(54321), frames[0].tcp.SrcPort)
	assert.Equal(t, layers.TCPPort(6543), frames[0].tcp.DstPort)
	assert.True(t, frames[0].tcp.PSH)
	assert.True(t, frames[0].tcp.ACK)

	// server -> client is the mirror image
	assert.Equal(t, "192.0.2.44", frames[1].ip.SrcIP.String())
	assert.Equal(t, "10.77.0.1", frames[1].ip.DstIP.String())
	assert.Equal(t, layers.TCPPort(6543), frames[1].tcp.SrcPort)

	// sequence numbers advance per direction, acks follow the peer
	assert.Equal(t, uint32(1), frames[0].tcp.Seq)
	assert.Equal(t, uint32(1), frames[1].tcp.Seq)
	assert.Equal(t, uint32(6), frames[1].tcp.Ack) // acks "hello"
	assert.Equal(t, uint32(6), frames[2].tcp.Seq) // 1 + len("hello")
	assert.Equal(t, uint32(7), frames[2].tcp.Ack) // acks "world!"
}

// TestWriter_LargePayloadSegmentation checks that payloads too large for a
// single IPv4 datagram are split into consecutive TCP segments.
func TestWriter_LargePayloadSegmentation(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "big"+FileExt)

	w, err := NewWriter(path, Header{
		SessionID:  uuid.New().String(),
		Protocol:   ProtocolMySQL,
		StartTime:  time.Now(),
		Connection: map[string]any{},
	}, 0)
	require.NoError(t, err)

	payload := make([]byte, maxTCPPayload+1000)
	for i := range payload {
		payload[i] = byte(i)
	}

	require.NoError(t, w.WritePacket(DirServerToClient, payload))
	require.NoError(t, w.Close())

	r, err := OpenReader(path)
	require.NoError(t, err)

	defer func() { _ = r.Close() }()

	var got []byte

	segments := 0

	for {
		pkt, err := r.ReadPacket()
		if err != nil {
			require.ErrorIs(t, err, io.EOF)

			break
		}

		assert.Equal(t, DirServerToClient, pkt.Direction)

		got = append(got, pkt.Data...)
		segments++
	}

	assert.Equal(t, 2, segments)
	assert.Equal(t, payload, got)
}

type rawFrame struct {
	ip  layers.IPv4
	tcp layers.TCP
}

// readRawFrames decodes the synthesized link/network/transport headers straight
// out of the pcapng file, bypassing the dump Reader.
func readRawFrames(t *testing.T, path string) []rawFrame {
	t.Helper()

	f, err := os.Open(path)
	require.NoError(t, err)

	defer func() { _ = f.Close() }()

	ng, err := pcapgo.NewNgReader(f, pcapgo.DefaultNgReaderOptions)
	require.NoError(t, err)

	var out []rawFrame

	for {
		data, _, err := ng.ReadPacketData()
		if err != nil {
			require.ErrorIs(t, err, io.EOF)

			break
		}

		var (
			eth layers.Ethernet
			ip  layers.IPv4
			tcp layers.TCP
		)

		decoded := []gopacket.LayerType{}
		parser := gopacket.NewDecodingLayerParser(layers.LayerTypeEthernet, &eth, &ip, &tcp)
		parser.IgnoreUnsupported = true
		require.NoError(t, parser.DecodeLayers(data, &decoded))

		out = append(out, rawFrame{ip: ip, tcp: tcp})
	}

	return out
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

	// Create an old legacy-extension file (pre-pcapng leftover): should be reaped too
	oldLegacyPath := filepath.Join(dir, uuid.New().String()+legacyFileExt)
	require.NoError(t, os.WriteFile(oldLegacyPath, []byte("legacy"), 0o644))
	oldLegacy := time.Now().Add(-2 * time.Hour)
	require.NoError(t, os.Chtimes(oldLegacyPath, oldLegacy, oldLegacy))

	// Create a recent legacy-extension file: should be left alone
	recentLegacyPath := filepath.Join(dir, uuid.New().String()+legacyFileExt)
	require.NoError(t, os.WriteFile(recentLegacyPath, []byte("legacy recent"), 0o644))

	// Create a non-dump file (should be ignored)
	nonDump := filepath.Join(dir, "notes.txt")
	require.NoError(t, os.WriteFile(nonDump, []byte("note"), 0o644))

	deleted, err := CleanupOldFiles(dir, 1*time.Hour)
	require.NoError(t, err)
	assert.Equal(t, 4, deleted)

	entries, _ := os.ReadDir(dir)
	assert.Len(t, entries, 3)

	remaining := make(map[string]bool)
	for _, e := range entries {
		remaining[e.Name()] = true
	}
	assert.True(t, remaining[filepath.Base(recentPath)], "recent .pcapng file should survive")
	assert.True(t, remaining[filepath.Base(recentLegacyPath)], "recent legacy file should survive")
	assert.True(t, remaining["notes.txt"], "non-dump file should survive")
	assert.False(t, remaining[filepath.Base(oldLegacyPath)], "old legacy file should be reaped")
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

// TestWriter_EpbFlagsDirection asserts the direction really rides in the pcapng
// epb_flags option, independently of the reader's source-port fallback.
func TestWriter_EpbFlagsDirection(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "flags"+FileExt)

	w, err := NewWriter(path, Header{
		SessionID:  uuid.New().String(),
		Protocol:   ProtocolMongo,
		StartTime:  time.Now(),
		Connection: map[string]any{},
	}, 0)
	require.NoError(t, err)
	require.NoError(t, w.WritePacket(DirClientToServer, []byte{1}))
	require.NoError(t, w.WritePacket(DirServerToClient, []byte{2}))
	require.NoError(t, w.Close())

	f, err := os.Open(path)
	require.NoError(t, err)

	defer func() { _ = f.Close() }()

	ng, err := pcapgo.NewNgReader(f, pcapgo.DefaultNgReaderOptions)
	require.NoError(t, err)

	var directions []pcapgo.NgEpbFlag

	for {
		_, _, opts, err := ng.ReadPacketDataWithOptions()
		if err != nil {
			require.ErrorIs(t, err, io.EOF)

			break
		}

		require.NotNil(t, opts.Flags, "every packet must carry epb_flags")
		directions = append(directions, opts.Flags.Direction)
	}

	assert.Equal(t, []pcapgo.NgEpbFlag{
		pcapgo.NgEpbFlagDirectionInbound,
		pcapgo.NgEpbFlagDirectionOutbound,
	}, directions)
}

// TestReader_DirectionFromFlagsOverridesAddressing proves the reader trusts
// epb_flags rather than inferring direction from the synthesized addressing.
func TestReader_DirectionFromFlagsOverridesAddressing(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "flagsonly"+FileExt)

	f, err := os.Create(path)
	require.NoError(t, err)

	metadata := `{"session_id":"s","protocol":"mysql","start_time":"2026-08-02T00:00:00Z","connection":{}}`

	ng, err := pcapgo.NewNgWriterInterface(f, pcapgo.NgInterface{
		LinkType:            layers.LinkTypeEthernet,
		SnapLength:          snapLength,
		TimestampResolution: 9,
	}, pcapgo.NgWriterOptions{SectionInfo: pcapgo.NgSectionInfo{Comment: metadata}})
	require.NoError(t, err)

	// Frame the payload as if it came *from* the client endpoint, but flag it
	// as server->client. The flag must win.
	fr := newFramer(newEndpoints(Header{Protocol: ProtocolMySQL}, true))
	frames, err := fr.frames(DirClientToServer, []byte{0xAA})
	require.NoError(t, err)
	require.Len(t, frames, 1)

	require.NoError(t, ng.WritePacketWithOptions(gopacket.CaptureInfo{
		Timestamp:     time.Date(2026, 8, 2, 0, 0, 0, 0, time.UTC),
		CaptureLength: len(frames[0]),
		Length:        len(frames[0]),
	}, frames[0], pcapgo.NgPacketOptions{
		Flags: &pcapgo.NgEpbFlags{Direction: pcapgo.NgEpbFlagDirectionOutbound},
	}))
	require.NoError(t, ng.Flush())
	require.NoError(t, f.Close())

	r, err := OpenReader(path)
	require.NoError(t, err)

	defer func() { _ = r.Close() }()

	pkt, err := r.ReadPacket()
	require.NoError(t, err)
	assert.Equal(t, DirServerToClient, pkt.Direction)
	assert.Equal(t, []byte{0xAA}, pkt.Data)
}
