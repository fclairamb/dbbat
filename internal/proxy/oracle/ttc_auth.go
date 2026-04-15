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
	buf := make([]byte, 0, 256)
	buf = append(buf, 0x00, 0x00, byte(TTCFuncResponse)) // data flags + 0x08
	buf = append(buf, ttcCompressedUint(2)...)           // 2 KV pairs
	buf = append(buf, ttcKeyVal(authKeySessKey, encServerSessKey, 1)...)
	buf = append(buf, ttcKeyVal(authKeyVfrData, authVfrData, o5LogonVerifierTypeNum)...)

	return buf
}

// buildAuthChallengeEndMarker builds the end-of-response (message code 4) that
// must follow the AUTH challenge. Clients read message codes in a loop and exit
// on code 4. This is sent as part of the same TNS Data packet or as a separate one.
// The bytes after code 4 are a minimal Summary structure captured from Oracle 19c.
func buildAuthChallengeEndMarker() []byte {
	// Message code 4 + minimal summary fields that NewSummary reads.
	// These must align with the TTC capabilities negotiated in Set Protocol.
	// Captured from the AUTH challenge tail of Oracle 19c.
	// Message code 4 (end of call) + minimal Summary structure.
	// All fields are zero (compressed int = 0x00), which is safe for any TTC version/capability combo.
	// We send 64 zero bytes after code 4 to ensure NewSummary has enough data regardless
	// of which conditional fields are enabled.
	buf := make([]byte, 256)
	buf[0] = 0x04 // message code 4
	// Remaining 255 zero bytes provide enough data for NewSummary to read
	// all conditional fields (RetCode, CurRowNumber, Flags, error messages, etc.)
	// regardless of TTC version and capability flags.

	return buf
}

// buildAuthOK constructs the TTC AUTH success response.
func buildAuthOK() []byte {
	return []byte{0x00, 0x00, byte(TTCFuncResponse), 0x00, 0x00}
}

// buildAuthFailed constructs the TTC AUTH failure response with an ORA error code.
func buildAuthFailed(oraCode int, message string) []byte {
	buf := make([]byte, 0, 3+8+len(message)+2)
	buf = append(buf, 0x00, 0x00, byte(TTCFuncResponse))
	buf = append(buf, ttcCompressedUint(uint64(oraCode))...)
	buf = append(buf, ttcClr([]byte(message))...)

	return buf
}

// ttcKeyVal encodes a key-value pair using Oracle's TTC wire format.
// Matches go-ora's PutKeyVal: PutUint(keyLen) + PutClr(key) + PutUint(valLen) + PutClr(val) + PutInt(flag).
func ttcKeyVal(key, value string, flag int) []byte {
	buf := make([]byte, 0, len(key)+len(value)+20)
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
// Uses proper TTC DLC+CLR decoding (matching go-ora's GetKeyVal format).
func parseAuthPhase2(tnsDataPayload []byte) (string, string, error) {
	if len(tnsDataPayload) < ttcDataFlagsSize+4 {
		return "", "", ErrAuthPhase2TooShort
	}

	// Skip data flags (2) + func code (0x03) + sub-op (0x73) + preamble
	// The preamble has variable-length fields before the KV pairs.
	// We scan for the first AUTH_ key using DLC-aware parsing.
	payload := tnsDataPayload[ttcDataFlagsSize+2:]

	var sessKey, password string

	// Scan through payload for AUTH_ KV pairs using DLC+CLR decoding
	pairs := scanTTCKeyValPairs(payload)

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

// scanTTCKeyValPairs scans a TTC payload for AUTH_ key-value pairs using DLC+CLR decoding.
// Format per pair: compressed_int(keyLen) + CLR(key) + compressed_int(valLen) + CLR(val) + compressed_int(flag).
func scanTTCKeyValPairs(payload []byte) []authKVPair {
	var pairs []authKVPair

	for offset := 0; offset < len(payload)-4; {
		kLen, kLenSize := readCompressedInt(payload[offset:])
		if kLenSize == 0 || kLen <= 0 || kLen > 128 || offset+kLenSize >= len(payload) {
			offset++

			continue
		}

		clrKey, clrKeySize := readCLR(payload[offset+kLenSize:])
		if clrKeySize == 0 || len(clrKey) == 0 {
			offset++

			continue
		}

		keyStr := strings.ToUpper(string(clrKey))
		if !strings.HasPrefix(keyStr, "AUTH_") {
			offset++

			continue
		}

		key, val, consumed := readKVValue(payload[offset+kLenSize+clrKeySize:])
		if consumed == 0 {
			offset++

			continue
		}

		_ = key // DLC length, not used
		pairs = append(pairs, authKVPair{Key: keyStr, Value: string(val)})
		offset += kLenSize + clrKeySize + consumed
	}

	return pairs
}

// readKVValue reads the value DLC + CLR + flag from a KV pair.
// Returns the DLC length, CLR data, and total bytes consumed.
func readKVValue(buf []byte) (int, []byte, int) {
	vLen, vLenSize := readCompressedInt(buf)
	if vLenSize == 0 || vLen < 0 {
		return 0, nil, 0
	}

	clrVal, clrValSize := readCLR(buf[vLenSize:])
	if clrValSize == 0 {
		return 0, nil, 0
	}

	flagOffset := vLenSize + clrValSize
	_, flagSize := readCompressedInt(buf[flagOffset:])

	total := flagOffset + flagSize
	if total == flagOffset {
		total++
	}

	return vLen, clrVal, total
}

// readCompressedInt reads a TTC compressed integer from the buffer.
// Returns the value and the number of bytes consumed.
func readCompressedInt(buf []byte) (int, int) {
	if len(buf) == 0 {
		return 0, 0
	}

	size := int(buf[0])
	if size == 0 {
		return 0, 1
	}

	if size > 8 || 1+size > len(buf) {
		return 0, 0
	}

	val := 0
	for i := 1; i <= size; i++ {
		val = val<<8 | int(buf[i])
	}

	return val, 1 + size
}

// readCLR reads TTC CLR-encoded data from the buffer.
// Returns the data and total bytes consumed (including length prefix).
func readCLR(buf []byte) ([]byte, int) {
	if len(buf) == 0 {
		return nil, 0
	}

	first := buf[0]
	if first == 0 || first == 0xFF || first == 0xFD {
		return nil, 1
	}

	if first == 0xFE {
		// Chunked: 0xFE + (chunkLen + chunk)* + 0x00
		var data []byte

		offset := 1
		for offset < len(buf) {
			chunkLen := int(buf[offset])
			offset++

			if chunkLen == 0 {
				break
			}

			if offset+chunkLen > len(buf) {
				return nil, 0
			}

			data = append(data, buf[offset:offset+chunkLen]...)
			offset += chunkLen
		}

		return data, offset
	}

	// Short form: 1-byte length + data
	dataLen := int(first)
	if 1+dataLen > len(buf) {
		return nil, 0
	}

	return buf[1 : 1+dataLen], 1 + dataLen
}

// encodeTTCString encodes a string with Oracle TTC length prefix.
// Short strings (< 254 bytes): 1-byte length prefix.
// Long strings (>= 254 bytes): 0xFE marker + 2-byte BE length.

// encodeTTCKVPair encodes a key-value pair for TTC AUTH messages.

// parseTTCKVPairs parses TTC key-value pairs from a payload.
// This is a best-effort parser — TTC encoding is complex and client-specific.

// readTTCString reads a TTC length-prefixed string from the payload at the given offset.
// Returns the string and the new offset after reading.

// isAuthKey checks if a key name is a known Oracle AUTH key.
