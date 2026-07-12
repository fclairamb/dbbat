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

// thinPhase1Payload builds a realistic thin-client (python-oracledb thin /
// go-ora) AUTH Phase 1 TNS Data payload — 2-byte data flags included — with
// real AUTH_* KV pairs following the username, so the anchored locator
// (locateAnchoredUsername → locateThinUsername) is exercised, not the
// fixed-offset fallback. For username "florent.clairambault" with a CLR
// prefix, the leading bytes reproduce the payload captured from
// python-oracledb thin on 2026-07-12:
//
//	00000376010101140101010105010114 || "florent.clairambault" || KV pairs
func thinPhase1Payload(username string, withCLRPrefix bool) []byte {
	payload := []byte{0x00, 0x00} // TNS data flags
	payload = append(payload, byte(TTCFuncPiggyback), PiggybackSubAuth1, 0x01, 0x01)
	payload = append(payload, ttcCompressedUint(uint64(len(username)))...) // user_id_len
	payload = append(payload, ttcCompressedUint(1)...)                     // logon mode
	payload = append(payload, 0x01, 0x01, 0x05, 0x01, 0x01)                // 5-byte magic

	if withCLRPrefix {
		payload = append(payload, byte(len(username)))
	}

	payload = append(payload, []byte(username)...)
	payload = append(payload, ttcCompressedUint(4)...) // KV pair count
	payload = append(payload, ttcKeyVal("AUTH_TERMINAL", "unknown", 0)...)
	payload = append(payload, ttcKeyVal("AUTH_PROGRAM_NM", "python", 0)...)

	return payload
}

// TestParseAuthPhase1_ThinDottedUsername is the regression test for the
// 2026-07-12 incident: python-oracledb thin logins with a dotted username were
// truncated at the '.' by the identifier-only walk ("user not found:
// CLAIRAMBAULT"), because only the wide/OCI locator had been fixed (PR #235).
// Dots, dashes and '@' must all survive, in both thin username encodings
// (CLR-prefixed = go-ora/python, bare = SQLcl/JDBC thin).
func TestParseAuthPhase1_ThinDottedUsername(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		username string
		clr      bool
		want     string
	}{
		{"dotted CLR-prefixed (python-oracledb thin capture)", "florent.clairambault", true, "FLORENT.CLAIRAMBAULT"},
		{"dotted bare (SQLcl style)", "FLORENT.CLAIRAMBAULT", false, "FLORENT.CLAIRAMBAULT"},
		{"dashed CLR-prefixed", "jean-pierre", true, "JEAN-PIERRE"},
		{"at-sign CLR-prefixed", "user@corp", true, "USER@CORP"},
		{"plain identifier still works", "connector", true, "CONNECTOR"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := parseAuthPhase1(thinPhase1Payload(tc.username, tc.clr))
			if err != nil {
				t.Fatalf("parseAuthPhase1: %v", err)
			}

			if got != tc.want {
				t.Fatalf("parseAuthPhase1 = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestParseAuthPhase1_ObservedThinCapture pins the exact leading bytes
// observed on the wire from python-oracledb thin during the 2026-07-12
// incident (hex 00000376010101140101010105010114 + 20-byte dotted username).
func TestParseAuthPhase1_ObservedThinCapture(t *testing.T) {
	t.Parallel()

	payload := thinPhase1Payload("florent.clairambault", true)

	wantPrefix := []byte{
		0x00, 0x00, 0x03, 0x76, 0x01, 0x01, 0x01, 0x14,
		0x01, 0x01, 0x01, 0x01, 0x05, 0x01, 0x01, 0x14,
	}
	if !bytes.HasPrefix(payload, wantPrefix) {
		t.Fatalf("fixture drifted from observed capture:\n got %x\nwant prefix %x", payload[:16], wantPrefix)
	}

	got, err := parseAuthPhase1(payload)
	if err != nil {
		t.Fatalf("parseAuthPhase1: %v", err)
	}

	if got != "FLORENT.CLAIRAMBAULT" {
		t.Fatalf("parseAuthPhase1 = %q, want FLORENT.CLAIRAMBAULT", got)
	}
}

// TestRewriteAuthPhase1Username_ThinDottedUsername keeps the upstream Phase 1
// rewriter in lock-step with client-auth parsing (they share
// locateAnchoredUsername): the dotted username must be replaced whole, with
// the user_id_len and CLR prefix updated — not spliced over its post-dot tail.
func TestRewriteAuthPhase1Username_ThinDottedUsername(t *testing.T) {
	t.Parallel()

	for _, clr := range []bool{true, false} {
		body := thinPhase1Payload("florent.clairambault", clr)[ttcDataFlagsSize:]

		out, err := rewriteAuthPhase1Username(body, "ABYLA_I3F")
		if err != nil {
			t.Fatalf("rewrite (clr=%v): %v", clr, err)
		}

		expected := thinPhase1Payload("ABYLA_I3F", clr)[ttcDataFlagsSize:]
		if !bytes.Equal(out, expected) {
			t.Fatalf("dotted rewrite mismatch (clr=%v):\n got %x\nwant %x", clr, out, expected)
		}

		if bytes.Contains(out, []byte("clairambault")) {
			t.Fatalf("old username fragment left behind (clr=%v): %x", clr, out)
		}
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
	buf = append(buf, []byte("__KV_TAIL__")...)

	return buf
}
