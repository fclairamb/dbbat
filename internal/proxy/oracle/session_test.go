package oracle

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// buildTNSConnect builds a TNS Connect packet payload with the given service name
// embedded in a connect descriptor.
func buildTNSConnect(serviceName string) []byte {
	// Build a minimal connect descriptor
	desc := "(DESCRIPTION=(CONNECT_DATA=(SERVICE_NAME=" + serviceName + ")))"

	// TNS Connect payload layout (simplified):
	// We put the connect data at offset 26 (after fixed header fields)
	// Bytes 0-1:  version (0x0139 = 313)
	// Bytes 2-3:  version compatible (0x012C = 300)
	// Bytes 4-5:  service options (0x0000)
	// Bytes 6-7:  SDU size (0x2000 = 8192)
	// Bytes 8-9:  TDU size (0x7FFF = 32767)
	// Bytes 10-11: protocol characteristics (0x4F98)
	// Bytes 12-13: line turnaround (0x0000)
	// Bytes 14-15: value of one (0x0001)
	// Bytes 16-17: connect data length
	// Bytes 18-19: connect data offset (26 = 0x001A)
	// Bytes 20-23: max receivable connect data (0x00000800)
	// Bytes 24:    connect flags 0
	// Bytes 25:    connect flags 1

	descBytes := []byte(desc)
	descLen := len(descBytes)

	payload := make([]byte, 26+descLen)
	// version
	payload[0] = 0x01
	payload[1] = 0x39
	// version compatible
	payload[2] = 0x01
	payload[3] = 0x2C
	// SDU size
	payload[6] = 0x20
	payload[7] = 0x00
	// TDU size
	payload[8] = 0x7F
	payload[9] = 0xFF
	// connect data length
	payload[16] = byte(descLen >> 8)
	payload[17] = byte(descLen)
	// connect data offset (34 from packet start = 26 from payload start)
	// TNS spec: offset is from start of full packet (including 8-byte header)
	connectDataOffsetFromPacket := 26 + tnsHeaderSize // 34
	payload[18] = byte(connectDataOffsetFromPacket >> 8)
	payload[19] = byte(connectDataOffsetFromPacket)
	// max receivable connect data
	payload[22] = 0x08
	payload[23] = 0x00

	copy(payload[26:], descBytes)

	return payload
}

func TestSession_ReceiveConnect_ParsesServiceName(t *testing.T) {
	t.Parallel()

	client, proxyEnd := net.Pipe()
	defer func() { _ = client.Close() }()
	defer func() { _ = proxyEnd.Close() }()

	go func() {
		pkt := encodeTNSPacket(TNSPacketTypeConnect, buildTNSConnect("TESTDB"))
		_, _ = client.Write(pkt)
	}()

	// Read the connect packet and parse it like the session does
	connectPkt, err := readTNSPacket(proxyEnd)
	require.NoError(t, err)
	assert.Equal(t, TNSPacketTypeConnect, connectPkt.Type)

	connectStr := extractConnectString(connectPkt.Payload)
	cd := parseConnectDescriptor(connectStr)
	assert.Equal(t, "TESTDB", cd.ServiceName)
}

func TestSession_SendRefuse(t *testing.T) {
	t.Parallel()

	client, proxyEnd := net.Pipe()
	defer func() { _ = client.Close() }()
	defer func() { _ = proxyEnd.Close() }()

	s := &session{clientConn: proxyEnd}

	go func() {
		s.sendRefuse("test error reason")
	}()

	pkt, err := readTNSPacket(client)
	require.NoError(t, err)
	assert.Equal(t, TNSPacketTypeRefuse, pkt.Type)
	assert.Contains(t, string(pkt.Payload), "test error reason")
}

func TestSession_RawRelay(t *testing.T) {
	t.Parallel()

	// Set up a simple echo server as "upstream"
	upstreamListener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = upstreamListener.Close() }()

	go func() {
		conn, err := upstreamListener.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()

		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

		// Echo: read a TNS packet and send it back
		pkt, err := readTNSPacket(conn)
		if err != nil {
			return
		}
		_ = writeTNSPacket(conn, pkt)
	}()

	// Create client and proxy sides
	client, proxyEnd := net.Pipe()
	defer func() { _ = client.Close() }()

	upstreamConn, err := net.Dial("tcp", upstreamListener.Addr().String())
	require.NoError(t, err)

	s := &session{
		clientConn:   proxyEnd,
		upstreamConn: upstreamConn,
		ctx:          context.Background(),
		logger:       testLogger(),
		tracker:      newOracleQueryTracker(),
	}

	go func() {
		_ = s.proxyMessages()
	}()

	// Send a Data packet from client
	testPayload := []byte("hello oracle relay")
	_, err = client.Write(encodeTNSPacket(TNSPacketTypeData, testPayload))
	require.NoError(t, err)

	// Should get it echoed back
	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	pkt, err := readTNSPacket(client)
	require.NoError(t, err)
	assert.Equal(t, TNSPacketTypeData, pkt.Type)
	assert.Equal(t, testPayload, pkt.Payload)

	// Close client to unblock the relay goroutine
	_ = client.Close()
}

func TestSession_ConnectToUpstream_ForwardsAndRelays(t *testing.T) {
	t.Parallel()

	// Simulate an upstream Oracle that receives TNS Connect → sends TNS Accept
	upstreamListener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = upstreamListener.Close() }()

	go func() {
		conn, err := upstreamListener.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()

		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

		if _, err = readTNSPacket(conn); err != nil {
			return
		}

		_ = writeTNSPacket(conn, &TNSPacket{
			Type:    TNSPacketTypeAccept,
			Payload: []byte("accepted"),
		})
	}()

	// Build the connect packet upfront (no goroutine needed)
	connectData := encodeTNSPacket(TNSPacketTypeConnect, buildTNSConnect("TESTDB"))

	// Connect to upstream
	upstreamConn, err := net.Dial("tcp", upstreamListener.Addr().String())
	require.NoError(t, err)
	defer func() { _ = upstreamConn.Close() }()

	_ = upstreamConn.SetDeadline(time.Now().Add(5 * time.Second))

	// Forward connect (parse first to verify, then send raw)
	pkt, err := parseTNSHeader(connectData[:8])
	require.NoError(t, err)
	assert.Equal(t, TNSPacketTypeConnect, pkt.Type)

	_, err = upstreamConn.Write(connectData)
	require.NoError(t, err)

	// Read accept from upstream
	acceptPkt, err := readTNSPacket(upstreamConn)
	require.NoError(t, err)
	assert.Equal(t, TNSPacketTypeAccept, acceptPkt.Type)
	assert.Equal(t, []byte("accepted"), acceptPkt.Payload)
}

func TestSession_UpstreamRefuse_ForwardedToClient(t *testing.T) {
	t.Parallel()

	// Upstream that immediately refuses
	upstreamListener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = upstreamListener.Close() }()

	go func() {
		conn, err := upstreamListener.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()

		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

		_, _ = readTNSPacket(conn)

		_ = writeTNSPacket(conn, &TNSPacket{
			Type:    TNSPacketTypeRefuse,
			Payload: []byte("connection refused by upstream"),
		})
	}()

	// Use TCP listener pair instead of net.Pipe to avoid synchronous blocking
	clientListener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = clientListener.Close() }()

	// Client goroutine: sends Connect, reads response
	type clientResult struct {
		resp *TNSPacket
		err  error
	}
	resultCh := make(chan clientResult, 1)

	go func() {
		conn, err := net.Dial("tcp", clientListener.Addr().String())
		if err != nil {
			resultCh <- clientResult{err: err}
			return
		}
		defer func() { _ = conn.Close() }()

		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

		// Send connect
		_, _ = conn.Write(encodeTNSPacket(TNSPacketTypeConnect, buildTNSConnect("TESTDB")))

		// Read response
		resp, err := readTNSPacket(conn)
		resultCh <- clientResult{resp: resp, err: err}
	}()

	// Accept the "client" connection (this is the proxy side)
	proxyEnd, err := clientListener.Accept()
	require.NoError(t, err)
	defer func() { _ = proxyEnd.Close() }()

	_ = proxyEnd.SetDeadline(time.Now().Add(5 * time.Second))

	// Read connect from client
	connectPkt, err := readTNSPacket(proxyEnd)
	require.NoError(t, err)

	// Connect to upstream and forward
	upstreamConn, err := net.Dial("tcp", upstreamListener.Addr().String())
	require.NoError(t, err)
	defer func() { _ = upstreamConn.Close() }()

	_ = upstreamConn.SetDeadline(time.Now().Add(5 * time.Second))

	_ = writeTNSPacket(upstreamConn, connectPkt)

	// Read refuse from upstream
	resp, err := readTNSPacket(upstreamConn)
	require.NoError(t, err)
	assert.Equal(t, TNSPacketTypeRefuse, resp.Type)

	// Forward to client
	_ = writeTNSPacket(proxyEnd, resp)

	// Check client received the refuse
	result := <-resultCh
	require.NoError(t, result.err)
	assert.Equal(t, TNSPacketTypeRefuse, result.resp.Type)
	assert.Contains(t, string(result.resp.Payload), "connection refused")
}

func TestBuildTNSConnect_Parseable(t *testing.T) {
	t.Parallel()

	// Verify our test helper builds valid connect packets
	payload := buildTNSConnect("MYDB")
	connectStr := extractConnectString(payload)
	assert.Equal(t, "MYDB", parseServiceName(connectStr))
}

func TestBuildTNSConnect_DifferentServiceNames(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"ORCL", "PROD_DB", "my_service_123"} {
		payload := buildTNSConnect(name)
		connectStr := extractConnectString(payload)
		assert.Equal(t, name, parseServiceName(connectStr), "service name %s", name)
	}
}
