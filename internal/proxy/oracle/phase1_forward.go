package oracle

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
)

// rewriteAuthPhase1UsernameAnchored locates the login username by anchoring on
// the AUTH_* key/value section that follows it (the same robust approach as
// usernameBeforeAuthKV) and splices in newUsername, updating its 1-byte CLR
// length prefix. This is preamble-layout independent, so it handles clients
// whose Phase 1 framing the fixed-offset parser misreads — notably
// python-oracledb thin's verifier-18453 login, where the fixed parser read
// user_id_len as 1 and produced a corrupt body the upstream rejects (ORA-03120).
// Returns ok=false if the username can't be located this way.
func rewriteAuthPhase1UsernameAnchored(body []byte, newUsername string) ([]byte, bool) {
	start, end, ok := locateAnchoredUsername(body)
	if !ok {
		return nil, false
	}

	oldLen := end - start
	newLen := byte(len(newUsername))

	// A 1-byte CLR length prefix precedes the username for go-ora / python; SQLcl
	// sends the bytes bare (length given only by user_id_len). Detect by checking
	// whether the preceding byte equals the username length.
	clrPrefixed := start > 0 && int(body[start-1]) == oldLen

	fieldStart := start
	if clrPrefixed {
		fieldStart = start - 1
	}

	userIDLenPos, userIDLenVal := findUserIDLenPos(body, fieldStart, oldLen, len(newUsername))

	out := make([]byte, 0, len(body)+len(newUsername))

	if userIDLenPos >= 0 {
		out = append(out, body[:userIDLenPos]...)
		out = append(out, userIDLenVal) // user_id_len (or its 3x buffer-size form)
		out = append(out, body[userIDLenPos+1:fieldStart]...)
	} else {
		out = append(out, body[:fieldStart]...)
	}

	if clrPrefixed {
		out = append(out, newLen) // CLR length prefix
	}

	out = append(out, []byte(newUsername)...)
	out = append(out, body[end:]...) // KV pairs

	return out, true
}

// locateAnchoredUsername returns the [start, end) byte range of the login
// username in a Phase 1 body by anchoring on the first AUTH_* key that follows
// it (see usernameBeforeAuthKV). Works for both length-prefixed (go-ora,
// python-oracledb thin) and bare (SQLcl/JDBC thin) usernames. ok=false if the
// run can't be located or resolves to a well-known AUTH key.
func locateAnchoredUsername(body []byte) (int, int, bool) {
	authIdx := bytes.Index(body, []byte("AUTH_"))
	if authIdx < 2 {
		return 0, 0, false
	}

	end := authIdx

	if payloadUsesWideKVEncoding(body) {
		// Wide (OCI) bodies frame the first key deterministically: a 4-byte LE
		// key length (sometimes a 3x buffer size) + a 1-byte CLR length — so
		// the username's last byte sits exactly 5 bytes before the key. The
		// non-identifier walk below is a trap here: instantclient 23.3's
		// buffer-sized AUTH_SESSKEY key length is 36 = 0x24 = '$', an Oracle
		// identifier byte, and the walk then anchors on that length byte
		// instead of the username — the "rewrite" splices the new username
		// over the key-length field and the upstream rejects the mangled
		// Phase 2 with ORA-28041.
		const wideKeyFraming = 5

		if authIdx < wideKeyFraming+1 {
			return 0, 0, false
		}

		end = authIdx - wideKeyFraming
	} else {
		const maxFramingGap = 8

		for gap := 0; end > 0 && !isIdentifierByte(body[end-1]); gap++ {
			if gap >= maxFramingGap {
				return 0, 0, false
			}

			end--
		}
	}

	start := end
	for start > 0 && isIdentifierByte(body[start-1]) && end-start < 30 {
		start--
	}

	if start == end || knownAuthKeys[strings.ToUpper(string(body[start:end]))] {
		return 0, 0, false
	}

	return start, end, true
}

// findUserIDLenPos returns the offset of the preamble user_id_len field and the
// byte to write there for the new username, or (-1, 0) if not found. Both it
// and the CLR prefix must be rewritten or the upstream reads a stale count and
// overflows (ORA-03120 / ORA-03146) — or, when the count grows, waits forever
// for bytes that never come.
//
// OCI (sqlplus) wide preamble — same shape for AUTH Phase 1 (03 76) and
// Phase 2 (03 73), captured from the DB-bundled 23.26 OCI client and the
// macOS/Windows Oracle Instant Client 23.3:
//
//	03 76 <seq> <variable...>
//	fe ff ff ff ff ff ff ff    first 8-byte pointer placeholder run
//	<user-len field: 4 LE>     == len(username)   (23.26 bundled client)
//	                           == 3*len(username) (instantclient 23.3 — a
//	                              UTF-8 max-expansion buffer size)
//	<logon mode: 4 LE>
//	... more pointer runs, the KV pair count, then the CLR username.
//
// The field is located by anchoring on the FIRST pointer run — never by
// scanning backward for a dword equal to oldLen: the KV pair count is also a
// small 4-byte LE integer (5 for OCI Phase 1) sitting between pointer runs, and
// a backward scan corrupts it whenever len(username) == numPairs (e.g. the
// 5-char "admin" — the upstream then waits for a 6th pair that never arrives
// and AUTH hangs).
//
// Thin clients use a single plain-length byte close to the username field.
func findUserIDLenPos(body []byte, fieldStart, oldLen, newLen int) (int, byte) {
	if payloadUsesWideKVEncoding(body) {
		ptrRun := []byte{0xfe, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}

		idx := bytes.Index(body[:fieldStart], ptrRun)
		if idx < 0 || idx+len(ptrRun)+4 > fieldStart {
			return -1, 0
		}

		pos := idx + len(ptrRun)
		if body[pos+1] != 0 || body[pos+2] != 0 || body[pos+3] != 0 {
			return -1, 0
		}

		switch int(body[pos]) {
		case oldLen:
			return pos, byte(newLen)
		case 3 * oldLen:
			return pos, byte(3 * newLen)
		default:
			return -1, 0
		}
	}

	for i := fieldStart - 1; i >= 2 && i >= fieldStart-12; i-- {
		if body[i] == byte(oldLen) {
			return i, byte(newLen)
		}
	}

	return -1, 0
}

// rewriteAuthPhase1Username takes a TTC AUTH Phase 1 body (everything after
// the TNS frame header AND the 2-byte data-flags prefix — i.e. what would
// follow ttcDataFlagsSize in pkt.Payload) and returns a new body in which the
// embedded username has been replaced with newUsername.
//
// Both wire encodings observed in the wild are preserved:
//
//   - go-ora / python-oracledb: 1-byte CLR length prefix followed by the
//     username bytes (matches PutClr for short strings).
//   - SQLcl JDBC thin: bare username bytes with no length prefix; the
//     length is given solely by the leading user_id_len compressed integer.
//
// Splicing keeps the original encoding style of the client; userLen is
// updated to the new username length.
//
// The rewritten body is returned as a fresh slice; the input is not
// mutated. An error is returned if the body does not look like AUTH Phase 1
// or the username field cannot be located. KV pairs that follow the
// username (AUTH_TERMINAL, AUTH_PROGRAM_NM, ...) are passed through
// unchanged so the upstream sees the client's actual TTC capabilities.
func rewriteAuthPhase1Username(body []byte, newUsername string) ([]byte, error) {
	const headerLen = 4 // [03 76 b0 b1] piggyback marker + sub-op + 2-byte trailer

	if len(body) < headerLen {
		return nil, fmt.Errorf("%w: body too short for header", ErrAuthPhase1Rewrite)
	}

	if body[0] != byte(TTCFuncPiggyback) || body[1] != PiggybackSubAuth1 {
		return nil, fmt.Errorf("%w: not a Phase 1 piggyback", ErrAuthPhase1Rewrite)
	}

	// Preferred: anchor on the AUTH_* keys (handles all client preambles,
	// including python-oracledb thin's 18453 login). Fall back to the
	// fixed-offset splice below only if the anchor can't find the username.
	if out, ok := rewriteAuthPhase1UsernameAnchored(body, newUsername); ok {
		return out, nil
	}

	rest := body[headerLen:]

	userLen, n := readCompressedInt(rest)
	if n == 0 || userLen <= 0 || userLen > 128 {
		return nil, fmt.Errorf("%w: invalid user_id_len", ErrAuthPhase1Rewrite)
	}

	userLenBytes := n
	rest = rest[n:]

	_, n = readCompressedInt(rest)
	if n == 0 {
		return nil, fmt.Errorf("%w: missing logon_mode", ErrAuthPhase1Rewrite)
	}

	modeBytes := n
	rest = rest[n:]

	const magicLen = 5
	if len(rest) < magicLen {
		return nil, fmt.Errorf("%w: missing 5-byte magic", ErrAuthPhase1Rewrite)
	}

	rest = rest[magicLen:]

	hasCLRPrefix := detectUsernameEncoding(rest, userLen)

	consumed := userLen
	if hasCLRPrefix {
		consumed = userLen + 1
	}

	if len(rest) < consumed {
		return nil, fmt.Errorf("%w: username field truncated", ErrAuthPhase1Rewrite)
	}

	tail := rest[consumed:]

	// Reassemble: header + new userLen + mode + magic + new username field + tail (KV pairs).
	preMagicLen := headerLen + userLenBytes + modeBytes + magicLen

	preMagic := body[:preMagicLen]
	// preMagic still contains the OLD userLen — we need to splice in the new one.
	headerSection := preMagic[:headerLen]
	modeAndMagic := preMagic[headerLen+userLenBytes:]

	newUserLen := ttcCompressedUint(uint64(len(newUsername)))

	out := make([]byte, 0, len(headerSection)+len(newUserLen)+len(modeAndMagic)+1+len(newUsername)+len(tail))
	out = append(out, headerSection...)
	out = append(out, newUserLen...)
	out = append(out, modeAndMagic...)

	if hasCLRPrefix {
		out = append(out, byte(len(newUsername)))
	}

	out = append(out, []byte(newUsername)...)
	out = append(out, tail...)

	return out, nil
}

// detectUsernameEncoding determines whether the buffer's username field uses
// the CLR-prefixed form (1-byte length followed by username) or the bare form
// (raw bytes). Used by both Phase 1 and Phase 2 forwarders to splice in a new
// username while preserving the client's original encoding.
func detectUsernameEncoding(rest []byte, userLen int) bool {
	if len(rest) >= userLen+1 && int(rest[0]) == userLen && isPrintableASCII(rest[1:1+userLen]) {
		return true
	}

	if len(rest) >= userLen && isPrintableASCII(rest[:userLen]) {
		return false
	}

	return false
}

// ErrAuthPhase1Rewrite signals a Phase 1 body that does not match the
// expected piggyback/sub-op/userLen/mode/magic/username layout.
var ErrAuthPhase1Rewrite = errors.New("AUTH Phase 1 rewrite failed")
