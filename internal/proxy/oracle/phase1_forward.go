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
	if len(body) < 2 {
		return nil, fmt.Errorf("%w: body too short for header", ErrAuthPhase1Rewrite)
	}

	if body[0] != byte(TTCFuncPiggyback) || body[1] != PiggybackSubAuth1 {
		return nil, fmt.Errorf("%w: not a Phase 1 piggyback", ErrAuthPhase1Rewrite)
	}

	// The framing bytes between the [03 76] sub-op and the user_id_len vary by
	// client: go-ora / SQLcl JDBC use a 2-byte trailer (headerLen 4), while
	// python-oracledb thin inserts one extra byte (headerLen 5). Try each and
	// keep the alignment whose username field is followed by a valid AUTH_* KV
	// pair — the only layout that will parse correctly upstream.
	for _, headerLen := range [...]int{4, 5} {
		out, ok := rewriteAuthPhase1AtHeader(body, newUsername, headerLen)
		if ok {
			return out, nil
		}
	}

	// OCI thick clients (sqlplus) use a wholly different "wide" wire encoding
	// (4/8-byte little-endian counts, 0xfe-sentinel pointer placeholders) with
	// no compressed-int user_id_len header — the username is just a bare
	// 1-byte-length token before the first AUTH_* KV pair. Splice it in place.
	if out, ok := spliceLenPrefixedUsername(body, newUsername); ok {
		return out, nil
	}

	return nil, fmt.Errorf("%w: could not align user_id_len / username", ErrAuthPhase1Rewrite)
}

// spliceLenPrefixedUsername replaces the [1-byte length][username] token that
// immediately precedes the first AUTH_* KV pair with newUsername, updating the
// length byte. It is wire-encoding agnostic: it locates the username by anchor
// (the first AUTH_* key) rather than by parsing the client-specific header, so
// it handles the OCI wide format whose header the compressed-int parsers can't
// read. Returns ok=false if no length-prefixed username can be located.
func spliceLenPrefixedUsername(body []byte, newUsername string) ([]byte, bool) {
	start, length, ok := findLenPrefixedUsername(body)
	if !ok {
		return nil, false
	}

	out := make([]byte, 0, len(body)-length+len(newUsername))
	out = append(out, body[:start-1]...)
	out = append(out, byte(len(newUsername)))
	out = append(out, []byte(newUsername)...)
	out = append(out, body[start+length:]...)

	return out, true
}

// findLenPrefixedUsername locates a [1-byte length][printable username] token
// sitting just before the first AUTH_* KV key in an AUTH body. Returns the
// username's start offset and length. The username is the printable token
// closest to (and before) the first AUTH_* key whose preceding byte equals its
// length — the framing between it and the key is a few encoding bytes.
func findLenPrefixedUsername(body []byte) (start, length int, ok bool) {
	authIdx := indexOfBytes(body, []byte(authKeyPrefix))
	if authIdx < 2 {
		return 0, 0, false
	}

	// The username ends within a small framing window before the first key.
	const maxFraming = 8

	lo := authIdx - maxFraming
	if lo < 2 {
		lo = 2
	}

	for uEnd := authIdx - 1; uEnd >= lo; uEnd-- {
		for l := 1; l <= 30 && uEnd-l-1 >= 0; l++ {
			s := uEnd - l
			if int(body[s-1]) != l {
				continue
			}

			if isPrintableASCII(body[s:uEnd]) {
				return s, l, true
			}
		}
	}

	return 0, 0, false
}

// indexOfBytes returns the first index of sub in b, or -1.
func indexOfBytes(b, sub []byte) int {
	for i := 0; i+len(sub) <= len(b); i++ {
		if string(b[i:i+len(sub)]) == string(sub) {
			return i
		}
	}

	return -1
}

// extractPhase1Username returns the client username from an AUTH Phase 1 body,
// trying each known header framing (go-ora/JDBC 4-byte, python-oracledb 5-byte)
// and both username encodings (CLR-prefixed, bare). Validated against the
// trailing AUTH_* KV pair, so it handles JDBC's bare uppercase username where
// the compressed-int / fallback scan otherwise misreads a truncated name.
func extractPhase1Username(body []byte) (string, bool) {
	for _, headerLen := range [...]int{4, 5} {
		if name, ok := phase1UsernameAtHeader(body, headerLen); ok {
			return name, true
		}
	}

	return "", false
}

// phase1UsernameAtHeader extracts the username assuming a specific header length.
func phase1UsernameAtHeader(body []byte, headerLen int) (string, bool) {
	if len(body) < headerLen+2 || body[0] != byte(TTCFuncPiggyback) || body[1] != PiggybackSubAuth1 {
		return "", false
	}

	rest := body[headerLen:]

	userLen, n := readCompressedInt(rest)
	if n == 0 || userLen <= 0 || userLen > 128 {
		return "", false
	}

	rest = rest[n:]

	if _, n = readCompressedInt(rest); n == 0 {
		return "", false
	}

	rest = rest[n:]

	const magicLen = 5
	if len(rest) < magicLen {
		return "", false
	}

	rest = rest[magicLen:]

	hasCLRPrefix := detectUsernameEncoding(rest, userLen)

	start := 0
	if hasCLRPrefix {
		start = 1
	}

	if len(rest) < start+userLen {
		return "", false
	}

	name := rest[start : start+userLen]
	if !isPrintableASCII(name) {
		return "", false
	}

	if !authKVTailLooksValid(rest[start+userLen:]) {
		return "", false
	}

	return string(name), true
}

// rewriteAuthPhase1AtHeader attempts the username splice assuming a specific
// header length (bytes before user_id_len). Returns ok=false if the layout does
// not parse or the username is not followed by a plausible AUTH_* KV pair.
func rewriteAuthPhase1AtHeader(body []byte, newUsername string, headerLen int) ([]byte, bool) {
	if len(body) < headerLen {
		return nil, false
	}

	rest := body[headerLen:]

	userLen, n := readCompressedInt(rest)
	if n == 0 || userLen <= 0 || userLen > 128 {
		return nil, false
	}

	userLenBytes := n
	rest = rest[n:]

	_, n = readCompressedInt(rest)
	if n == 0 {
		return nil, false
	}

	modeBytes := n
	rest = rest[n:]

	const magicLen = 5
	if len(rest) < magicLen {
		return nil, false
	}

	rest = rest[magicLen:]

	hasCLRPrefix := detectUsernameEncoding(rest, userLen)

	consumed := userLen
	if hasCLRPrefix {
		consumed = userLen + 1
	}

	if len(rest) < consumed {
		return nil, false
	}

	tail := rest[consumed:]
	if !authKVTailLooksValid(tail) {
		return nil, false
	}

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

	return out, true
}

// authKVTailLooksValid reports whether the bytes following the username field
// begin a plausible AUTH_* key-value pair (compressed-int key length, CLR key
// bytes starting with "AUTH_"). Used by both Phase 1 and Phase 2 to confirm the
// header alignment picked the real username boundary rather than a coincidental
// one.
func authKVTailLooksValid(tail []byte) bool {
	keyLen, n := readCompressedInt(tail)
	if n == 0 || keyLen < len(authKeyPrefix) || keyLen > 64 {
		return false
	}

	p := tail[n:]
	// CLR short form: 1-byte length then the key bytes.
	if len(p) < 1+keyLen {
		return false
	}

	if int(p[0]) != keyLen {
		return false
	}

	key := p[1 : 1+keyLen]

	return len(key) >= len(authKeyPrefix) && string(key[:len(authKeyPrefix)]) == authKeyPrefix
}

// authKeyPrefix is the common prefix of the KV keys that follow the username in
// an AUTH Phase 1 body (AUTH_TERMINAL, AUTH_PROGRAM_NM, ...).
const authKeyPrefix = "AUTH_"

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
