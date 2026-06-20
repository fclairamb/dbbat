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
	authIdx := bytes.Index(body, []byte("AUTH_"))
	if authIdx < 2 {
		return nil, false
	}

	const maxFramingGap = 8

	// Locate the username as the identifier run ending just before the first
	// AUTH_ key (see usernameBeforeAuthKV). Works for both length-prefixed
	// (go-ora, python-oracledb thin) and bare (SQLcl/JDBC thin) usernames.
	end := authIdx

	for gap := 0; end > 0 && !isIdentifierByte(body[end-1]); gap++ {
		if gap >= maxFramingGap {
			return nil, false
		}

		end--
	}

	start := end
	for start > 0 && isIdentifierByte(body[start-1]) && end-start < 30 {
		start--
	}

	if start == end || knownAuthKeys[strings.ToUpper(string(body[start:end]))] {
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

	// The preamble also carries a user_id_len (== old username length). Both it
	// and the CLR prefix must be rewritten or the upstream reads a stale count
	// and overflows (ORA-03120 / ORA-03146). OCI (sqlplus) encodes it as a 4-byte
	// little-endian field (oldLen 00 00 00) sitting tens of bytes ahead of the
	// username; thin clients use a single byte close to it.
	userIDLenPos := -1

	if payloadUsesWideKVEncoding(body) {
		for i := fieldStart - 4; i >= 2; i-- {
			if body[i] == byte(oldLen) && body[i+1] == 0 && body[i+2] == 0 && body[i+3] == 0 {
				userIDLenPos = i

				break
			}
		}
	} else {
		for i := fieldStart - 1; i >= 2 && i >= fieldStart-12; i-- {
			if body[i] == byte(oldLen) {
				userIDLenPos = i

				break
			}
		}
	}

	out := make([]byte, 0, len(body)+len(newUsername))

	if userIDLenPos >= 0 {
		out = append(out, body[:userIDLenPos]...)
		out = append(out, newLen) // user_id_len
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
