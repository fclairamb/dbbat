package oracle

import (
	"errors"
	"fmt"
	"strings"
)

// rewriteAuthPhase2 takes a TTC AUTH Phase 2 body (everything after the TNS
// frame header AND the 2-byte data-flags prefix — i.e. what would follow
// ttcDataFlagsSize in pkt.Payload) and returns a new body in which:
//
//   - The username is replaced with newUsername.
//   - AUTH_SESSKEY value is replaced with sec.encClientSessKey.
//   - AUTH_PASSWORD value is replaced with sec.encPassword.
//   - AUTH_PBKDF2_SPEEDY_KEY value (when present) is replaced with sec.eSpeedyKey.
//
// All other KV pairs (AUTH_TERMINAL, AUTH_PROGRAM_NM, SESSION_CLIENT_DRIVER_NAME,
// AUTH_CONNECT_STRING, AUTH_COPYRIGHT, AUTH_ACL, AUTH_ALTER_SESSION, …) are
// passed through verbatim. This preserves the client's TTC capability surface
// so the upstream's AUTH OK is conditioned on the client's actual identity
// (notably: the AUTH_CONNECT_STRING / AUTH_ACL / driver-version triplet that
// JDBC thin sends and which standard go-ora does not).
//
// Phase 2 wire layout for go-ora-style clients:
//
//	[03 73 b0]                 -- piggyback header + sub-op + 1 client-specific byte
//	                              (0x00 for go-ora; 0x02 for JDBC thin)
//	[01]                       -- has-username flag
//	[compressed-int user_id_len]
//	[compressed-int mode]
//	[01]
//	[compressed-int pair_count]
//	[01 01]
//	[username bare OR CLR-prefixed]
//	[KV pairs...]
//
// Both observed username encodings are handled. The b0 byte at offset 2 is
// preserved verbatim (its meaning is opaque; preserving it keeps the upstream's
// caps-conditioned parser happy).
//
// The rewritten body is returned as a fresh slice; the input is not mutated.
func rewriteAuthPhase2(body []byte, newUsername string, sec *upstreamAuthSecrets) ([]byte, error) {
	const headerLen = 3 // [03 73 b0]

	if len(body) < headerLen+1 {
		return nil, fmt.Errorf("%w: body too short for header", ErrAuthPhase2Rewrite)
	}

	if body[0] != byte(TTCFuncPiggyback) || body[1] != PiggybackSubAuth2 {
		return nil, fmt.Errorf("%w: not a Phase 2 piggyback", ErrAuthPhase2Rewrite)
	}

	pos := headerLen
	hasUsername := body[pos] == 0x01
	pos++

	var userLen int

	if hasUsername {
		ul, n := readCompressedInt(body[pos:])
		if n == 0 || ul <= 0 || ul > 128 {
			return nil, fmt.Errorf("%w: invalid user_id_len", ErrAuthPhase2Rewrite)
		}

		userLen = ul
		pos += n
	} else {
		// has-username==0 layout uses [00 00] in place of the [01 user_id_len].
		if len(body) < pos+1 {
			return nil, fmt.Errorf("%w: truncated zero user marker", ErrAuthPhase2Rewrite)
		}

		pos++
	}

	_, modeSize := readCompressedInt(body[pos:])
	if modeSize == 0 {
		return nil, fmt.Errorf("%w: missing mode", ErrAuthPhase2Rewrite)
	}

	modeStart := pos
	pos += modeSize

	if pos >= len(body) {
		return nil, fmt.Errorf("%w: truncated after mode", ErrAuthPhase2Rewrite)
	}

	// Skip 1-byte padding (always 0x01).
	pos++

	pairCount, pairCountSize := readCompressedInt(body[pos:])
	if pairCountSize == 0 || pairCount < 0 {
		return nil, fmt.Errorf("%w: invalid pair_count", ErrAuthPhase2Rewrite)
	}

	pos += pairCountSize

	if pos+2 > len(body) {
		return nil, fmt.Errorf("%w: truncated 0x01 0x01 marker", ErrAuthPhase2Rewrite)
	}

	// Skip 0x01 0x01.
	pos += 2

	usernameEnd := pos
	hasCLRPrefix := false

	if hasUsername {
		hasCLRPrefix, _ = detectUsernameEncoding(body[pos:], userLen)

		consumed := userLen
		if hasCLRPrefix {
			consumed = userLen + 1
		}

		if len(body) < pos+consumed {
			return nil, fmt.Errorf("%w: username field truncated", ErrAuthPhase2Rewrite)
		}

		usernameEnd = pos + consumed
	}

	// Walk and rewrite KV pairs.
	rewrittenPairs, err := rewritePhase2KVPairs(body[usernameEnd:], pairCount, sec)
	if err != nil {
		return nil, err
	}

	// Reassemble the body.
	out := make([]byte, 0, len(body)+len(newUsername))

	// Header [03 73 b0] — preserve b0 verbatim (varies by client).
	out = append(out, body[:headerLen]...)

	if hasUsername {
		// has-username flag.
		out = append(out, 0x01)
		out = append(out, ttcCompressedUint(uint64(len(newUsername)))...)
	} else {
		out = append(out, 0x00, 0x00)
	}

	// Mode (verbatim — same NoNewPass | UserAndPass bits).
	out = append(out, body[modeStart:modeStart+modeSize]...)

	// 0x01 byte after mode.
	out = append(out, 0x01)
	// Pair count (verbatim).
	out = append(out, ttcCompressedUint(uint64(pairCount))...)
	// 0x01 0x01 marker.
	out = append(out, 0x01, 0x01)

	// Username — preserve original encoding (bare for JDBC, CLR-prefixed for go-ora).
	if hasUsername {
		if hasCLRPrefix {
			out = append(out, byte(len(newUsername)))
		}

		out = append(out, []byte(newUsername)...)
	}

	out = append(out, rewrittenPairs...)

	return out, nil
}

// rewritePhase2KVPairs walks pairCount TTC KV pairs in buf and returns a slice
// in which the AUTH_SESSKEY / AUTH_PASSWORD / AUTH_PBKDF2_SPEEDY_KEY values
// have been swapped to the upstream-derived ones in sec. Other pairs are
// copied verbatim.
//
// On any decoding error, returns ErrAuthPhase2Rewrite.
func rewritePhase2KVPairs(buf []byte, pairCount int, sec *upstreamAuthSecrets) ([]byte, error) {
	out := make([]byte, 0, len(buf))
	pos := 0

	for i := 0; i < pairCount; i++ {
		if pos >= len(buf) {
			return nil, fmt.Errorf("%w: truncated at pair %d/%d", ErrAuthPhase2Rewrite, i, pairCount)
		}

		pair, ok := readAuthKVPair(buf[pos:])
		if !ok {
			return nil, fmt.Errorf("%w: bad pair %d/%d", ErrAuthPhase2Rewrite, i, pairCount)
		}

		newValue, replaced := upstreamPhase2Value(string(pair.Key), sec)
		if replaced {
			out = append(out, ttcKeyVal(string(pair.Key), newValue, pair.Flag)...)
		} else {
			out = append(out, buf[pos:pos+pair.Consumed]...)
		}

		pos += pair.Consumed
	}

	return out, nil
}

// upstreamPhase2Value returns the upstream-derived value for an AUTH_*-key the
// client sent. ok=false signals "leave verbatim".
func upstreamPhase2Value(key string, sec *upstreamAuthSecrets) (string, bool) {
	switch strings.ToUpper(key) {
	case authKeySessKey:
		return sec.encClientSessKey, true
	case authKeyPassword:
		return sec.encPassword, true
	case pbkdf2SpeedyKeyLabel:
		if sec.eSpeedyKey == "" {
			return "", false
		}

		return sec.eSpeedyKey, true
	}

	return "", false
}

// ErrAuthPhase2Rewrite signals a Phase 2 body that does not match the
// expected piggyback / has-username / user_id_len / mode / pair_count layout
// or whose KV pairs cannot be parsed.
var ErrAuthPhase2Rewrite = errors.New("AUTH Phase 2 rewrite failed")
