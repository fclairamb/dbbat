package main

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/gopacket/gopacket"
	"github.com/gopacket/gopacket/layers"
	"github.com/gopacket/gopacket/pcapgo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/urfave/cli/v3"

	"github.com/fclairamb/dbbat/internal/dump"
)

// writeCapture produces a one-packet capture whose synthesized headers encode
// the given upstream address.
func writeCapture(t *testing.T, path, upstreamAddr string) {
	t.Helper()

	w, err := dump.NewWriter(path, dump.Header{
		SessionID: "11111111-2222-3333-4444-555555555555",
		Protocol:  dump.ProtocolPostgreSQL,
		StartTime: time.Now(),
		Connection: map[string]any{
			"user":          "readonly",
			"upstream_addr": upstreamAddr,
		},
	}, 0)
	require.NoError(t, err)
	require.NoError(t, w.WritePacket(dump.DirClientToServer, []byte{1, 2, 3}))
	require.NoError(t, w.Close())
}

// serverEndpoint returns the destination IP:port of the first client→server
// frame in a capture, i.e. the addressing that `--keep-addresses` governs.
func serverEndpoint(t *testing.T, path string) string {
	t.Helper()

	f, err := os.Open(path)
	require.NoError(t, err)

	defer func() { _ = f.Close() }()

	ng, err := pcapgo.NewNgReader(f, pcapgo.DefaultNgReaderOptions)
	require.NoError(t, err)

	data, _, err := ng.ReadPacketData()
	require.NoError(t, err)

	var (
		eth layers.Ethernet
		ip  layers.IPv4
		tcp layers.TCP
	)

	decoded := []gopacket.LayerType{}
	parser := gopacket.NewDecodingLayerParser(layers.LayerTypeEthernet, &eth, &ip, &tcp)
	parser.IgnoreUnsupported = true
	require.NoError(t, parser.DecodeLayers(data, &decoded))

	return net.JoinHostPort(ip.DstIP.String(), strconv.Itoa(int(tcp.DstPort)))
}

func runCLI(t *testing.T, args ...string) error {
	t.Helper()

	cmd := &cli.Command{
		Name:     "dbbat",
		Commands: []*cli.Command{dumpCommand()},
	}

	return cmd.Run(context.Background(), append([]string{"dbbat"}, args...))
}

// TestDumpAnonymiseCLI_RewritesAddressesByDefault pins the CLI default: without
// any flag, the synthesized addressing is scrubbed.
func TestDumpAnonymiseCLI_RewritesAddressesByDefault(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	input := filepath.Join(dir, "capture"+dump.FileExt)
	writeCapture(t, input, "203.0.113.7:6543")

	require.Equal(t, "203.0.113.7:6543", serverEndpoint(t, input))
	require.NoError(t, runCLI(t, "dump", "anonymise", input))

	// The default output path is derived from the input.
	output := filepath.Join(dir, "capture.anonymised"+dump.FileExt)
	require.FileExists(t, output)

	assert.Equal(t, "10.77.0.2:5432", serverEndpoint(t, output))

	r, err := dump.OpenReader(output)
	require.NoError(t, err)

	defer func() { _ = r.Close() }()

	assert.Empty(t, r.Header().Connection)
}

// TestDumpAnonymiseCLI_KeepAddresses checks the opt-out flag is actually wired
// to the anonymiser.
func TestDumpAnonymiseCLI_KeepAddresses(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	input := filepath.Join(dir, "capture"+dump.FileExt)
	output := filepath.Join(dir, "out"+dump.FileExt)
	writeCapture(t, input, "203.0.113.7:6543")

	require.NoError(t, runCLI(t, "dump", "anonymise", "--keep-addresses", input, output))

	assert.Equal(t, "203.0.113.7:6543", serverEndpoint(t, output))

	r, err := dump.OpenReader(output)
	require.NoError(t, err)

	defer func() { _ = r.Close() }()

	assert.Empty(t, r.Header().Connection, "metadata is stripped either way")
}

// TestDumpAnonymiseCLI_MissingArgument checks the usage error.
func TestDumpAnonymiseCLI_MissingArgument(t *testing.T) {
	t.Parallel()

	assert.ErrorIs(t, runCLI(t, "dump", "anonymise"), errDumpAnonymiseUsage)
}
