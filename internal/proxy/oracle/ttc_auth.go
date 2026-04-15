package oracle

import (
	"encoding/binary"
	"strings"
)

// TTC AUTH key names used in Oracle authentication messages.
const (
	authKeyUsername = "AUTH_TERMINAL"
	authKeySessKey  = "AUTH_SESSKEY"
	authKeyVfrData  = "AUTH_VFR_DATA"
	authKeyPassword = "AUTH_PASSWORD"
)

// authKVPair represents a key-value pair in a TTC AUTH message.
type authKVPair struct {
	Key   string
	Value string
}

// parseAuthPhase1 extracts the username from a TTC AUTH Phase 1 message.
// AUTH Phase 1 is sent as func=0x03, sub=0x76 (PiggybackSubAuth1).
//
// The wire format has a variable-length preamble of TTC-encoded fields (logon mode,
// auth flags) before the username. The username is a length-prefixed string that
// appears before the first AUTH_* key-value pair. We scan for it by looking for
// a length prefix (2+) followed by that many printable ASCII characters.
func parseAuthPhase1(tnsDataPayload []byte) (string, error) {
	if len(tnsDataPayload) < ttcDataFlagsSize+4 {
		return "", ErrAuthPhase1TooShort
	}

	// Skip data flags (2 bytes) + func code (0x03) + sub-op (0x76)
	payload := tnsDataPayload[ttcDataFlagsSize+2:]

	// Scan for the first length-prefixed string that looks like a username.
	// The preamble contains small values (0x01, 0x05, etc.) — the username
	// is the first string with length >= 2 where all bytes are printable ASCII.
	for i := 0; i < len(payload)-2 && i < 64; i++ {
		strLen := int(payload[i])
		if strLen < 2 || strLen > 128 || i+1+strLen > len(payload) {
			continue
		}

		candidate := payload[i+1 : i+1+strLen]
		if isPrintableASCII(candidate) {
			return strings.ToUpper(string(candidate)), nil
		}
	}

	return "", ErrAuthPhase1BadUsername
}

// isPrintableASCII checks if all bytes are printable ASCII (letters, digits, _, $, #).
func isPrintableASCII(b []byte) bool {
	for _, c := range b {
		if (c < 'A' || c > 'Z') && (c < 'a' || c > 'z') &&
			(c < '0' || c > '9') && c != '_' && c != '$' && c != '#' {
			return false
		}
	}

	return true
}

// buildAuthChallenge constructs the TTC AUTH challenge response using Oracle's
// TTC wire encoding (compressed uint + CLR encoding).
//
// The challenge contains AUTH_SESSKEY and AUTH_VFR_DATA key-value pairs that the
// client uses for O5LOGON authentication.
func buildAuthChallenge(encServerSessKey, authVfrData string) []byte {
	buf := []byte{
		0x00, 0x00, // data flags
		byte(TTCFuncResponse), // 0x08
	}

	// Number of KV pairs (compressed uint)
	buf = append(buf, ttcCompressedUint(2)...)

	// AUTH_SESSKEY
	buf = append(buf, ttcKeyVal(authKeySessKey, encServerSessKey, 1)...)
	// AUTH_VFR_DATA
	buf = append(buf, ttcKeyVal(authKeyVfrData, authVfrData, 0)...)

	return buf
}

// buildAuthOK constructs the TTC AUTH success response.
func buildAuthOK() []byte {
	buf := []byte{
		0x00, 0x00, // data flags
		byte(TTCFuncResponse), // 0x08
	}
	// return code = 0 (compressed uint)
	buf = append(buf, 0x00)
	// pair count = 0
	buf = append(buf, 0x00)

	return buf
}

// buildAuthFailed constructs the TTC AUTH failure response with an ORA error code.
func buildAuthFailed(oraCode int, message string) []byte {
	buf := []byte{
		0x00, 0x00, // data flags
		byte(TTCFuncResponse), // 0x08
	}
	// return code (compressed uint)
	buf = append(buf, ttcCompressedUint(uint64(oraCode))...)
	// error message (CLR-encoded)
	buf = append(buf, ttcClr([]byte(message))...)

	return buf
}

// ttcKeyVal encodes a key-value pair using Oracle's TTC wire format.
// Matches go-ora's PutKeyVal: PutUint(keyLen) + PutClr(key) + PutUint(valLen) + PutClr(val) + PutInt(flag).
func ttcKeyVal(key, value string, flag uint8) []byte {
	var buf []byte
	keyBytes := []byte(key)
	valBytes := []byte(value)

	buf = append(buf, ttcCompressedUint(uint64(len(keyBytes)))...)
	buf = append(buf, ttcClr(keyBytes)...)
	buf = append(buf, ttcCompressedUint(uint64(len(valBytes)))...)
	buf = append(buf, ttcClr(valBytes)...)
	buf = append(buf, ttcCompressedUint(uint64(flag))...)

	return buf
}

// ttcCompressedUint encodes a uint as a compressed TTC integer.
// Zero = single 0x00. Otherwise: 1-byte count + big-endian bytes (no leading zeros).
func ttcCompressedUint(n uint64) []byte {
	if n == 0 {
		return []byte{0x00}
	}

	tmp := make([]byte, 8)
	binary.BigEndian.PutUint64(tmp, n)

	start := 0
	for start < len(tmp) && tmp[start] == 0 {
		start++
	}

	trimmed := tmp[start:]
	result := make([]byte, 1+len(trimmed))
	result[0] = byte(len(trimmed))
	copy(result[1:], trimmed)

	return result
}

// ttcClr encodes data in Oracle's CLR (Chunked Length-prefixed Raw) format.
// Short: 1-byte len + data. Long (>=252): 0xFE + chunked + 0x00 terminator.
func ttcClr(data []byte) []byte {
	if len(data) == 0 {
		return []byte{0x00}
	}

	if len(data) < 0xFC {
		buf := make([]byte, 1+len(data))
		buf[0] = byte(len(data))
		copy(buf[1:], data)

		return buf
	}

	var buf []byte
	buf = append(buf, 0xFE)

	chunkSize := 252
	for i := 0; i < len(data); i += chunkSize {
		end := i + chunkSize
		if end > len(data) {
			end = len(data)
		}

		chunk := data[i:end]
		buf = append(buf, byte(len(chunk)))
		buf = append(buf, chunk...)
	}

	buf = append(buf, 0x00)

	return buf
}

// parseAuthPhase2 extracts the client session key and encrypted password from AUTH Phase 2.
// AUTH Phase 2 is sent as func=0x03, sub=0x73 (PiggybackSubAuth2).
//
// The message contains key-value pairs including:
//   - AUTH_SESSKEY: encrypted client session key (hex string)
//   - AUTH_PASSWORD: encrypted password (hex string)
func parseAuthPhase2(tnsDataPayload []byte) (string, string, error) {
	if len(tnsDataPayload) < ttcDataFlagsSize+4 {
		return "", "", ErrAuthPhase2TooShort
	}

	// Skip data flags (2 bytes) + func code (0x03) + sub-op (0x73)
	offset := ttcDataFlagsSize + 2

	// Parse key-value pairs from the remaining payload
	pairs := parseTTCKVPairs(tnsDataPayload[offset:])

	var sessKey, password string

	for _, p := range pairs {
		switch strings.ToUpper(p.Key) {
		case authKeySessKey:
			sessKey = p.Value
		case authKeyPassword:
			password = p.Value
		}
	}

	if sessKey == "" {
		return "", "", ErrAuthPhase2MissingSessKey
	}

	if password == "" {
		return "", "", ErrAuthPhase2MissingPassword
	}

	return sessKey, password, nil
}

// encodeTTCString encodes a string with Oracle TTC length prefix.
// Short strings (< 254 bytes): 1-byte length prefix.
// Long strings (>= 254 bytes): 0xFE marker + 2-byte BE length.
func encodeTTCString(s string) []byte {
	data := []byte(s)
	if len(data) < 254 {
		return append([]byte{byte(len(data))}, data...)
	}

	buf := []byte{0xFE}
	buf = binary.BigEndian.AppendUint16(buf, uint16(len(data)))

	return append(buf, data...)
}

// encodeTTCKVPair encodes a key-value pair for TTC AUTH messages.
func encodeTTCKVPair(key, value string) []byte {
	keyBytes := encodeTTCString(key)
	valueBytes := encodeTTCString(value)
	buf := make([]byte, 0, len(keyBytes)+len(valueBytes))
	buf = append(buf, keyBytes...)
	buf = append(buf, valueBytes...)

	return buf
}

// parseTTCKVPairs parses TTC key-value pairs from a payload.
// This is a best-effort parser — TTC encoding is complex and client-specific.
func parseTTCKVPairs(payload []byte) []authKVPair {
	var pairs []authKVPair
	offset := 0

	// Skip initial bytes (logon mode, flags, etc.) until we find key-value pairs.
	// The format varies, but we look for well-known key names.
	for offset < len(payload)-2 {
		// Try to read a length-prefixed string
		key, newOffset := readTTCString(payload, offset)
		if key == "" || newOffset <= offset {
			offset++

			continue
		}

		// Check if this looks like a known AUTH key
		upperKey := strings.ToUpper(key)
		if !isAuthKey(upperKey) {
			offset++

			continue
		}

		// Read the value
		value, valueOffset := readTTCString(payload, newOffset)
		if valueOffset <= newOffset {
			offset++

			continue
		}

		pairs = append(pairs, authKVPair{Key: upperKey, Value: value})
		offset = valueOffset
	}

	return pairs
}

// readTTCString reads a TTC length-prefixed string from the payload at the given offset.
// Returns the string and the new offset after reading.
func readTTCString(payload []byte, offset int) (string, int) {
	if offset >= len(payload) {
		return "", offset
	}

	strLen := int(payload[offset])
	offset++

	if strLen == 0 {
		return "", offset
	}

	// Long string marker
	if strLen == 0xFE {
		if offset+2 > len(payload) {
			return "", offset
		}

		strLen = int(binary.BigEndian.Uint16(payload[offset : offset+2]))
		offset += 2
	}

	if offset+strLen > len(payload) {
		return "", offset
	}

	s := string(payload[offset : offset+strLen])

	return s, offset + strLen
}

// isAuthKey checks if a key name is a known Oracle AUTH key.
func isAuthKey(key string) bool {
	switch key {
	case "AUTH_TERMINAL", "AUTH_PROGRAM_NM", "AUTH_MACHINE", "AUTH_PID",
		"AUTH_SID", "AUTH_SESSKEY", "AUTH_VFR_DATA", "AUTH_PASSWORD",
		"AUTH_ACL", "AUTH_ALTER_SESSION", "AUTH_LOGON_AS_SYSDBA",
		"AUTH_LOGON_AS_SYSOPER", "AUTH_INITIAL_CLIENT_ROLE",
		"AUTH_SVR_RESPONSE", "AUTH_VERSION_NO", "AUTH_STATUS":
		return true
	default:
		return strings.HasPrefix(key, "AUTH_")
	}
}

// encodeVarUint encodes an unsigned integer in Oracle's variable-length format.
func encodeVarUint(v uint32) []byte {
	if v < 254 {
		return []byte{byte(v)}
	}

	buf := []byte{0xFE}
	buf = binary.BigEndian.AppendUint32(buf, v)

	return buf
}
