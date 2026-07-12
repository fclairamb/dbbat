package oracle

import (
	"encoding/binary"
	"testing"
)

// TestPayloadUsesWideKVEncoding verifies the OCI-vs-thin discriminator: OCI
// (sqlplus) encodes a 13-byte key length as 4-byte little-endian (0d 00 00 00)
// before the 1-byte CLR length, while thin clients use the compressed form
// (01 0d). Fixtures are the byte run around AUTH_TERMINAL captured from each.
func TestPayloadUsesWideKVEncoding(t *testing.T) {
	t.Parallel()

	wide := append([]byte{0x54, 0x0d, 0x00, 0x00, 0x00, 0x0d}, []byte("AUTH_TERMINAL")...)
	if !payloadUsesWideKVEncoding(wide) {
		t.Fatalf("expected wide encoding to be detected")
	}

	compressed := append([]byte{0x45, 0x53, 0x54, 0x01, 0x0d, 0x0d}, []byte("AUTH_TERMINAL")...)
	if payloadUsesWideKVEncoding(compressed) {
		t.Fatalf("compressed encoding wrongly detected as wide")
	}
}

// TestBuildAuthChallengeWide checks the OCI challenge framing: data flags 0x2000,
// func 0x08, a 2-byte little-endian pair count, and 4-byte little-endian key
// lengths — the shape sqlplus parses (a compressed challenge makes it abort).
func TestBuildAuthChallengeWide(t *testing.T) {
	t.Parallel()

	c := buildAuthChallenge("AABB", "CCDD", "EEFF", 4096, 3, VerifierType18453, true)

	if c[0] != 0x20 || c[1] != 0x00 {
		t.Fatalf("wide data flags = %02x %02x, want 20 00", c[0], c[1])
	}

	if c[2] != byte(TTCFuncResponse) {
		t.Fatalf("func = %02x, want %02x", c[2], byte(TTCFuncResponse))
	}

	pairs := binary.LittleEndian.Uint16(c[3:5])
	if pairs != 6 {
		t.Fatalf("pair count = %d, want 6", pairs)
	}

	// First KV pair: keyLen is a 4-byte LE integer = len("AUTH_SESSKEY") = 12.
	keyLen := binary.LittleEndian.Uint32(c[5:9])
	if keyLen != uint32(len(authKeySessKey)) {
		t.Fatalf("first keyLen = %d, want %d", keyLen, len(authKeySessKey))
	}

	// The compressed form must NOT use 4-byte lengths (regression guard).
	cc := buildAuthChallenge("AABB", "CCDD", "EEFF", 4096, 3, VerifierType18453, false)
	if cc[0] != 0x00 || cc[1] != 0x00 {
		t.Fatalf("compressed data flags = %02x %02x, want 00 00", cc[0], cc[1])
	}
}

// TestParseAuthPhase2Wide round-trips a wide-encoded Phase 2: ttcKeyValWide
// writes AUTH_SESSKEY/AUTH_PASSWORD with 4-byte lengths, and parseAuthPhase2
// must recover their values via the wide finder.
func TestParseAuthPhase2Wide(t *testing.T) {
	t.Parallel()

	body := make([]byte, 0, 64)
	body = append(body, 0x00, 0x00, byte(TTCFuncPiggyback), PiggybackSubAuth2)
	// A wide preamble byte run so payloadUsesWideKVEncoding trips before the keys.
	body = append(body, 0x09, 0x00, 0x00, 0x00, 0x09)
	body = append(body, []byte("ORAUSERXX")...)
	body = append(body, ttcKeyValWide(authKeySessKey, "DEADBEEF", 0)...)
	body = append(body, ttcKeyValWide(authKeyPassword, "CAFEBABE", 0)...)

	sess, pw, err := parseAuthPhase2(body)
	if err != nil {
		t.Fatalf("parseAuthPhase2 (wide) failed: %v", err)
	}

	if sess != "DEADBEEF" {
		t.Fatalf("sesskey = %q, want DEADBEEF", sess)
	}

	if pw != "CAFEBABE" {
		t.Fatalf("password = %q, want CAFEBABE", pw)
	}
}
