package oracle

import (
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTNSPacket_ParseHeader(t *testing.T) {
	t.Parallel()

	raw := []byte{0x00, 0x2A, 0x00, 0x00, 0x06, 0x00, 0x00, 0x00}
	pkt, err := parseTNSHeader(raw)
	require.NoError(t, err)
	assert.Equal(t, uint16(42), pkt.Length)
	assert.Equal(t, TNSPacketTypeData, pkt.Type)
}

func TestTNSPacket_ParseHeader_TooShort(t *testing.T) {
	t.Parallel()

	_, err := parseTNSHeader([]byte{0x00, 0x0A})
	assert.ErrorIs(t, err, ErrTNSHeaderTooShort)
}

func TestTNSPacket_ParseHeader_AllTypes(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		code byte
		want TNSPacketType
	}{
		{0x01, TNSPacketTypeConnect},
		{0x02, TNSPacketTypeAccept},
		{0x03, TNSPacketTypeRefuse},
		{0x04, TNSPacketTypeRedirect},
		{0x05, TNSPacketTypeMarker},
		{0x06, TNSPacketTypeData},
		{0x0B, TNSPacketTypeResend},
		{0x0C, TNSPacketTypeControl},
	} {
		raw := []byte{0x00, 0x08, 0x00, 0x00, tt.code, 0x00, 0x00, 0x00}
		pkt, err := parseTNSHeader(raw)
		require.NoError(t, err)
		assert.Equal(t, tt.want, pkt.Type)
	}
}

func TestTNSPacket_ParseHeader_UnknownType(t *testing.T) {
	t.Parallel()

	raw := []byte{0x00, 0x08, 0x00, 0x00, 0xFF, 0x00, 0x00, 0x00}
	pkt, err := parseTNSHeader(raw)
	require.NoError(t, err)
	assert.Equal(t, TNSPacketType(0xFF), pkt.Type)
}

func TestTNSPacket_Encode(t *testing.T) {
	t.Parallel()

	payload := []byte("hello")
	encoded := encodeTNSPacket(TNSPacketTypeData, payload)
	assert.Len(t, encoded, 8+len(payload))
	assert.Equal(t, byte(0x00), encoded[0])
	assert.Equal(t, byte(0x0D), encoded[1]) // length = 13
	assert.Equal(t, byte(0x06), encoded[4]) // type = Data
	assert.Equal(t, payload, encoded[8:])
}

func TestTNSPacket_RoundTrip(t *testing.T) {
	t.Parallel()

	original := TNSPacket{Type: TNSPacketTypeConnect, Payload: []byte("test-connect-data")}
	encoded := encodeTNSPacket(original.Type, original.Payload)
	parsed, err := parseTNSHeader(encoded[:8])
	require.NoError(t, err)
	assert.Equal(t, original.Type, parsed.Type)
	assert.Equal(t, uint16(len(encoded)), parsed.Length)
}

func TestTNSPacket_ZeroLengthPayload(t *testing.T) {
	t.Parallel()

	encoded := encodeTNSPacket(TNSPacketTypeResend, nil)
	assert.Len(t, encoded, 8)
}

func TestTNSPacket_MaxSDU(t *testing.T) {
	t.Parallel()

	payload := make([]byte, 32767-8)
	encoded := encodeTNSPacket(TNSPacketTypeData, payload)
	parsed, err := parseTNSHeader(encoded[:8])
	require.NoError(t, err)
	assert.Equal(t, uint16(32767), parsed.Length)
}

func TestTNSPacketType_String(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "Connect", TNSPacketTypeConnect.String())
	assert.Equal(t, "Data", TNSPacketTypeData.String())
	assert.Equal(t, "Refuse", TNSPacketTypeRefuse.String())
	assert.Equal(t, "Unknown(255)", TNSPacketType(0xFF).String())
}

// --- I/O tests with net.Pipe ---

func TestTNSPacket_ReadFromConn(t *testing.T) {
	t.Parallel()

	client, server := net.Pipe()
	defer func() { _ = client.Close() }()
	defer func() { _ = server.Close() }()

	go func() {
		raw := encodeTNSPacket(TNSPacketTypeConnect, []byte("connect-data"))
		_, _ = client.Write(raw)
	}()

	pkt, err := readTNSPacket(server)
	require.NoError(t, err)
	assert.Equal(t, TNSPacketTypeConnect, pkt.Type)
	assert.Equal(t, []byte("connect-data"), pkt.Payload)
}

func TestTNSPacket_ReadFromConn_PartialWrites(t *testing.T) {
	t.Parallel()

	client, server := net.Pipe()
	defer func() { _ = client.Close() }()
	defer func() { _ = server.Close() }()

	go func() {
		raw := encodeTNSPacket(TNSPacketTypeData, []byte("payload"))
		for i := range raw {
			_, _ = client.Write(raw[i : i+1])
			time.Sleep(time.Millisecond)
		}
	}()

	pkt, err := readTNSPacket(server)
	require.NoError(t, err)
	assert.Equal(t, []byte("payload"), pkt.Payload)
}

func TestTNSPacket_ReadFromConn_EOF(t *testing.T) {
	t.Parallel()

	client, server := net.Pipe()
	go func() {
		_, _ = client.Write([]byte{0x00, 0x20}) // partial header
		_ = client.Close()
	}()

	_, err := readTNSPacket(server)
	assert.Error(t, err)
}

func TestTNSPacket_MultiplePackets(t *testing.T) {
	t.Parallel()

	client, server := net.Pipe()
	defer func() { _ = client.Close() }()
	defer func() { _ = server.Close() }()

	types := []TNSPacketType{TNSPacketTypeConnect, TNSPacketTypeData, TNSPacketTypeMarker}
	go func() {
		for _, typ := range types {
			raw := encodeTNSPacket(typ, []byte(fmt.Sprintf("pkt-%d", typ)))
			_, _ = client.Write(raw)
		}
	}()

	for _, expected := range types {
		pkt, err := readTNSPacket(server)
		require.NoError(t, err)
		assert.Equal(t, expected, pkt.Type)
	}
}

func TestTNSPacket_WriteThenRead(t *testing.T) {
	t.Parallel()

	client, server := net.Pipe()
	defer func() { _ = client.Close() }()
	defer func() { _ = server.Close() }()

	original := &TNSPacket{
		Type:    TNSPacketTypeData,
		Payload: []byte("round-trip-test"),
	}

	go func() {
		_ = writeTNSPacket(client, original)
	}()

	pkt, err := readTNSPacket(server)
	require.NoError(t, err)
	assert.Equal(t, original.Type, pkt.Type)
	assert.Equal(t, original.Payload, pkt.Payload)
}
