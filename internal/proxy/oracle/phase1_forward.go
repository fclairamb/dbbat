package oracle

import (
	"errors"
	"fmt"
)

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

	hasCLRPrefix, usernameBytes := detectUsernameEncoding(rest, userLen)

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

	_ = usernameBytes // sole purpose of detectUsernameEncoding's second return is documentation

	return out, nil
}

// detectUsernameEncoding determines whether the Phase 1 body uses the
// CLR-prefixed form (1-byte length followed by username) or the bare form
// (raw bytes). Returns (hasPrefix, usernameBytes). The usernameBytes return
// is informational; callers replace the field wholesale.
func detectUsernameEncoding(rest []byte, userLen int) (bool, []byte) {
	if len(rest) >= userLen+1 && int(rest[0]) == userLen && isPrintableASCII(rest[1:1+userLen]) {
		return true, rest[1 : 1+userLen]
	}

	if len(rest) >= userLen && isPrintableASCII(rest[:userLen]) {
		return false, rest[:userLen]
	}

	return false, nil
}

// ErrAuthPhase1Rewrite signals a Phase 1 body that does not match the
// expected piggyback/sub-op/userLen/mode/magic/username layout.
var ErrAuthPhase1Rewrite = errors.New("AUTH Phase 1 rewrite failed")
