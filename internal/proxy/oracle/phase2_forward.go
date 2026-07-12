package oracle

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
)

// replaceAuthKVValue finds the TTC AUTH key in body and rebuilds its KV pair
// with newValue, preserving the key and flag. Returns body unchanged if the key
// isn't found or its framing doesn't parse. Used to splice the upstream-derived
// AUTH_SESSKEY / AUTH_PASSWORD / AUTH_PBKDF2_SPEEDY_KEY into a Phase 2 body
// without relying on the preamble layout (which varies by client). wide selects
// the OCI fixed 4-byte little-endian length/flag encoding instead of the
// compressed form thin clients use.
func replaceAuthKVValue(body []byte, key, newValue string, wide bool) []byte {
	if wide {
		return replaceAuthKVValueWide(body, key, newValue)
	}

	keyB := []byte(key)

	idx := bytes.Index(body, keyB)
	if idx < 0 {
		return body
	}

	// The KV pair is [keyLen (compressed int)][CLR-len][key][valLen][CLR val][flag].
	// keyLen is a compressed int (e.g. 0x01 0x0c for 12), not a single byte, so
	// the pair starts len(keyLenEnc)+1 bytes before the key bytes.
	keyLenEnc := ttcCompressedUint(uint64(len(key)))
	kvStart := idx - 1 - len(keyLenEnc)

	if kvStart < 0 || !bytes.Equal(body[kvStart:kvStart+len(keyLenEnc)], keyLenEnc) || int(body[idx-1]) != len(key) {
		return body
	}

	pos := idx + len(key)

	_, n := readCompressedInt(body[pos:])
	if n == 0 {
		return body
	}

	valPos := pos + n

	_, clrN := readCLR(body[valPos:])
	if clrN == 0 {
		return body
	}

	flagPos := valPos + clrN

	flagVal, fn := readCompressedInt(body[flagPos:])
	if fn == 0 {
		return body
	}

	end := flagPos + fn

	out := make([]byte, 0, kvStart+len(body)-end+len(newValue)+8)
	out = append(out, body[:kvStart]...)
	out = append(out, ttcKeyVal(key, newValue, flagVal)...)
	out = append(out, body[end:]...)

	return out
}

// replaceAuthKVValueWide is the OCI (4-byte little-endian) counterpart of
// replaceAuthKVValue. The KV pair is
// [keyLen:4 LE][CLR-len][key][valLen:4 LE][CLR val][flag:4 LE]. It preserves the
// key side verbatim (the 4-byte keyLen field is sometimes a buffer size, not the
// exact key length) and rewrites only the value side: a fresh 4-byte valLen, the
// CLR-encoded new value, and the original flag.
//
// The 4-byte valLen convention is client-specific and must be preserved: the
// DB-bundled 23.26 OCI client sends the plain value byte length, while the
// macOS/Windows Oracle Instant Client 23.3 sends a UTF-8 max-expansion buffer
// size (3x the CLR length) for EVERY value — and Oracle 23ai, parsing at that
// client's caps level, rejects a spliced plain length with ORA-28041
// ("authentication protocol internal error"). Mirror whichever convention the
// original pair used.
func replaceAuthKVValueWide(body []byte, key, newValue string) []byte {
	keyB := []byte(key)

	idx := bytes.Index(body, keyB)
	if idx < 0 {
		return body
	}

	// The byte before the key is its CLR length and must equal the key length —
	// confirms this is a key boundary, not a substring inside a value.
	if idx < 1 || int(body[idx-1]) != len(key) {
		return body
	}

	pos := idx + len(key) // start of the value side: [valLen:4 LE][CLR val][flag:4 LE]
	if pos+4 > len(body) {
		return body
	}

	origValLen := binary.LittleEndian.Uint32(body[pos : pos+4])
	valPos := pos + 4 // skip 4-byte LE valLen

	origVal, clrN := readCLR(body[valPos:])
	if clrN == 0 {
		return body
	}

	flagPos := valPos + clrN
	if flagPos+4 > len(body) {
		return body
	}

	flagVal := binary.LittleEndian.Uint32(body[flagPos : flagPos+4])
	end := flagPos + 4

	newValLen := uint32(len(newValue))
	if len(origVal) > 0 && origValLen == uint32(3*len(origVal)) {
		newValLen = uint32(3 * len(newValue))
	}

	out := make([]byte, 0, pos+len(body)-end+len(newValue)+12)
	out = append(out, body[:pos]...) // key side preserved verbatim
	out = binary.LittleEndian.AppendUint32(out, newValLen)
	out = append(out, ttcClr([]byte(newValue))...)
	out = binary.LittleEndian.AppendUint32(out, flagVal)
	out = append(out, body[end:]...)

	return out
}

// rewriteAuthPhase2Anchored rewrites the username (preamble-independent, via the
// AUTH_* anchor) and splices the upstream-derived AUTH_SESSKEY / AUTH_PASSWORD /
// AUTH_PBKDF2_SPEEDY_KEY values by key search. Handles client Phase 2 framings
// the fixed-offset parser misreads (notably python-oracledb thin's 18453 login,
// whose preamble has an extra byte so hasUsername is read from the wrong offset).
func rewriteAuthPhase2Anchored(body []byte, newUsername string, sec *upstreamAuthSecrets) ([]byte, bool) {
	out, ok := rewriteAuthPhase1UsernameAnchored(body, newUsername)
	if !ok {
		return nil, false
	}

	wide := payloadUsesWideKVEncoding(body)

	out = replaceAuthKVValue(out, authKeySessKey, sec.encClientSessKey, wide)
	out = replaceAuthKVValue(out, authKeyPassword, sec.encPassword, wide)

	if sec.eSpeedyKey != "" {
		out = replaceAuthKVValue(out, pbkdf2SpeedyKeyLabel, sec.eSpeedyKey, wide)
	}

	return out, true
}

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
	// Preferred: anchor on the AUTH_* keys (handles all client preambles,
	// including python-oracledb thin's 18453 login). Fall back to the
	// fixed-offset parser only if the anchor can't locate the username.
	if out, ok := rewriteAuthPhase2Anchored(body, newUsername, sec); ok {
		return out, nil
	}

	hdr, err := parseAuthPhase2Header(body)
	if err != nil {
		return nil, err
	}

	rewrittenPairs, err := rewritePhase2KVPairs(body[hdr.usernameEnd:], hdr.pairCount, sec)
	if err != nil {
		return nil, err
	}

	return assembleAuthPhase2(body, hdr, newUsername, rewrittenPairs), nil
}

// authPhase2Header captures the byte boundaries needed to splice in a new
// username and KV-pair set.
type authPhase2Header struct {
	hasUsername  bool
	hasCLRPrefix bool
	modeStart    int
	modeSize     int
	pairCount    int
	usernameEnd  int
}

// parseAuthPhase2Header walks the Phase 2 preamble — header bytes, has-username
// flag, user_id_len, mode, pair_count, marker, and username field — returning
// the offsets needed to reassemble the body.
func parseAuthPhase2Header(body []byte) (authPhase2Header, error) {
	const headerLen = 3 // [03 73 b0]

	out := authPhase2Header{}

	if len(body) < headerLen+1 {
		return out, fmt.Errorf("%w: body too short for header", ErrAuthPhase2Rewrite)
	}

	if body[0] != byte(TTCFuncPiggyback) || body[1] != PiggybackSubAuth2 {
		return out, fmt.Errorf("%w: not a Phase 2 piggyback", ErrAuthPhase2Rewrite)
	}

	pos := headerLen
	out.hasUsername = body[pos] == 0x01
	pos++

	userLen, err := readPhase2UserLen(body, &pos, out.hasUsername)
	if err != nil {
		return out, err
	}

	_, modeSize := readCompressedInt(body[pos:])
	if modeSize == 0 {
		return out, fmt.Errorf("%w: missing mode", ErrAuthPhase2Rewrite)
	}

	out.modeStart = pos
	out.modeSize = modeSize
	pos += modeSize

	if pos >= len(body) {
		return out, fmt.Errorf("%w: truncated after mode", ErrAuthPhase2Rewrite)
	}

	// Skip 1-byte padding (always 0x01).
	pos++

	pairCount, pairCountSize := readCompressedInt(body[pos:])
	if pairCountSize == 0 || pairCount < 0 {
		return out, fmt.Errorf("%w: invalid pair_count", ErrAuthPhase2Rewrite)
	}

	out.pairCount = pairCount
	pos += pairCountSize

	if pos+2 > len(body) {
		return out, fmt.Errorf("%w: truncated 0x01 0x01 marker", ErrAuthPhase2Rewrite)
	}

	// Skip 0x01 0x01.
	pos += 2

	out.usernameEnd = pos

	if out.hasUsername {
		out.hasCLRPrefix = detectUsernameEncoding(body[pos:], userLen)

		consumed := userLen
		if out.hasCLRPrefix {
			consumed = userLen + 1
		}

		if len(body) < pos+consumed {
			return out, fmt.Errorf("%w: username field truncated", ErrAuthPhase2Rewrite)
		}

		out.usernameEnd = pos + consumed
	}

	return out, nil
}

// readPhase2UserLen advances pos past the user_id_len field and returns the
// length value (0 when hasUsername is false, in which case it skips a single
// padding byte instead).
func readPhase2UserLen(body []byte, pos *int, hasUsername bool) (int, error) {
	if !hasUsername {
		if len(body) < *pos+1 {
			return 0, fmt.Errorf("%w: truncated zero user marker", ErrAuthPhase2Rewrite)
		}

		*pos++

		return 0, nil
	}

	ul, n := readCompressedInt(body[*pos:])
	if n == 0 || ul <= 0 || ul > 128 {
		return 0, fmt.Errorf("%w: invalid user_id_len", ErrAuthPhase2Rewrite)
	}

	*pos += n

	return ul, nil
}

// assembleAuthPhase2 builds the rewritten body with the new username and
// pre-rewritten KV pairs, preserving the original header / mode / marker bytes.
func assembleAuthPhase2(body []byte, hdr authPhase2Header, newUsername string, rewrittenPairs []byte) []byte {
	const headerLen = 3

	out := make([]byte, 0, len(body)+len(newUsername))
	out = append(out, body[:headerLen]...)

	if hdr.hasUsername {
		out = append(out, 0x01)
		out = append(out, ttcCompressedUint(uint64(len(newUsername)))...)
	} else {
		out = append(out, 0x00, 0x00)
	}

	out = append(out, body[hdr.modeStart:hdr.modeStart+hdr.modeSize]...)
	out = append(out, 0x01)
	out = append(out, ttcCompressedUint(uint64(hdr.pairCount))...)
	out = append(out, 0x01, 0x01)

	if hdr.hasUsername {
		if hdr.hasCLRPrefix {
			out = append(out, byte(len(newUsername)))
		}

		out = append(out, []byte(newUsername)...)
	}

	out = append(out, rewrittenPairs...)

	return out
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

		pair, ok := readAuthKVPair(buf[pos:], false)
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
