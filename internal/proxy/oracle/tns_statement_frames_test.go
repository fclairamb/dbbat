package oracle

import (
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// legacyDataPacket builds a Data packet in the pre-v315 form: a two-byte length
// at [0:2].
func legacyDataPacket(payload []byte) []byte {
	raw := make([]byte, tnsHeaderSize+len(payload))
	binary.BigEndian.PutUint16(raw[0:2], uint16(len(raw)))
	raw[4] = byte(TNSPacketTypeData)
	copy(raw[tnsHeaderSize:], payload)

	return raw
}

// TestEncodeDataPacketLikeKeepsTheClientsHeaderForm is the prerequisite the spec
// names: encodeTNSPacket can only write the legacy two-byte length, and every
// session this rewriter touches is v315+, where the length is a four-byte field
// and the two-byte one reads zero.
func TestEncodeDataPacketLikeKeepsTheClientsHeaderForm(t *testing.T) {
	t.Parallel()

	payload := []byte{0x00, 0x00, 0x03, 0x5e, 0x01, 0x02, 0x03}

	v315 := encodeDataPacketLike(encodeV315DataPacket(payload), payload)
	require.NotNil(t, v315)
	assert.Zero(t, binary.BigEndian.Uint16(v315[0:2]),
		"a v315+ packet leaves the legacy length field at zero")
	assert.Equal(t, uint32(len(v315)), binary.BigEndian.Uint32(v315[0:4]))
	assert.Equal(t, byte(TNSPacketTypeData), v315[4])
	assert.Equal(t, payload, v315[tnsHeaderSize:])

	legacy := encodeDataPacketLike(legacyDataPacket(payload), payload)
	require.NotNil(t, legacy)
	assert.Equal(t, uint16(len(legacy)), binary.BigEndian.Uint16(legacy[0:2]))
	assert.Equal(t, payload, legacy[tnsHeaderSize:])

	// And the two really are different bytes, which is the point of modelling
	// the header on the client's own instead of re-encoding it.
	assert.NotEqual(t, v315[:4], legacy[:4])
}

// TestEncodeDataPacketLikeGrowsTheLength pins that a rewritten packet declares
// its own size and not the size of the packet it was modelled on — the failure
// that would desynchronize the upstream on the first tagged statement.
func TestEncodeDataPacketLikeGrowsTheLength(t *testing.T) {
	t.Parallel()

	model := encodeV315DataPacket([]byte{0x00, 0x00, 0x01})

	out := encodeDataPacketLike(model, []byte{0x00, 0x00, 0x01, 0x02, 0x03, 0x04})
	require.NotNil(t, out)
	assert.Equal(t, uint32(len(out)), binary.BigEndian.Uint32(out[0:4]))
	assert.Len(t, out, tnsHeaderSize+6)
}

// TestRefragmentStatementMessage covers the re-cutting itself: one packet when
// the message fits the negotiated unit, several when it does not, every one of
// them carrying the first packet's data-flags prefix and nothing else.
func TestRefragmentStatementMessage(t *testing.T) {
	t.Parallel()

	flags := []byte{0x00, 0x11}
	first := &TNSPacket{
		Type:    TNSPacketTypeData,
		Payload: append(append([]byte{}, flags...), 0x03, 0x5e),
	}
	first.Raw = encodeV315DataPacket(first.Payload)

	body := make([]byte, 5000)
	for i := range body {
		body[i] = byte(i)
	}

	single, ok := refragmentStatementMessage(first, body, 8192)
	require.True(t, ok)
	require.Len(t, single, 1)
	assert.Equal(t, flags, single[0][tnsHeaderSize:tnsHeaderSize+ttcDataFlagsSize])
	assert.Equal(t, body, single[0][tnsHeaderSize+ttcDataFlagsSize:])

	// sqlplus negotiates 2048, which is exactly why the unit is read off the
	// Accept rather than assumed to be 8192.
	multi, ok := refragmentStatementMessage(first, body, 2048)
	require.True(t, ok)
	require.Len(t, multi, 3)

	var rebuilt []byte

	for _, frame := range multi {
		assert.LessOrEqual(t, len(frame), 2048, "no fragment may exceed the negotiated unit")
		assert.Equal(t, uint32(len(frame)), binary.BigEndian.Uint32(frame[0:4]))
		assert.Equal(t, flags, frame[tnsHeaderSize:tnsHeaderSize+ttcDataFlagsSize])

		rebuilt = append(rebuilt, frame[tnsHeaderSize+ttcDataFlagsSize:]...)
	}

	assert.Equal(t, body, rebuilt, "the fragments must reassemble to the message")
}

// TestAcceptNegotiatedSDU reads the unit out of the Accept, and refuses rather
// than guessing when it cannot — a guessed SDU is ORA-12592 on the first tagged
// statement, or a statement pointlessly split in two.
func TestAcceptNegotiatedSDU(t *testing.T) {
	t.Parallel()

	accept := make([]byte, 45)
	binary.BigEndian.PutUint16(accept[0:2], 45)
	accept[4] = byte(TNSPacketTypeAccept)
	binary.BigEndian.PutUint16(accept[8:10], 319)
	binary.BigEndian.PutUint32(accept[acceptSDUOffset:acceptSDUOffset+4], 2048)

	sdu, ok := acceptNegotiatedSDU(accept)
	require.True(t, ok)
	assert.Equal(t, 2048, sdu)

	// A unit past what readTNSPacket will relay is clamped, not accepted as-is:
	// several go-ora recordings negotiate 65536.
	binary.BigEndian.PutUint32(accept[acceptSDUOffset:acceptSDUOffset+4], 65536)
	sdu, ok = acceptNegotiatedSDU(accept)
	require.True(t, ok)
	assert.Equal(t, maxTNSPacketSize, sdu)

	// A legacy Accept declares the unit in the two-byte field instead.
	legacy := make([]byte, 45)
	legacy[4] = byte(TNSPacketTypeAccept)
	binary.BigEndian.PutUint16(legacy[12:14], 8192)
	sdu, ok = acceptNegotiatedSDU(legacy)
	require.True(t, ok)
	assert.Equal(t, 8192, sdu)

	// Refusals.
	_, ok = acceptNegotiatedSDU(make([]byte, 45))
	assert.False(t, ok, "a packet that is not an Accept yields nothing")

	short := make([]byte, 20)
	short[4] = byte(TNSPacketTypeAccept)
	_, ok = acceptNegotiatedSDU(short)
	assert.False(t, ok, "a truncated Accept yields nothing")

	tiny := make([]byte, 45)
	tiny[4] = byte(TNSPacketTypeAccept)
	binary.BigEndian.PutUint16(tiny[12:14], 128)
	_, ok = acceptNegotiatedSDU(tiny)
	assert.False(t, ok, "a unit below Oracle's own minimum means the read is wrong")
}

// TestAcceptNegotiatedSDUAcrossTheCorpus is the same reading run against every
// real Accept dbbat has recorded, which is what makes acceptSDUOffset a measured
// offset rather than a remembered one.
func TestAcceptNegotiatedSDUAcrossTheCorpus(t *testing.T) {
	t.Parallel()

	seen := map[int]int{}

	for _, name := range surveyCorpus(t) {
		td := loadTestDump(t, name)

		for _, dpkt := range td.Packets {
			pkt, err := parseTNSFromDumpPacket(dpkt.Data)
			if err != nil || pkt == nil || pkt.Type != TNSPacketTypeAccept {
				continue
			}

			sdu, ok := acceptNegotiatedSDU(pkt.Raw)
			require.True(t, ok, "%s: every recorded Accept must yield a session data unit", name)
			seen[sdu]++
		}
	}

	t.Logf("negotiated session data units across the corpus: %v", seen)

	require.NotEmpty(t, seen)

	for sdu := range seen {
		require.GreaterOrEqual(t, sdu, minNegotiatedSDU)
		require.LessOrEqual(t, sdu, maxTNSPacketSize)
	}

	// The three the corpus actually carries: sqlplus at 2048, the thin clients
	// at 8192, and the go-ora captures whose 65536 is clamped to what this proxy
	// will relay.
	require.Contains(t, seen, 2048)
	require.Contains(t, seen, 8192)
	require.Contains(t, seen, maxTNSPacketSize)
}
