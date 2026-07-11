package oracle

import (
	"encoding/binary"
	"strconv"
)

// OCI thick clients (sqlplus / OCI-based drivers) marshal the AUTH TTC messages
// in a "wide" wire encoding that differs from the compact one go-ora and
// python-oracledb thin use:
//
//   - Every length and flag is a 4-byte little-endian integer (uint32) instead
//     of a TTC compressed integer.
//   - String/raw values are still CLR-framed (1-byte chunk length + bytes for
//     short values), preceded by the 4-byte total length.
//   - The AUTH KV dictionary is [msg-code 0x08][uint16-LE pair count][pairs...].
//   - Absent user_id_len header: the Phase 1 username is a bare 1-byte-length
//     token (handled by spliceLenPrefixedUsername).
//
// This file provides the wide-encoding equivalents of the compact KV helpers so
// the auth pipeline can speak either dialect based on session.clientWideFormat.

// ociWideSentinel is the 8-byte pointer placeholder (0xfe + 7×0xff) OCI thick
// clients emit in place of NULL ub4/ub8 fields. Its presence near the start of
// an AUTH Phase 1 body reliably identifies the wide encoding.
var ociWideSentinel = []byte{0xfe, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}

// isOCIWideAuthPhase1 reports whether an AUTH Phase 1 body uses the OCI wide
// encoding. body is the TTC message (piggyback header included), i.e. the
// packet payload after the 2-byte data-flags prefix.
func isOCIWideAuthPhase1(body []byte) bool {
	head := body
	if len(head) > 48 {
		head = head[:48]
	}

	return indexOfBytes(head, ociWideSentinel) >= 0
}

// wideKVPair is one decoded key/value/flag triple.
type wideKVPair struct {
	Key      []byte
	Value    []byte
	Flag     int
	Consumed int
}

// readWideKVPair decodes one wide-encoded KV pair from buf: [uint32-LE keyLen
// hint][CLR key][uint32-LE valLen hint][CLR value if hint>0][uint32-LE flag].
// The uint32 lengths are buffer-size hints — the authoritative length comes
// from the self-delimiting CLR framing (short form, or 0xfe-chunked long form),
// so the value is decoded with the canonical readCLR. ok=false when truncated.
func readWideKVPair(buf []byte) (wideKVPair, bool) {
	p := 0

	if _, ok := readWideUint32(buf, &p); !ok {
		return wideKVPair{}, false
	}

	key, kn := readCLR(buf[p:])
	if kn == 0 {
		return wideKVPair{}, false
	}

	p += kn

	valHint, ok := readWideUint32(buf, &p)
	if !ok {
		return wideKVPair{}, false
	}

	var val []byte
	if valHint > 0 {
		v, vn := readCLR(buf[p:])
		if vn == 0 {
			return wideKVPair{}, false
		}

		val = v
		p += vn
	}

	flag, ok := readWideUint32(buf, &p)
	if !ok {
		return wideKVPair{}, false
	}

	return wideKVPair{Key: key, Value: val, Flag: int(flag), Consumed: p}, true
}

// readWideUint32 reads a 4-byte little-endian integer, advancing *p.
func readWideUint32(buf []byte, p *int) (uint32, bool) {
	if *p+4 > len(buf) {
		return 0, false
	}

	v := binary.LittleEndian.Uint32(buf[*p:])
	*p += 4

	return v, true
}

// writeWideUint32 appends a 4-byte little-endian integer.
func writeWideUint32(buf []byte, v uint32) []byte {
	var tmp [4]byte
	binary.LittleEndian.PutUint32(tmp[:], v)

	return append(buf, tmp[:]...)
}

// buildAuthChallengeWide constructs the O5LOGON AUTH Phase 1 challenge in the
// OCI wide encoding, the client-facing counterpart of buildAuthChallenge. The
// message is [data-flags 0x2000][0x08 dict code][uint16-LE pair count][wide KV
// pairs]. In customHash mode (pbkdf2ChkSaltHex non-empty) the three PBKDF2
// fields are appended, matching what real Oracle sends to an OCI client.
func buildAuthChallengeWide(encServerSessKey, authVfrData, pbkdf2ChkSaltHex string, vgenCount, sderCount, verifierType int) []byte {
	pairs := 2
	if pbkdf2ChkSaltHex != "" {
		pairs += 3
	}

	buf := make([]byte, 0, 256)
	buf = append(buf, 0x20, 0x00, byte(TTCFuncResponse)) // data flags (END_OF_RESPONSE) + 0x08
	buf = binary.LittleEndian.AppendUint16(buf, uint16(pairs))
	buf = writeWideKV(buf, authKeySessKey, encServerSessKey, 1)
	buf = writeWideKV(buf, authKeyVfrData, authVfrData, verifierType)

	if pbkdf2ChkSaltHex != "" {
		buf = writeWideKV(buf, authKeyPbkdf2CskSalt, pbkdf2ChkSaltHex, 0)
		buf = writeWideKV(buf, authKeyPbkdf2VgenCount, strconv.Itoa(vgenCount), 0)
		buf = writeWideKV(buf, authKeyPbkdf2SderCount, strconv.Itoa(sderCount), 0)
	}

	return buf
}

// findWideKVValue locates a wide-encoded KV pair by key name and returns its
// value. In the wide layout the value directly follows the key bytes as
// [uint32-LE valLen][1-byte chunk][val]. Returns "" if not found.
func findWideKVValue(payload []byte, key string) string {
	i := indexOfBytes(payload, []byte(key))
	if i < 0 {
		return ""
	}

	p := i + len(key)

	valHint, ok := readWideUint32(payload, &p)
	if !ok || valHint == 0 {
		return ""
	}

	val, vn := readCLR(payload[p:])
	if vn == 0 {
		return ""
	}

	return string(val)
}

// parseAuthPhase2Wide extracts AUTH_SESSKEY and AUTH_PASSWORD from an OCI
// wide-encoded AUTH Phase 2 body (the packet payload after the 2-byte
// data-flags prefix).
func parseAuthPhase2Wide(payload []byte) (sessKey, password string) {
	return findWideKVValue(payload, authKeySessKey), findWideKVValue(payload, authKeyPassword)
}

// rewriteAuthPhase2Wide rewrites an OCI wide-encoded AUTH Phase 2 body for the
// upstream: swaps the username, and replaces the AUTH_SESSKEY / AUTH_PASSWORD /
// AUTH_PBKDF2_SPEEDY_KEY values with the upstream-derived ones in sec. All other
// KV pairs pass through so the upstream's AUTH OK stays conditioned on the
// client's real identity. Matches the rewriteAuthPhase2 signature.
func rewriteAuthPhase2Wide(body []byte, newUsername string, sec *upstreamAuthSecrets) ([]byte, error) {
	spliced, ok := spliceLenPrefixedUsername(body, newUsername)
	if !ok {
		return nil, ErrAuthPhase2Rewrite
	}

	authIdx := indexOfBytes(spliced, []byte(authKeyPrefix))
	if authIdx < 5 {
		return nil, ErrAuthPhase2Rewrite
	}

	// A wide KV pair starts with [uint32 keyLen][1-byte CLR len] before the key.
	kvStart := authIdx - 5

	out := append([]byte{}, spliced[:kvStart]...)
	p := kvStart

	for p < len(spliced) {
		pair, ok := readWideKVPair(spliced[p:])
		if !ok {
			break
		}

		p += pair.Consumed

		value := string(pair.Value)

		switch string(pair.Key) {
		case authKeySessKey:
			value = sec.encClientSessKey
		case authKeyPassword:
			value = sec.encPassword
		case pbkdf2SpeedyKeyLabel:
			value = sec.eSpeedyKey
		}

		out = writeWideKV(out, string(pair.Key), value, pair.Flag)
	}

	// Preserve any trailing bytes after the last KV pair.
	out = append(out, spliced[p:]...)

	return out, nil
}

// writeWideKV appends one wide-encoded KV pair: [u32 keyLen][CLR key][u32 valLen]
// [CLR val][u32 flag]. An empty value emits length 0 with no CLR body, matching
// what real Oracle sends.
func writeWideKV(buf []byte, key, value string, flag int) []byte {
	buf = writeWideUint32(buf, uint32(len(key)))
	buf = appendWideCLR(buf, []byte(key))

	buf = writeWideUint32(buf, uint32(len(value)))
	if len(value) > 0 {
		buf = appendWideCLR(buf, []byte(value))
	}

	buf = writeWideUint32(buf, uint32(flag))

	return buf
}

const (
	// wideCLRChunk is the chunk size used inside the 0xfe long form.
	wideCLRChunk = 64
	// wideCLRShortMax is the largest value emitted as a single short-form CLR
	// chunk (length byte 0x01..0xfc). 0xfd..0xff are reserved markers, and 0xfe
	// introduces the chunked long form, so anything larger uses long form.
	wideCLRShortMax = 0xfc
)

// appendWideCLR appends data using the canonical Oracle CLR framing that
// readCLR decodes: a single [len][data] short-form chunk for values up to
// wideCLRShortMax, or the 0xfe long form ([0xfe] + 64-byte chunks + 0x00) for
// larger values.
func appendWideCLR(buf, data []byte) []byte {
	if len(data) <= wideCLRShortMax {
		buf = append(buf, byte(len(data)))

		return append(buf, data...)
	}

	buf = append(buf, 0xfe)

	for len(data) > 0 {
		n := len(data)
		if n > wideCLRChunk {
			n = wideCLRChunk
		}

		buf = append(buf, byte(n))
		buf = append(buf, data[:n]...)
		data = data[n:]
	}

	return append(buf, 0x00)
}
