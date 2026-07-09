package oracle

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// Fixtures captured 2026-07-09 from real OCI clients against Oracle 23ai Free
// (gvenzl/oracle-free:23-slim):
//
//   - macOS Oracle Instant Client 23.3 (sqlplus 23.3.0.23.09, ARM64) — the
//     "instantclient" flavor: 4-byte LE length fields hold UTF-8 max-expansion
//     BUFFER sizes (3x the CLR byte length) in client→server messages.
//   - DB-bundled OCI client 23.26 (container sqlplus) — the "bundled" flavor:
//     the same fields hold plain byte lengths.
//
// Both must survive dbbat's Phase 1 / Phase 2 rewrites; see the regression
// stories on findUserIDLenPos and replaceAuthKVValueWide.

// instantclientPhase1Preamble reproduces the macOS instantclient 23.3 AUTH
// Phase 1 body for user "admin": the user-len field after the first pointer
// run is a 3x buffer size (0x0f = 15), and the KV pair count (5) EQUALS the
// username length — the collision that made the old backward scan corrupt the
// pair count (upstream then hangs waiting for a 6th pair).
func instantclientPhase1Body(username string) []byte {
	ptr := []byte{0xfe, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}

	body := []byte{0x03, 0x76, 0x02, 0x01, 0x03}
	body = append(body, ptr...)
	body = binary.LittleEndian.AppendUint32(body, uint32(3*len(username))) // buffer-sized user len
	body = binary.LittleEndian.AppendUint32(body, 1)                       // logon mode
	body = append(body, ptr...)
	body = binary.LittleEndian.AppendUint32(body, 5) // KV pair count
	body = append(body, ptr...)
	body = append(body, ptr...)
	body = append(body, byte(len(username)))
	body = append(body, []byte(username)...)

	// First KV pair (AUTH_TERMINAL, empty value), buffer-sized key length
	// (0x27 = 39 = 3*13) exactly as captured.
	body = binary.LittleEndian.AppendUint32(body, 39)
	body = append(body, 0x0d)
	body = append(body, []byte("AUTH_TERMINAL")...)
	body = binary.LittleEndian.AppendUint32(body, 0) // empty value
	body = binary.LittleEndian.AppendUint32(body, 0) // flag

	return body
}

// bundledPhase1Body reproduces the DB-bundled 23.26 OCI client's Phase 1 body
// for a username: the user-len field holds the PLAIN byte length and an extra
// zero dword follows the pair count.
func bundledPhase1Body(username string) []byte {
	ptr := []byte{0xfe, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}

	body := []byte{0x03, 0x76, 0x02, 0x02, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}
	body = append(body, ptr...)
	body = binary.LittleEndian.AppendUint32(body, uint32(len(username))) // plain user len
	body = binary.LittleEndian.AppendUint32(body, 1)                     // logon mode
	body = append(body, ptr...)
	body = binary.LittleEndian.AppendUint32(body, 5) // KV pair count
	body = binary.LittleEndian.AppendUint32(body, 0)
	body = append(body, ptr...)
	body = append(body, ptr...)
	body = append(body, byte(len(username)))
	body = append(body, []byte(username)...)

	body = binary.LittleEndian.AppendUint32(body, 13)
	body = append(body, 0x0d)
	body = append(body, []byte("AUTH_TERMINAL")...)
	body = binary.LittleEndian.AppendUint32(body, 0)
	body = binary.LittleEndian.AppendUint32(body, 0)

	return body
}

// TestRewritePhase1WideInstantclient covers the numPairs-collision regression:
// rewriting "admin" (5 chars — same as the KV pair count) to "SYSTEM" must
// update the 3x buffer-sized user-len field (15 → 18) and MUST NOT touch the
// pair count dword.
func TestRewritePhase1WideInstantclient(t *testing.T) {
	t.Parallel()

	body := instantclientPhase1Body("admin")

	out, ok := rewriteAuthPhase1UsernameAnchored(body, "SYSTEM")
	if !ok {
		t.Fatalf("rewriteAuthPhase1UsernameAnchored failed")
	}

	if !bytes.Contains(out, []byte("SYSTEM")) {
		t.Fatalf("rewritten body does not contain new username")
	}

	// user-len field (after the FIRST pointer run) must be 3*len("SYSTEM") = 18.
	userLen := binary.LittleEndian.Uint32(out[13:17])
	if userLen != 18 {
		t.Fatalf("user-len field = %d, want 18 (3x buffer-size convention)", userLen)
	}

	// The KV pair count (after the SECOND pointer run) must still be 5.
	pairCount := binary.LittleEndian.Uint32(out[29:33])
	if pairCount != 5 {
		t.Fatalf("KV pair count corrupted: got %d, want 5", pairCount)
	}
}

// TestRewritePhase1WideBundledClient covers the DB-bundled 23.26 client: the
// plain-length user-len field must be rewritten to the new plain length.
func TestRewritePhase1WideBundledClient(t *testing.T) {
	t.Parallel()

	body := bundledPhase1Body("system")

	out, ok := rewriteAuthPhase1UsernameAnchored(body, "LONGERUSER")
	if !ok {
		t.Fatalf("rewriteAuthPhase1UsernameAnchored failed")
	}

	// user-len field (after the FIRST pointer run at offset 11) = plain 10.
	userLen := binary.LittleEndian.Uint32(out[19:23])
	if userLen != 10 {
		t.Fatalf("user-len field = %d, want 10 (plain-length convention)", userLen)
	}
}

// TestRewritePhase1WideDottedUsername covers the ORA-03146 regression: a login
// username containing a non-identifier character (the '.' in
// "florent.clairambault") must be located and rewritten whole. The old
// identifier-run walk stopped at the '.', capturing only "clairambault" — it
// then spliced the new user over that substring (leaving "florent." in front)
// and failed to update the user_id_len field, so the upstream saw a
// length/buffer mismatch and rejected AUTH Phase 1 with
// "ORA-03146: invalid buffer length for TTC field".
func TestRewritePhase1WideDottedUsername(t *testing.T) {
	t.Parallel()

	const user = "florent.clairambault" // 20 chars, contains a '.'

	body := instantclientPhase1Body(user)

	out, ok := rewriteAuthPhase1UsernameAnchored(body, "GLH")
	if !ok {
		t.Fatalf("rewriteAuthPhase1UsernameAnchored failed for dotted username")
	}

	// The username field must become exactly "GLH" — not "florent.GLH", which is
	// what a walk that truncated at the '.' produces.
	if bytes.Contains(out, []byte("florent")) || bytes.Contains(out, []byte("clairambault")) {
		t.Fatalf("dotted username not replaced whole; leftover fragment in body: %q", out)
	}

	if !bytes.Contains(out, []byte("GLH")) {
		t.Fatalf("rewritten body does not contain new username")
	}

	// user_id_len field (after the FIRST pointer run at offset 13) must be the
	// 3x buffer-size of the NEW username: 3*len("GLH") = 9.
	userLen := binary.LittleEndian.Uint32(out[13:17])
	if userLen != uint32(3*len("GLH")) {
		t.Fatalf("user-len field = %d, want %d (3x buffer-size of new username)", userLen, 3*len("GLH"))
	}

	// The CLR length prefix immediately before the username must equal the new
	// username length (3); otherwise the upstream overruns/underruns the field.
	glhIdx := bytes.Index(out, []byte("GLH"))
	if glhIdx < 1 || int(out[glhIdx-1]) != len("GLH") {
		t.Fatalf("CLR length prefix before username = %d, want %d", out[glhIdx-1], len("GLH"))
	}
}

// TestReplaceAuthKVValueWideBufferSized covers the ORA-28041 regression: the
// instantclient 23.3 sends every 4-byte value length as a 3x UTF-8
// max-expansion buffer size; the splice must mirror that convention, not write
// a plain length.
func TestReplaceAuthKVValueWideBufferSized(t *testing.T) {
	t.Parallel()

	// [keyLen:4][clr key][valLen:4 = 3x][clr val][flag:4]
	body := binary.LittleEndian.AppendUint32(nil, 36) // buffer-sized key length (3*12)
	body = append(body, 0x0c)
	body = append(body, []byte("AUTH_SESSKEY")...)
	body = binary.LittleEndian.AppendUint32(body, 24) // 3 * len("OLDVALUE")
	body = append(body, 0x08)
	body = append(body, []byte("OLDVALUE")...)
	body = binary.LittleEndian.AppendUint32(body, 1) // flag

	out := replaceAuthKVValueWide(body, "AUTH_SESSKEY", "NEWLONGERVALUE")

	valLenOff := 4 + 1 + 12
	valLen := binary.LittleEndian.Uint32(out[valLenOff : valLenOff+4])

	if valLen != uint32(3*len("NEWLONGERVALUE")) {
		t.Fatalf("valLen = %d, want %d (3x buffer-size convention)", valLen, 3*len("NEWLONGERVALUE"))
	}

	if !bytes.Contains(out, []byte("NEWLONGERVALUE")) {
		t.Fatalf("value not replaced")
	}

	flagOff := valLenOff + 4 + 1 + len("NEWLONGERVALUE")
	if flag := binary.LittleEndian.Uint32(out[flagOff : flagOff+4]); flag != 1 {
		t.Fatalf("flag = %d, want 1 (preserved)", flag)
	}
}

// TestReplaceAuthKVValueWidePlain keeps the plain-length convention (bundled
// 23.26 client) untouched.
func TestReplaceAuthKVValueWidePlain(t *testing.T) {
	t.Parallel()

	body := binary.LittleEndian.AppendUint32(nil, 12)
	body = append(body, 0x0c)
	body = append(body, []byte("AUTH_SESSKEY")...)
	body = binary.LittleEndian.AppendUint32(body, 8) // plain len("OLDVALUE")
	body = append(body, 0x08)
	body = append(body, []byte("OLDVALUE")...)
	body = binary.LittleEndian.AppendUint32(body, 0)

	out := replaceAuthKVValueWide(body, "AUTH_SESSKEY", "NEWVAL")

	valLenOff := 4 + 1 + 12
	if valLen := binary.LittleEndian.Uint32(out[valLenOff : valLenOff+4]); valLen != 6 {
		t.Fatalf("valLen = %d, want 6 (plain-length convention)", valLen)
	}
}
