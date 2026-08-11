package oracle

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestServerCapBitSet_RealSetProtocolReplies pins the ServerCompileTimeCaps
// indices against real Set Protocol replies captured from Oracle, so a future
// refactor cannot silently shift the array again.
//
// The array is framed [numCaps][caps…] and *opens with* the byte run
// 06 01 01 01 — that run is caps[0..3], not a prefix in front of the array.
// observeCustomHashFlag and observeBigClrChunksFlag used to anchor on it and
// index from the byte after, shifting every capability by four. That is
// invisible at index 0 (customHash, which was right by accident) and wrong at
// index 37: it read caps[41] instead, whose 0x20 bit is clear on every server
// measured, so dbbat concluded UseBigClrChunks = false on sessions where
// go-ora, JDBC thin and python-oracledb all conclude true.
func TestServerCapBitSet_RealSetProtocolReplies(t *testing.T) {
	t.Parallel()

	for name, want := range map[string]struct {
		numCaps    int
		customHash byte // caps[4]
		bigChunks  byte // caps[37]
		shifted    byte // caps[41] — what the old anchor read for big chunks
	}{
		// The two capability-array widths present in testdata: 0x36 (54) and
		// 0x2a (42).
		"go_ora_dml.pcapng":              {54, 0xef, 0x7f, 0x0d},
		"sqlplus_cursor_reexec.pcapng":   {54, 0xef, 0x7f, 0x0d},
		"jdbc_thin_cursor_reexec.pcapng": {54, 0xef, 0x7f, 0x0d},
		"python_thin.pcapng":             {42, 0x6f, 0x7f, 0x0d},
		"dbeaver.pcapng":                 {42, 0x6f, 0x7f, 0x0d},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			raw, caps := firstSetProtocolReply(t, name)

			require.Len(t, caps, want.numCaps)
			assert.Equal(t, []byte{0x06, 0x01, 0x01, 0x01}, caps[:4],
				"the 06 01 01 01 run is caps[0..3], not a prefix before the array")

			assert.Equal(t, want.customHash, caps[customHashCapIndex])
			assert.Equal(t, want.bigChunks, caps[bigClrChunksCapIndex])
			assert.Equal(t, want.shifted, caps[bigClrChunksCapIndex+4])

			assert.True(t, observeCustomHashFlag(raw), "caps[4]&0x20 is set on every server measured")
			assert.True(t, observeBigClrChunksFlag(raw), "caps[37]&0x20 is set on every server measured")

			// The byte the shifted anchor used to read: this is what made
			// observeBigClrChunksFlag answer false.
			assert.Zero(t, caps[bigClrChunksCapIndex+4]&0x20,
				"caps[41]&0x20 is clear — reading it is the bug this test guards against")
		})
	}
}

// TestServerCapBitSet_Rejects covers everything that is not a readable
// capability array. All of it must answer "not advertised" rather than panic or
// false-match, which is the conservative default: the legacy verifier-6949
// challenge for customHash, single-byte CLR chunk lengths for UseBigClrChunks.
func TestServerCapBitSet_Rejects(t *testing.T) {
	t.Parallel()

	// A Set Protocol reply whose capability array is too short to hold index 37.
	shortCaps := buildSetProtocolReply(t, append([]byte{0x06, 0x01, 0x01, 0x01, 0xef}, make([]byte, 5)...))

	for name, tc := range map[string]struct {
		raw            []byte
		wantCustomHash bool
		wantBigChunks  bool
	}{
		"nil":                  {nil, false, false},
		"truncated header":     {[]byte{0x00, 0x0a, 0x00}, false, false},
		"not a data packet":    {[]byte{0x00, 0x00, 0x00, 0x0a, byte(TNSPacketTypeAccept), 0x00, 0x00, 0x00, 0x00, 0x00}, false, false},
		"bare 06 01 01 01 run": {[]byte{0x00, 0x00, 0x00, 0x0e, byte(TNSPacketTypeData), 0x00, 0x00, 0x00, 0x00, 0x2a, 0x06, 0x01, 0x01, 0x01}, false, false},
		"caps array too short": {shortCaps, true, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.wantCustomHash, observeCustomHashFlag(tc.raw))
			assert.Equal(t, tc.wantBigChunks, observeBigClrChunksFlag(tc.raw))
		})
	}
}

// firstSetProtocolReply returns the first packet in a capture whose
// ServerCompileTimeCaps can be parsed, together with the array itself.
func firstSetProtocolReply(t *testing.T, dumpName string) ([]byte, []byte) {
	t.Helper()

	td := loadTestDump(t, dumpName)

	for i := range td.Packets {
		pkt, err := parseTNSFromDumpPacket(td.Packets[i].Data)
		if err != nil {
			continue
		}

		if caps, ok := serverCompileTimeCaps(pkt.Raw); ok {
			return pkt.Raw, caps
		}
	}

	t.Fatalf("%s contains no Set Protocol reply", dumpName)

	return nil, nil
}

// buildSetProtocolReply frames caps into a synthetic Set Protocol (OSETPRO)
// reply matching the preamble serverCompileTimeCaps walks:
//
//	[func 0x01][version:1][pad:1][server string NUL][charset:2][flags:1]
//	[charsetElems:2][len:2 BE][numArray][capsLen:1][caps...]
func buildSetProtocolReply(t *testing.T, caps []byte) []byte {
	t.Helper()

	body := make([]byte, 0, 64+len(caps))
	body = append(body, 0x00, 0x00, byte(TTCFuncSetProtocol), 0x06, 0x00)
	body = append(body, []byte("Oracle Database test server\x00")...)
	body = append(body, 0x69, 0x03) // server charset
	body = append(body, 0x03)       // server flags
	body = append(body, 0x00, 0x00) // charset element count
	body = append(body, 0x00, 0x00) // numArray length (BE)
	body = append(body, byte(len(caps)))
	body = append(body, caps...)

	raw := make([]byte, tnsHeaderSize+len(body))
	raw[0] = byte((tnsHeaderSize + len(body)) >> 24)
	raw[1] = byte((tnsHeaderSize + len(body)) >> 16)
	raw[2] = byte((tnsHeaderSize + len(body)) >> 8)
	raw[3] = byte(tnsHeaderSize + len(body))
	raw[4] = byte(TNSPacketTypeData)
	copy(raw[tnsHeaderSize:], body)

	return raw
}
