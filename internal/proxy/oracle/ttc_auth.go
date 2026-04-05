package oracle

import (
	"encoding/binary"
	"strings"
)

// TTC AUTH key names used in Oracle authentication messages.
const (
	authKeyUsername = "AUTH_TERMINAL" //nolint:unused // used when O5LOGON terminated auth is activated
	authKeySessKey  = "AUTH_SESSKEY"  //nolint:unused // used when O5LOGON terminated auth is activated
	authKeyVfrData  = "AUTH_VFR_DATA" //nolint:unused // used when O5LOGON terminated auth is activated
	authKeyPassword = "AUTH_PASSWORD" //nolint:unused // used when O5LOGON terminated auth is activated
)

// authKVPair represents a key-value pair in a TTC AUTH message.
type authKVPair struct { //nolint:unused // used when O5LOGON terminated auth is activated
	Key   string
	Value string
}

// parseAuthPhase1 extracts the username from a TTC AUTH Phase 1 message.
// AUTH Phase 1 is sent as func=0x03, sub=0x76 (PiggybackSubAuth1).
// The message contains the username and a logon mode.
//
// Wire format (after TNS Data flags):
//
//	[0] = 0x03 (piggyback)
//	[1] = 0x76 (AUTH Phase 1 sub-op)
//	[2] = username length (1 byte for short strings)
//	[3..] = username bytes
//	... followed by logon mode and key-value pairs
func parseAuthPhase1(tnsDataPayload []byte) (string, error) { //nolint:unused // used when O5LOGON terminated auth is activated
	if len(tnsDataPayload) < ttcDataFlagsSize+3 {
		return "", ErrAuthPhase1TooShort
	}

	// Skip data flags (2 bytes) + func code (0x03) + sub-op (0x76)
	offset := ttcDataFlagsSize + 2

	if offset >= len(tnsDataPayload) {
		return "", ErrAuthPhase1NoData
	}

	// Read username length
	usernameLen := int(tnsDataPayload[offset])
	offset++

	if usernameLen == 0 || offset+usernameLen > len(tnsDataPayload) {
		return "", ErrAuthPhase1BadUsername
	}

	username := string(tnsDataPayload[offset : offset+usernameLen])

	return strings.ToUpper(username), nil
}

// buildAuthChallenge constructs the TTC AUTH challenge response.
// This is sent by the server after AUTH Phase 1 to challenge the client.
//
// Wire format (TNS Data payload):
//
//	[0-1] = 0x0000 (data flags)
//	[2]   = 0x08 (TTC Response func code)
//	[3..] = key-value pairs: AUTH_SESSKEY, AUTH_VFR_DATA
//
// The key-value pairs use Oracle's TTC encoding:
//   - Each key: length-prefixed string
//   - Each value: length-prefixed string
//   - Pairs are structured as a counted list
func buildAuthChallenge(encServerSessKey, authVfrData string) []byte { //nolint:unused // used when O5LOGON terminated auth is activated
	pairs := []authKVPair{
		{Key: authKeySessKey, Value: encServerSessKey},
		{Key: authKeyVfrData, Value: authVfrData},
	}

	return buildTTCAuthResponse(pairs, 0, "")
}

// buildAuthOK constructs the TTC AUTH success response.
func buildAuthOK() []byte { //nolint:unused // used when O5LOGON terminated auth is activated
	return buildTTCAuthResponse(nil, 0, "")
}

// buildAuthFailed constructs the TTC AUTH failure response with an ORA error code.
func buildAuthFailed(oraCode int, message string) []byte { //nolint:unused // used when O5LOGON terminated auth is activated
	return buildTTCAuthResponse(nil, oraCode, message)
}

// buildTTCAuthResponse constructs a TTC Response message with optional key-value pairs and error.
// This is the server's response format for AUTH exchanges.
//
// The TTC Response (func=0x08) format:
//
//	[0-1] data flags (0x0000)
//	[2]   func code (0x08 = Response)
//	[3]   sequence number
//	[4-5] return code (0 = success, error code otherwise)
//	[6..] key-value count + pairs (if any)
//	      or error message (if error)
func buildTTCAuthResponse(pairs []authKVPair, errCode int, errMsg string) []byte { //nolint:unused // used when O5LOGON terminated auth is activated
	// Start with data flags + response function code
	buf := []byte{
		0x00, 0x00, // data flags
		byte(TTCFuncResponse), // 0x08
		0x00,                  // sequence
	}

	if errCode != 0 {
		// Error response: return code + error message
		buf = append(buf, encodeVarUint(uint32(errCode))...)
		buf = append(buf, encodeTTCString(errMsg)...)

		return buf
	}

	// Success: return code = 0
	buf = append(buf, 0x00, 0x00) // return code = 0

	if len(pairs) == 0 {
		// No key-value pairs (AUTH OK with no data)
		buf = append(buf, 0x00) // pair count = 0

		return buf
	}

	// Key-value pairs count
	buf = append(buf, byte(len(pairs)))

	for _, p := range pairs {
		buf = append(buf, encodeTTCKVPair(p.Key, p.Value)...)
	}

	return buf
}

// parseAuthPhase2 extracts the client session key and encrypted password from AUTH Phase 2.
// AUTH Phase 2 is sent as func=0x03, sub=0x73 (PiggybackSubAuth2).
//
// The message contains key-value pairs including:
//   - AUTH_SESSKEY: encrypted client session key (hex string)
//   - AUTH_PASSWORD: encrypted password (hex string)
func parseAuthPhase2(tnsDataPayload []byte) (string, string, error) { //nolint:unused // used when O5LOGON terminated auth is activated
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
func encodeTTCString(s string) []byte { //nolint:unused // used when O5LOGON terminated auth is activated
	data := []byte(s)
	if len(data) < 254 {
		return append([]byte{byte(len(data))}, data...)
	}

	buf := []byte{0xFE}
	buf = binary.BigEndian.AppendUint16(buf, uint16(len(data)))

	return append(buf, data...)
}

// encodeTTCKVPair encodes a key-value pair for TTC AUTH messages.
func encodeTTCKVPair(key, value string) []byte { //nolint:unused // used when O5LOGON terminated auth is activated
	keyBytes := encodeTTCString(key)
	valueBytes := encodeTTCString(value)
	buf := make([]byte, 0, len(keyBytes)+len(valueBytes))
	buf = append(buf, keyBytes...)
	buf = append(buf, valueBytes...)

	return buf
}

// parseTTCKVPairs parses TTC key-value pairs from a payload.
// This is a best-effort parser — TTC encoding is complex and client-specific.
func parseTTCKVPairs(payload []byte) []authKVPair { //nolint:unused // used when O5LOGON terminated auth is activated
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
func readTTCString(payload []byte, offset int) (string, int) { //nolint:unused // used when O5LOGON terminated auth is activated
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
func isAuthKey(key string) bool { //nolint:unused // used when O5LOGON terminated auth is activated
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
func encodeVarUint(v uint32) []byte { //nolint:unused // used when O5LOGON terminated auth is activated
	if v < 254 {
		return []byte{byte(v)}
	}

	buf := []byte{0xFE}
	buf = binary.BigEndian.AppendUint32(buf, v)

	return buf
}
