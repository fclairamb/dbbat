package mssql

import (
	"bytes"
	"encoding/binary"
	"io"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// synthPacket builds one raw TDS packet from its parts, so the tests below work
// from bytes on the wire rather than from the codec's own output.
func synthPacket(msgType, status byte, packetID byte, payload []byte) []byte {
	buf := make([]byte, packetHeaderSize+len(payload))
	buf[0] = msgType
	buf[1] = status
	binary.BigEndian.PutUint16(buf[2:4], uint16(packetHeaderSize+len(payload)))
	binary.BigEndian.PutUint16(buf[4:6], 0)
	buf[6] = packetID
	buf[7] = 0
	copy(buf[packetHeaderSize:], payload)

	return buf
}

func TestDecodeHeader(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		raw     []byte
		want    packetHeader
		wantErr error
	}{
		{
			name: "prelogin eom",
			raw:  []byte{0x12, 0x01, 0x00, 0x0A, 0x00, 0x00, 0x01, 0x00, 0xAA, 0xBB},
			want: packetHeader{Type: 0x12, Status: 0x01, Length: 10, SPID: 0, PacketID: 1},
		},
		{
			name: "big endian spid and length",
			raw:  []byte{0x04, 0x00, 0x01, 0x02, 0x12, 0x34, 0x07, 0x00},
			want: packetHeader{Type: 0x04, Status: 0x00, Length: 0x0102, SPID: 0x1234, PacketID: 7},
		},
		{
			name:    "truncated header",
			raw:     []byte{0x12, 0x01, 0x00},
			wantErr: ErrShortHeader,
		},
		{
			name:    "length shorter than the header it covers",
			raw:     []byte{0x12, 0x01, 0x00, 0x03, 0x00, 0x00, 0x01, 0x00},
			wantErr: ErrBadPacketLength,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := decodeHeader(tc.raw)
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
			assert.Equal(t, tc.raw[:packetHeaderSize], func() []byte {
				e := got.encode()

				return e[:]
			}(), "encode must round-trip the bytes it was decoded from")
		})
	}
}

func TestReadMessage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		stream   []byte
		wantType byte
		wantBody []byte
		wantErr  error
	}{
		{
			name:     "single packet",
			stream:   synthPacket(packetTypePrelogin, statusEOM, 1, []byte("hello")),
			wantType: packetTypePrelogin,
			wantBody: []byte("hello"),
		},
		{
			name: "message split across three packets",
			stream: bytes.Join([][]byte{
				synthPacket(packetTypeLogin7, statusNormal, 1, []byte("aaa")),
				synthPacket(packetTypeLogin7, statusNormal, 2, []byte("bbb")),
				synthPacket(packetTypeLogin7, statusEOM, 3, []byte("ccc")),
			}, nil),
			wantType: packetTypeLogin7,
			wantBody: []byte("aaabbbccc"),
		},
		{
			name:     "header-only packet is a valid empty message",
			stream:   synthPacket(packetTypeSQLBatch, statusEOM, 1, nil),
			wantType: packetTypeSQLBatch,
			wantBody: nil,
		},
		{
			name:    "truncated trailer: payload shorter than the length promises",
			stream:  synthPacket(packetTypePrelogin, statusEOM, 1, []byte("hello"))[:packetHeaderSize+2],
			wantErr: ErrShortPayload,
		},
		{
			name:    "truncated header mid-stream",
			stream:  append(synthPacket(packetTypeLogin7, statusNormal, 1, []byte("aa")), 0x10, 0x01),
			wantErr: ErrShortHeader,
		},
		{
			name:    "eof before any packet",
			stream:  nil,
			wantErr: io.EOF,
		},
		{
			name: "packets of the message disagree on type",
			stream: bytes.Join([][]byte{
				synthPacket(packetTypeLogin7, statusNormal, 1, []byte("aa")),
				synthPacket(packetTypeSQLBatch, statusEOM, 2, []byte("bb")),
			}, nil),
			wantErr: ErrMixedMessageTypes,
		},
		{
			name:     "ignore bit discards the message",
			stream:   synthPacket(packetTypeSQLBatch, statusEOM|statusIgnore, 1, []byte("drop me")),
			wantType: packetTypeSQLBatch,
			wantErr:  ErrMessageIgnored,
		},
		{
			name:    "length field below the header size",
			stream:  []byte{0x12, 0x01, 0x00, 0x02, 0x00, 0x00, 0x01, 0x00},
			wantErr: ErrBadPacketLength,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			rw := newPacketRW(bytes.NewBuffer(tc.stream))

			gotType, gotBody, err := rw.ReadMessage()
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tc.wantType, gotType)
			assert.Equal(t, tc.wantBody, gotBody)
		})
	}
}

func TestWriteMessageSplitsAtPacketSize(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	rw := newPacketRW(&buf)
	rw.SetPacketSize(minPacketSize) // 512 total => 504 payload bytes per packet

	payload := bytes.Repeat([]byte{0x5A}, 504*2+7)
	require.NoError(t, rw.WriteMessage(packetTypeReply, payload))

	raw := buf.Bytes()

	// Three packets: 504 + 504 + 7.
	wantLens := []int{504, 504, 7}
	offset := 0

	for i, wantLen := range wantLens {
		hdr, err := decodeHeader(raw[offset:])
		require.NoError(t, err)
		assert.Equal(t, packetTypeReply, hdr.Type)
		assert.Equal(t, wantLen, int(hdr.Length)-packetHeaderSize)
		assert.Equal(t, byte(i+1), hdr.PacketID, "packet ids start at 1 and increment")

		if i == len(wantLens)-1 {
			assert.True(t, hdr.isEOM(), "only the last packet carries EOM")
		} else {
			assert.False(t, hdr.isEOM())
		}

		offset += int(hdr.Length)
	}

	assert.Equal(t, len(raw), offset, "the packets must exactly cover the stream")
}

func TestWriteMessageRoundTripsAtSeveralPacketSizes(t *testing.T) {
	t.Parallel()

	payload := make([]byte, 5000)
	for i := range payload {
		payload[i] = byte(i % 251)
	}

	for _, size := range []int{minPacketSize, 1024, defaultPacketSize, maxPacketSize} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer

			w := newPacketRW(&buf)
			w.SetPacketSize(size)
			require.NoError(t, w.WriteMessage(packetTypeLogin7, payload))

			r := newPacketRW(&buf)

			gotType, gotBody, err := r.ReadMessage()
			require.NoError(t, err)
			assert.Equal(t, packetTypeLogin7, gotType)
			assert.Equal(t, payload, gotBody)
		})
	}
}

func TestWriteMessageEmptyPayloadStillEmitsOnePacket(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	rw := newPacketRW(&buf)
	require.NoError(t, rw.WriteMessage(packetTypeReply, nil))
	assert.Equal(t, packetHeaderSize, buf.Len())

	hdr, err := decodeHeader(buf.Bytes())
	require.NoError(t, err)
	assert.True(t, hdr.isEOM())
}

func TestSetPacketSizeClamps(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   int
		want int
	}{
		{"below the floor", 128, minPacketSize},
		{"zero", 0, minPacketSize},
		{"in range", 8192, 8192},
		{"above the ceiling", 100000, maxPacketSize},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			rw := newPacketRW(&bytes.Buffer{})
			rw.SetPacketSize(tc.in)
			assert.Equal(t, tc.want, rw.PacketSize())
		})
	}
}

func TestWriteMessageRefusesWhileAStreamingPacketIsOpen(t *testing.T) {
	t.Parallel()

	rw := newPacketRW(&bytes.Buffer{})
	rw.beginPacket(packetTypePrelogin)

	require.ErrorIs(t, rw.WriteMessage(packetTypeReply, []byte("x")), ErrPacketPending)
}

func TestStreamingPacketFlushesOnCapacityAndFinish(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	rw := newPacketRW(&buf)
	rw.SetPacketSize(minPacketSize) // 504 payload bytes

	rw.beginPacket(packetTypePrelogin)
	assert.True(t, rw.packetOpen())

	n, err := rw.WriteStream(bytes.Repeat([]byte{1}, 504))
	require.NoError(t, err)
	assert.Equal(t, 504, n)
	assert.Zero(t, buf.Len(), "an exactly-full buffer is not flushed until more arrives")

	n, err = rw.WriteStream(bytes.Repeat([]byte{2}, 10))
	require.NoError(t, err)
	assert.Equal(t, 10, n)
	assert.Equal(t, packetHeaderSize+504, buf.Len(), "the full packet went out without EOM")

	require.NoError(t, rw.finishPacket())
	assert.False(t, rw.packetOpen())

	r := newPacketRW(&buf)

	gotType, body, err := r.ReadMessage()
	require.NoError(t, err)
	assert.Equal(t, packetTypePrelogin, gotType)
	assert.Equal(t, append(bytes.Repeat([]byte{1}, 504), bytes.Repeat([]byte{2}, 10)...), body)
}

func TestFinishPacketIsANoOpWhenNothingIsOpen(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	rw := newPacketRW(&buf)
	require.NoError(t, rw.finishPacket())
	assert.Zero(t, buf.Len())
}

func TestPacketTypeName(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "PRELOGIN", packetTypeName(packetTypePrelogin))
	assert.Equal(t, "LOGIN7", packetTypeName(packetTypeLogin7))
	assert.Equal(t, "PreTDS7Login", packetTypeName(packetTypeLegacyAuth))
	assert.Equal(t, "0x2f", packetTypeName(0x2F))
}
