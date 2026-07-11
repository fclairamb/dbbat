package oracle

import (
	"encoding/hex"
	"strings"
	"testing"
)

// These fixtures are real AUTH bodies captured from the pre-auth relay. go-ora
// and SQLcl use one framing (a 2-byte trailer after the [03 76]/[03 73] sub-op);
// python-oracledb thin inserts one extra byte. The rewriter must align to both.
//
// Payloads include the 2-byte data-flags prefix; the rewrite functions take the
// body after that prefix.
const (
	gooraPhase1Payload  = "00000376000101090101010105010109636f6e6e6563746f72010d0d415554485f5445524d494e414c"
	pythonPhase1Payload = "0000037601000101090101010105010109636f6e6e6563746f72010d0d415554485f5445524d494e414c"

	gooraPhase2Payload  = "000003730001010902010101010e010109636f6e6e6563746f72010c0c415554485f534553534b4559"
	pythonPhase2Payload = "000003730200010109020101010107010109636f6e6e6563746f72010c0c415554485f534553534b4559"
)

func bodyOf(t *testing.T, payloadHex string) []byte {
	t.Helper()

	raw, err := hex.DecodeString(payloadHex)
	if err != nil {
		t.Fatalf("decode fixture: %v", err)
	}

	return raw[ttcDataFlagsSize:]
}

// ociPhase1Body is a real sqlplus (OCI thick) AUTH Phase 1 body — the "wide"
// wire encoding (4/8-byte little-endian counts, 0xfe pointer sentinels) with a
// bare 1-byte-length username token and no compressed-int user_id_len header.
const ociPhase1Body = "0376020103feffffffffffffff1b00000001000000feffffffffffffff05000000feffffffffffffff" +
	"feffffffffffffff09636f6e6e6563746f72270000000d415554485f5445524d494e414c"

func TestRewriteAuthPhase1Username_ClientFramings(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		payload string
	}{
		{"go-ora (4-byte header)", gooraPhase1Payload},
		{"python-oracledb (5-byte header)", pythonPhase1Payload},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			out, err := rewriteAuthPhase1Username(bodyOf(t, tc.payload), "DBBATTEST")
			if err != nil {
				t.Fatalf("rewrite: %v", err)
			}

			// The upstream username must appear, the client's must not, and the
			// AUTH_* KV tail must be preserved.
			if !strings.Contains(string(out), "DBBATTEST") {
				t.Errorf("rewritten body missing new username: %x", out)
			}

			if strings.Contains(strings.ToLower(string(out)), "connector") {
				t.Errorf("rewritten body still contains client username: %x", out)
			}

			if !strings.Contains(string(out), "AUTH_TERMINAL") {
				t.Errorf("rewritten body dropped AUTH_* KV tail: %x", out)
			}
		})
	}
}

func TestRewriteAuthPhase1Username_OCIWide(t *testing.T) {
	t.Parallel()

	body, err := hex.DecodeString(ociPhase1Body)
	if err != nil {
		t.Fatalf("decode fixture: %v", err)
	}

	out, err := rewriteAuthPhase1Username(body, "DBBATTEST")
	if err != nil {
		t.Fatalf("rewrite OCI wide Phase 1: %v", err)
	}

	if !strings.Contains(string(out), "DBBATTEST") {
		t.Errorf("missing new username: %x", out)
	}

	if strings.Contains(strings.ToLower(string(out)), "connector") {
		t.Errorf("client username not replaced: %x", out)
	}

	if !strings.Contains(string(out), "AUTH_TERMINAL") {
		t.Errorf("KV tail dropped: %x", out)
	}
}

func TestParseAuthPhase2Header_ClientFramings(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name          string
		payload       string
		wantHeaderLen int
	}{
		{"go-ora (3-byte header)", gooraPhase2Payload, 3},
		{"python-oracledb (4-byte header)", pythonPhase2Payload, 4},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			body := bodyOf(t, tc.payload)

			hdr, err := parseAuthPhase2Header(body)
			if err != nil {
				t.Fatalf("parse header: %v", err)
			}

			if hdr.headerLen != tc.wantHeaderLen {
				t.Errorf("headerLen = %d, want %d", hdr.headerLen, tc.wantHeaderLen)
			}

			if !hdr.hasUsername {
				t.Error("expected hasUsername=true")
			}

			// The KV region must start at the AUTH_SESSKEY pair.
			if !strings.HasPrefix(string(body[hdr.usernameEnd:]), "\x01\x0c\x0cAUTH_SESSKEY") {
				t.Errorf("KV region misaligned: %x", body[hdr.usernameEnd:])
			}
		})
	}
}
