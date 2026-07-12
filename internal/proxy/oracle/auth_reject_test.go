package oracle

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// decodeAuthRejectFrame parses a raw frame written by sendAuthFailed and returns
// the ORA error code and message it carries. It asserts the frame uses v315+
// TNS framing (4-byte length header, 2-byte length field 0x0000, Data type) —
// the regression the fix guards: a legacy 2-byte-framed reject is misread by
// modern clients and surfaces as ORA-12566 instead of the real code.
func decodeAuthRejectFrame(t *testing.T, frame []byte) (uint16, string) {
	t.Helper()

	require.GreaterOrEqual(t, len(frame), 8, "frame shorter than a TNS header")

	// v315+ framing: the legacy 2-byte length field must be zero and the real
	// length lives in the 4-byte big-endian field at bytes 0-3.
	assert.Equal(t, uint16(0), binary.BigEndian.Uint16(frame[0:2]),
		"2-byte length field must be 0x0000 for v315+ framing (else client reads ORA-12566)")
	assert.Equal(t, uint32(len(frame)), binary.BigEndian.Uint32(frame[0:4]),
		"4-byte length header must equal the total frame length")
	assert.Equal(t, byte(TNSPacketTypeData), frame[4], "packet type must be Data (0x06)")

	payload := frame[8:]
	require.GreaterOrEqual(t, len(payload), 3, "payload shorter than TTC header")
	// Data flags (2 bytes) + TTC function code.
	assert.Equal(t, byte(TTCFuncResponse), payload[2], "TTC func must be Response (0x08)")

	// ORA code: compressed TTC uint — 1-byte count then that many big-endian bytes.
	p := payload[3:]
	require.NotEmpty(t, p)
	n := int(p[0])
	require.LessOrEqual(t, 1+n, len(p), "truncated ORA code")

	var code uint64
	for _, b := range p[1 : 1+n] {
		code = code<<8 | uint64(b)
	}

	// Message: CLR-encoded — 1-byte length then the bytes (short form, <0xFC).
	rest := p[1+n:]
	var message string
	if len(rest) > 0 {
		msgLen := int(rest[0])
		if msgLen > 0 && 1+msgLen <= len(rest) {
			message = string(rest[1 : 1+msgLen])
		}
	}

	return uint16(code), message
}

func TestSendAuthFailed_EmitsV315FrameWithORACode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		code    uint16
		message string
	}{
		{"invalid credentials", ORA01017, "invalid username/password; logon denied"},
		{"no active grant", ORA01045, "no active grant for this database; request access via dbbat"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			client, proxyEnd := net.Pipe()
			defer func() { _ = client.Close() }()
			defer func() { _ = proxyEnd.Close() }()

			s := &session{clientConn: proxyEnd}

			go func() {
				s.sendAuthFailed(tt.code, tt.message)
			}()

			// Read the full frame off the wire and decode it directly, so the
			// framing itself is under test (not readTNSPacket's tolerance).
			header := make([]byte, 8)
			_, err := io.ReadFull(client, header)
			require.NoError(t, err)

			total := int(binary.BigEndian.Uint32(header[0:4]))
			require.GreaterOrEqual(t, total, 8)
			body := make([]byte, total-8)
			_, err = io.ReadFull(client, body)
			require.NoError(t, err)

			frame := append(header, body...)
			code, message := decodeAuthRejectFrame(t, frame)
			assert.Equal(t, tt.code, code, "reject frame must carry the chosen ORA code, not an abrupt EOF")
			assert.Equal(t, tt.message, message)
		})
	}
}

func TestAuthRejectFor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		err      error
		wantCode uint16
	}{
		{"no active grant is actionable", ErrNoActiveGrant, ORA01045},
		{"unknown user is generic", ErrUserNotFound, ORA01017},
		{"wrapped no-grant still routes to 1045", fmt.Errorf("%w: user=x database=y", ErrNoActiveGrant), ORA01045},
		{"other failures are generic", ErrDecryptedPasswordTooShort, ORA01017},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			code, message := authRejectFor(tt.err)
			assert.Equal(t, tt.wantCode, code)
			assert.NotEmpty(t, message)

			if tt.wantCode == ORA01045 {
				assert.Contains(t, message, "grant")
			} else {
				assert.Contains(t, message, "username/password")
			}
		})
	}
}
