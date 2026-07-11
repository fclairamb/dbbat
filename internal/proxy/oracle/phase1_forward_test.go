package oracle

import (
	"bytes"
	"testing"
)

// TestRewriteAuthPhase1Username_BareEncoding covers SQLcl's wire shape: no CLR
// length prefix on the username (the leading user_id_len compressed integer
// is sole authority on length).
func TestRewriteAuthPhase1Username_BareEncoding(t *testing.T) {
	t.Parallel()

	body := buildPhase1Body(t, "CONNECTOR", false)

	out, err := rewriteAuthPhase1Username(body, "LABEOMNGR_DEV")
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}

	expected := buildPhase1Body(t, "LABEOMNGR_DEV", false)
	if !bytes.Equal(out, expected) {
		t.Fatalf("bare-encoding rewrite mismatch:\n got %x\nwant %x", out, expected)
	}
}

// TestRewriteAuthPhase1Username_CLREncoding covers go-ora / python-oracledb's
// wire shape: 1-byte CLR length prefix followed by the username bytes.
func TestRewriteAuthPhase1Username_CLREncoding(t *testing.T) {
	t.Parallel()

	body := buildPhase1Body(t, "connector", true)

	out, err := rewriteAuthPhase1Username(body, "LABEOMNGR_DEV")
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}

	expected := buildPhase1Body(t, "LABEOMNGR_DEV", true)
	if !bytes.Equal(out, expected) {
		t.Fatalf("CLR-encoding rewrite mismatch:\n got %x\nwant %x", out, expected)
	}
}

// TestRewriteAuthPhase1Username_Errors covers the obvious malformed-input
// cases — too-short body, wrong piggyback marker, and an absurd user_id_len.
func TestRewriteAuthPhase1Username_Errors(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		body []byte
	}{
		{"empty", nil},
		{"truncated header", []byte{0x03, 0x76}},
		{"wrong func code", []byte{0x05, 0x76, 0x00, 0x01, 0x01, 0x09}},
		{"absurd userIDLen", append([]byte{byte(TTCFuncPiggyback), PiggybackSubAuth1, 0x00, 0x01}, 0x02, 0xff, 0xff)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if _, err := rewriteAuthPhase1Username(tc.body, "X"); err == nil {
				t.Fatalf("expected error for %s", tc.name)
			}
		})
	}
}

// buildPhase1Body returns a TTC AUTH Phase 1 body matching the wire layout
// `[03 76 00 01] [userLen] [mode] [magic] [(CLR-prefix?) username] [trailing
// KV-pair bytes]`. The trailing bytes are a fixed sentinel so tests can
// confirm they are passed through unchanged.
func buildPhase1Body(t *testing.T, username string, withCLRPrefix bool) []byte {
	t.Helper()

	buf := []byte{byte(TTCFuncPiggyback), PiggybackSubAuth1, 0x00, 0x01}
	buf = append(buf, ttcCompressedUint(uint64(len(username)))...)
	buf = append(buf, ttcCompressedUint(1)...)
	buf = append(buf, 0x01, 0x01, 0x05, 0x01, 0x01)

	if withCLRPrefix {
		buf = append(buf, byte(len(username)))
	}

	buf = append(buf, []byte(username)...)
	// Real Phase 1 bodies carry AUTH_* KV pairs after the username; the rewriter
	// uses that to confirm it aligned the username boundary correctly.
	buf = append(buf, ttcKeyVal("AUTH_TERMINAL", "unknown", 0)...)

	return buf
}
