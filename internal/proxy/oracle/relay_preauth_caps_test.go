package oracle

import (
	"bytes"
	"encoding/hex"
	"testing"
)

// buildSetProtocolPayload assembles a Set Protocol (protocol negotiation)
// response payload (data-flags prefix included, TNS header excluded) with the
// given ServerCompileTimeCaps array, mirroring go-ora's newTCPNego layout.
func buildSetProtocolPayload(caps []byte) []byte {
	p := []byte{0x00, 0x00} // data flags
	p = append(p, 0x01)     // MessageCode 1 = Accept protocol
	p = append(p, 0x06)     // ProtocolServerVersion
	p = append(p, 0x00)     // reserved byte
	p = append(p, []byte("Oracle\x00")...) // ProtocolServerString (null-terminated)
	p = append(p, 0x00, 0x00)              // ServerCharset (uint16 LE)
	p = append(p, 0x00)                    // ServerFlags
	p = append(p, 0x00, 0x00)              // CharsetElem (uint16 LE) = 0 -> no table
	p = append(p, 0x00, 0x00)              // len1 (uint16 BE) = 0 -> no reserved array
	p = append(p, byte(len(caps)))         // len2 = caps length
	p = append(p, caps...)

	return p
}

func TestServerCompileTimeCaps_ParsesCustomHash(t *testing.T) {
	t.Parallel()

	// caps[4]=0xef has bit 0x20 (customHash) set. Length is 54, not the 42 the
	// old marker scan hardcoded — the whole point of the structural parse.
	caps := make([]byte, 54)
	copy(caps, []byte{0x06, 0x01, 0x01, 0x01, 0xef})
	payload := buildSetProtocolPayload(caps)

	got, ok := serverCompileTimeCaps(payload)
	if !ok {
		t.Fatal("serverCompileTimeCaps returned ok=false")
	}

	if len(got) != len(caps) {
		t.Fatalf("caps length = %d, want %d", len(got), len(caps))
	}

	if got[logonCompatibilityCapIndex]&capCustomHash == 0 {
		t.Fatalf("expected customHash bit set in caps[%d]=0x%02x",
			logonCompatibilityCapIndex, got[logonCompatibilityCapIndex])
	}
}

func TestServerCompileTimeCaps_NoCustomHash(t *testing.T) {
	t.Parallel()

	caps := make([]byte, 42) // legacy 42-byte caps, customHash bit clear
	copy(caps, []byte{0x06, 0x01, 0x01, 0x01, 0x0f})
	payload := buildSetProtocolPayload(caps)

	got, ok := serverCompileTimeCaps(payload)
	if !ok {
		t.Fatal("serverCompileTimeCaps returned ok=false")
	}

	if got[logonCompatibilityCapIndex]&capCustomHash != 0 {
		t.Fatalf("customHash bit unexpectedly set: caps[4]=0x%02x", got[logonCompatibilityCapIndex])
	}
}

func TestServerCompileTimeCaps_RejectsNonProtocolResponse(t *testing.T) {
	t.Parallel()

	// A Data packet body whose first TTC byte after the data flags is not the
	// Accept-protocol message code (0x01) must not be mistaken for caps.
	payload := []byte{0x00, 0x00, 0x06, 0xde, 0xad, 0xbe, 0xef}
	if _, ok := serverCompileTimeCaps(payload); ok {
		t.Fatal("expected ok=false for a non-protocol-negotiation payload")
	}
}

func TestStripUnsupportedAcceptFlags(t *testing.T) {
	t.Parallel()

	// Real Oracle 23ai Accept packet (version 319) whose flags2 word at body
	// offset 33 is 0x1a000000 = FAST_AUTH(0x10000000) | 0x08000000 |
	// HAS_END_OF_RESPONSE(0x02000000).
	accept, err := hex.DecodeString(
		"003d000002000000013f00010000000001000000003dc50000000000000000000000200000002000001a000000217863740c16543f60f06900c1888700")
	if err != nil {
		t.Fatalf("decode fixture: %v", err)
	}

	if !stripUnsupportedAcceptFlags(accept) {
		t.Fatal("expected stripUnsupportedAcceptFlags to report a change")
	}

	body := accept[8:]
	off := acceptFlags2BodyOffset
	got := uint32(body[off])<<24 | uint32(body[off+1])<<16 | uint32(body[off+2])<<8 | uint32(body[off+3])

	if got&acceptFlags2FastAuth != 0 {
		t.Errorf("FAST_AUTH not cleared: flags2=0x%08x", got)
	}

	if got&acceptFlags2EndOfResponse != 0 {
		t.Errorf("HAS_END_OF_RESPONSE not cleared: flags2=0x%08x", got)
	}

	// The unrelated 0x08000000 bit must be preserved.
	if got != 0x08000000 {
		t.Errorf("unexpected flags2 after strip: got 0x%08x want 0x08000000", got)
	}
}

func TestStripUnsupportedAcceptFlags_NoFlags2(t *testing.T) {
	t.Parallel()

	// A short Accept (go-ora style, no flags2 word) must be left untouched.
	accept := append([]byte{0x00, 0x29, 0x00, 0x00, 0x02, 0x00, 0x00, 0x00}, // TNS header, type Accept
		0x01, 0x3f, // version 319
	)
	accept = append(accept, bytes.Repeat([]byte{0x00}, 20)...)

	before := append([]byte(nil), accept...)
	if stripUnsupportedAcceptFlags(accept) {
		t.Fatal("expected no change for a short Accept without flags2")
	}

	if !bytes.Equal(accept, before) {
		t.Fatal("short Accept was mutated")
	}
}
