package oracle

import (
	"bytes"
	"encoding/binary"
	"strconv"
	"strings"
)

// TTC AUTH key names used in Oracle authentication messages.
const (
	authKeyUsername           = "AUTH_TERMINAL"
	authKeySessKey            = "AUTH_SESSKEY"
	authKeyVfrData            = "AUTH_VFR_DATA"
	authKeyPassword           = "AUTH_PASSWORD"
	authKeyPbkdf2CskSalt      = "AUTH_PBKDF2_CSK_SALT"
	authKeyPbkdf2VgenCount    = "AUTH_PBKDF2_VGEN_COUNT"
	authKeyPbkdf2SderCount    = "AUTH_PBKDF2_SDER_COUNT"
	authKeyGloballyUniqueDBID = "AUTH_GLOBALLY_UNIQUE_DBID"
)

// dbbatGloballyUniqueDBID is the synthetic AUTH_GLOBALLY_UNIQUE_DBID value dbbat
// returns in the 12c/customHash challenge. It is DB metadata the client records
// but does not use in O5LOGON key derivation, so a fixed 32-hex-char value is
// sufficient for terminated authentication.
const dbbatGloballyUniqueDBID = "DBBA700000000000DBBA700000000000"

// authKVPair represents a key-value pair in a TTC AUTH message.
type authKVPair struct {
	Key   string
	Value string
}

// knownAuthKeys are TTC AUTH key names that must not be returned as the username.
// Different clients serialize the username at different offsets; the parser skips
// any match on these well-known keys and keeps scanning.
var knownAuthKeys = map[string]bool{
	authKeyUsername:           true, // "AUTH_TERMINAL"
	authKeySessKey:            true, // "AUTH_SESSKEY"
	authKeyVfrData:            true, // "AUTH_VFR_DATA"
	authKeyPassword:           true, // "AUTH_PASSWORD"
	"AUTH_PROGRAM_NM":         true,
	"AUTH_MACHINE":            true,
	"AUTH_PID":                true,
	"AUTH_SID":                true,
	"AUTH_ACL":                true,
	"AUTH_ALTER_SESSION":      true,
	"AUTH_LOGICAL_SESSION_ID": true,
}

// parseAuthPhase1 extracts the username from a TTC AUTH Phase 1 message.
// AUTH Phase 1 is sent as func=0x03, sub=0x76 (PiggybackSubAuth1).
//
// Wire layout shared across go-ora, python-oracledb thin, and JDBC thin / SQLcl:
//
//	[03 76 b0 b1]                            -- piggyback header (b0/b1 vary by client)
//	[compressed-int user_id_len]             -- length of the username
//	[compressed-int logon_mode]              -- typically 0x01 (NoNewPass)
//	[01 01 05 01 01]                         -- 5-byte magic
//	[user_id_len bytes username]             -- bare username (SQLcl)
//	  OR
//	[1-byte CLR length, user_id_len bytes]   -- CLR-encoded username (go-ora)
//	[KV pairs ...]
//
// We parse the structured fields to extract user_id_len, then accept the
// next user_id_len printable-ASCII bytes as the username. If the first byte
// after the magic equals user_id_len we treat it as a CLR length prefix and
// skip it (the go-ora style); otherwise we read the bytes directly (SQLcl).
func parseAuthPhase1(tnsDataPayload []byte) (string, error) {
	if len(tnsDataPayload) < ttcDataFlagsSize+4 {
		return "", ErrAuthPhase1TooShort
	}

	// Primary path: every O5LOGON Phase 1 carries the username as a
	// length-prefixed identifier immediately before the AUTH_* key/value pairs,
	// regardless of the per-client preamble bytes (which vary: go-ora, SQLcl,
	// and python-oracledb thin classic vs fast-auth all differ by a byte or
	// two). Anchoring on the AUTH_ keys is robust where fixed offsets are not.
	if name, ok := usernameBeforeAuthKV(tnsDataPayload); ok {
		return name, nil
	}

	// Skip data flags (2 bytes) + func code (0x03) + sub-op (0x76) + 2-byte trailer.
	if len(tnsDataPayload) < ttcDataFlagsSize+4 {
		return "", ErrAuthPhase1TooShort
	}

	payload := tnsDataPayload[ttcDataFlagsSize+4:]

	// Read user_id_len (compressed-int).
	userIDLen, n := readCompressedInt(payload)
	if n == 0 || userIDLen <= 0 || userIDLen > 128 {
		return parseAuthPhase1Fallback(tnsDataPayload[ttcDataFlagsSize+2:])
	}

	payload = payload[n:]

	// Skip logon_mode (compressed-int, ignored).
	_, n = readCompressedInt(payload)
	if n == 0 {
		return parseAuthPhase1Fallback(tnsDataPayload[ttcDataFlagsSize+2:])
	}

	payload = payload[n:]

	// Skip the 5-byte magic.
	const magicLen = 5
	if len(payload) < magicLen {
		return parseAuthPhase1Fallback(tnsDataPayload[ttcDataFlagsSize+2:])
	}

	payload = payload[magicLen:]

	if name, ok := readUsernameAtOffset(payload, userIDLen); ok {
		return name, nil
	}

	// Some clients prefix the username with its CLR length again; tolerate.
	if len(payload) > 0 && int(payload[0]) == userIDLen {
		if name, ok := readUsernameAtOffset(payload[1:], userIDLen); ok {
			return name, nil
		}
	}

	return parseAuthPhase1Fallback(tnsDataPayload[ttcDataFlagsSize+2:])
}

// usernameBeforeAuthKV extracts the login username by anchoring on the AUTH_*
// key/value section that follows it in every O5LOGON Phase 1 message. It locates
// the first "AUTH_" key and walks back a short window for the length-prefixed
// identifier (the username) that sits just ahead of the KV pairs — tolerating
// the small, client-specific framing bytes between them. This is preamble-layout
// independent, so it handles go-ora, SQLcl/JDBC, and python-oracledb thin
// (classic and fast-auth) uniformly.
func usernameBeforeAuthKV(payload []byte) (string, bool) {
	authIdx := bytes.Index(payload, []byte("AUTH_"))
	if authIdx < 2 {
		return "", false
	}

	const (
		maxFramingGap = 8  // KV-count/key-length framing bytes between username and AUTH_
		maxUserLen    = 30 // longest plausible username
	)

	// The username is the run of identifier bytes ending just before the first
	// AUTH_ key, separated only by a few non-identifier framing bytes (the KV
	// pair count and key-length prefixes). This works whether the username is
	// length-prefixed (go-ora, python-oracledb thin) or bare (SQLcl/JDBC thin) —
	// the preceding length byte (<32 for any real username) is a control byte and
	// so is not part of the identifier run.
	end := authIdx

	for gap := 0; end > 0 && !isIdentifierByte(payload[end-1]); gap++ {
		if gap >= maxFramingGap {
			return "", false
		}

		end--
	}

	start := end
	for start > 0 && isIdentifierByte(payload[start-1]) && end-start < maxUserLen {
		start--
	}

	if start == end {
		return "", false
	}

	name := strings.ToUpper(string(payload[start:end]))
	if knownAuthKeys[name] {
		return "", false
	}

	return name, true
}

// isIdentifierByte reports whether b is an Oracle identifier character.
func isIdentifierByte(b byte) bool {
	switch {
	case b >= 'A' && b <= 'Z', b >= 'a' && b <= 'z', b >= '0' && b <= '9':
		return true
	case b == '_' || b == '$' || b == '#':
		return true
	default:
		return false
	}
}

// readUsernameAtOffset returns the next userIDLen bytes as a username if they
// are all printable ASCII identifier characters; ok=false otherwise.
func readUsernameAtOffset(payload []byte, userIDLen int) (string, bool) {
	if userIDLen <= 0 || userIDLen > len(payload) {
		return "", false
	}

	candidate := payload[:userIDLen]
	if !isPrintableASCII(candidate) {
		return "", false
	}

	name := strings.ToUpper(string(candidate))
	if knownAuthKeys[name] {
		return "", false
	}

	return name, true
}

// parseAuthPhase1Fallback retains the original heuristic scan for clients
// whose Phase 1 layout deviates from the documented one (sqlplus's
// direct-length-prefix format and unusual proxy_auth modes). Unchanged from
// the previous implementation.
func parseAuthPhase1Fallback(payload []byte) (string, error) {
	for i := 0; i+4 < len(payload); i++ {
		if payload[i] != 0x01 || payload[i+2] != 0x01 || payload[i+3] != 0x01 {
			continue
		}

		strLen := int(payload[i+1])
		if strLen < 2 || strLen > 32 || i+4+strLen > len(payload) {
			continue
		}

		candidate := payload[i+4 : i+4+strLen]
		if !isPrintableASCII(candidate) {
			continue
		}

		name := strings.ToUpper(string(candidate))
		if !knownAuthKeys[name] {
			return name, nil
		}
	}

	for i := 0; i < len(payload)-2; i++ {
		strLen := int(payload[i])
		if strLen < 2 || strLen > 128 || i+1+strLen > len(payload) {
			continue
		}

		candidate := payload[i+1 : i+1+strLen]
		if !isPrintableASCII(candidate) {
			continue
		}

		name := strings.ToUpper(string(candidate))
		if knownAuthKeys[name] {
			continue
		}

		return name, nil
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
// In customHash mode (pbkdf2ChkSaltHex non-empty), three extra KV pairs are
// included: AUTH_PBKDF2_CSK_SALT plus AUTH_PBKDF2_VGEN_COUNT and
// AUTH_PBKDF2_SDER_COUNT. Together they tell the client to derive the
// combined key via PBKDF2 (matching what real Oracle 19c sends when
// caps[4]&0x20 is set in the Set Protocol response).
func buildAuthChallenge(encServerSessKey, authVfrData, pbkdf2ChkSaltHex string, vgenCount, sderCount, verifierType int, wide bool) []byte {
	pairs := 2

	if pbkdf2ChkSaltHex != "" {
		// AUTH_PBKDF2_CSK_SALT / VGEN_COUNT / SDER_COUNT + AUTH_GLOBALLY_UNIQUE_DBID.
		pairs += 4
	}

	// AUTH_SESSKEY carries flag 1 in the legacy 19c/6949 challenge but flag 0 in
	// the modern 12c/18453 challenge; modern thin clients are strict about it.
	sessKeyFlag := 1
	if verifierType == VerifierType18453 {
		sessKeyFlag = 0
	}

	// OCI clients (sqlplus / instant client) negotiate fixed 4-byte little-endian
	// key/value lengths and flags; thin clients (go-ora, python-oracledb thin,
	// JDBC thin) use the compressed length-prefixed form. Encode the challenge to
	// match the client, observed from its AUTH Phase 1 (see payloadUsesWideKVEncoding).
	kv := ttcKeyVal
	if wide {
		kv = ttcKeyValWide
	}

	buf := make([]byte, 0, 256)

	if wide {
		buf = append(buf, 0x20, 0x00, byte(TTCFuncResponse)) // OCI data flags + 0x08
		buf = binary.LittleEndian.AppendUint16(buf, uint16(pairs))
	} else {
		buf = append(buf, 0x00, 0x00, byte(TTCFuncResponse)) // data flags + 0x08
		buf = append(buf, ttcCompressedUint(uint64(pairs))...)
	}

	buf = append(buf, kv(authKeySessKey, encServerSessKey, sessKeyFlag)...)
	buf = append(buf, kv(authKeyVfrData, authVfrData, verifierType)...)

	if pbkdf2ChkSaltHex != "" {
		buf = append(buf, kv(authKeyPbkdf2CskSalt, pbkdf2ChkSaltHex, 0)...)
		buf = append(buf, kv(authKeyPbkdf2VgenCount, strconv.Itoa(vgenCount), 0)...)
		buf = append(buf, kv(authKeyPbkdf2SderCount, strconv.Itoa(sderCount), 0)...)
		// Modern thin clients (python-oracledb thin, JDBC thin) expect the
		// server's globally-unique DB id in the 12c/customHash challenge. Real
		// Oracle 23ai encodes this key with a trailing NUL ("AUTH_GLOBALLY_
		// UNIQUE_DBID\0"); python-oracledb thin requires that exact key framing or
		// it stalls. The value is DB metadata (not used in O5LOGON key
		// derivation), so a stable synthetic value suffices for terminated auth.
		buf = append(buf, kv(authKeyGloballyUniqueDBID+"\x00", dbbatGloballyUniqueDBID, 0)...)
	}

	return buf
}

// ttcKeyValWide encodes a TTC key/value pair with fixed 4-byte little-endian
// lengths and flag — the form OCI (sqlplus / instant client) negotiates. The CLR
// bodies keep the 1-byte length prefix. Mirror of ttcKeyVal (compressed form).
func ttcKeyValWide(key, value string, flag int) []byte {
	keyBytes := []byte(key)
	valBytes := []byte(value)

	buf := make([]byte, 0, len(key)+len(value)+20)
	buf = binary.LittleEndian.AppendUint32(buf, uint32(len(keyBytes)))
	buf = append(buf, ttcClr(keyBytes)...)
	buf = binary.LittleEndian.AppendUint32(buf, uint32(len(valBytes)))
	buf = append(buf, ttcClr(valBytes)...)
	buf = binary.LittleEndian.AppendUint32(buf, uint32(flag))

	return buf
}

// payloadUsesWideKVEncoding reports whether a TTC AUTH message encodes its
// key/value lengths as fixed 4-byte little-endian integers (OCI / sqlplus)
// rather than the compressed length-prefixed form (go-ora, python-oracledb thin,
// JDBC thin). It scans every AUTH_ key: in wide mode a 4-byte keyLen (e.g.
// 0d 00 00 00 for a 13-byte key) precedes the 1-byte CLR length, leaving three
// zero bytes the compressed form (01 0d) never produces. Scanning all keys (not
// just the first) is robust to clients whose first AUTH_ occurrence sits behind
// a preamble the fixed offset would misread.
func payloadUsesWideKVEncoding(payload []byte) bool {
	for off := 0; off < len(payload); {
		rel := bytes.Index(payload[off:], []byte("AUTH_"))
		if rel < 0 {
			return false
		}

		i := off + rel
		off = i + 5

		if i < 5 {
			continue
		}

		// keyLen is the length of the identifier run that starts here (the full
		// AUTH_ key name, e.g. AUTH_TERMINAL = 13). In wide mode this key is
		// framed [keyLen:4 LE][CLR-len:1][key]; the CLR length equals the key
		// length and the 4-byte field's high three bytes are zero — a triple zero
		// the compressed form (01 0d) never produces. (The low byte of the 4-byte
		// field is not always the key length: some clients put a buffer size there
		// for value-bearing keys like a non-empty AUTH_TERMINAL.)
		j := i
		for j < len(payload) && isIdentifierByte(payload[j]) {
			j++
		}

		keyLen := j - i
		if keyLen >= 5 && int(payload[i-1]) == keyLen &&
			payload[i-2] == 0 && payload[i-3] == 0 && payload[i-4] == 0 {
			return true
		}
	}

	return false
}

// buildAuthChallengeEndMarker builds the end-of-response (message code 4)
// that must follow the AUTH challenge. The Summary structure is captured from
// a real Oracle 19c Phase 1 challenge response — exactly 32 bytes after the
// 0x04 code byte (33 bytes total).
//
// Earlier versions emitted 256 zero-padded bytes "to be safe", but JDBC thin
// (SQLcl 26.1+) parses a fixed-size Summary off the wire and leaves the
// remaining bytes in its read buffer. On the next round-trip those leftover
// 0x00 bytes are read as TTC message codes — code 0 isn't a valid case in
// T4CTTIfun.receive's switch, so JDBC throws ORA-17401 with no useful
// stack location below T4CTTIfun.receive:1048. Matching Oracle's exact
// 33-byte tail keeps JDBC's buffer empty after the challenge.
//
// go-ora and python-oracledb thin happen to consume more bytes from the
// trailing zeros (their parsers are tolerant) so they survived this bug
// for ~2 years. The fix is also a cleanup — sending fewer bytes per challenge.
func buildAuthChallengeEndMarker(verifierType int, wide bool) []byte {
	// OCI (sqlplus) parses a fixed-width end-of-call Summary off the wire; the
	// 4-byte-encoded 153-byte form below is captured byte-for-byte from a real
	// Oracle 23ai classic 18453 challenge to an OCI client. Three bytes are
	// connection-specific server state (a call sequence at [5] and an SCN-like
	// value at [73:75]) that OCI records but does not validate, so fixed values
	// from the capture are sufficient for terminated auth.
	if wide {
		return []byte{
			0x04, 0x01, 0x00, 0x00, 0x00, 0x23, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
			0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
			0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
			0x02, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x36, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
			0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0xb0, 0xc4, 0x06, 0xb5, 0xff, 0xff, 0x00, 0x00,
			0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
			0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
			0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
			0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
			0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x1d,
		}
	}

	// The end-of-call Summary differs between the legacy 19c/6949 challenge and
	// the modern 12c/18453 challenge; modern thin clients (python-oracledb thin)
	// reject a mismatched Summary and block. The 18453 form below is captured
	// byte-for-byte from a real Oracle 23ai classic 18453 challenge.
	if verifierType == VerifierType18453 {
		return []byte{
			0x04, // end-of-call code
			0x01, 0x01, 0x01, 0x3c, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
			0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
			0x00, 0x00,
		}
	}

	// Legacy 19c/6949 Summary (32 bytes after the 0x04 code).
	return []byte{
		0x04, // end-of-call code
		0x01, 0x01, 0x01, 0x02, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	}
}

// buildAuthOK constructs the TTC AUTH success response.

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

// parseAuthPhase2 extracts the client session key and (optionally) encrypted password
// from AUTH Phase 2.
// AUTH Phase 2 is sent as func=0x03, sub=0x73 (PiggybackSubAuth2).
// Uses proper TTC DLC+CLR decoding (matching go-ora's GetKeyVal format).
//
// password is empty for clients that do not send AUTH_PASSWORD content (e.g. SQLcl and
// the modern Oracle JDBC thin driver). In that mode, the password verification happens
// implicitly via the AUTH_SESSKEY exchange — the server proves password knowledge by
// returning AUTH_SVR_RESPONSE encrypted with the combined session key, and the client
// validates it locally.
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

	// Fallback: some clients encode the preamble in a way that confuses the
	// DLC+CLR state machine. Do a byte-level search for the literal key names and
	// read the value that follows. OCI (sqlplus) uses fixed 4-byte little-endian
	// value lengths; thin clients use the compressed form — pick by the message's
	// own encoding (see payloadUsesWideKVEncoding).
	wide := payloadUsesWideKVEncoding(payload)

	findVal := findKVByKeyBytes
	if wide {
		findVal = findKVByKeyBytesWide
	}

	if sessKey == "" {
		sessKey = findVal(payload, []byte(authKeySessKey))
	}

	if password == "" {
		password = findVal(payload, []byte(authKeyPassword))
	}

	if sessKey == "" {
		return "", "", ErrAuthPhase2MissingSessKey
	}

	return sessKey, password, nil
}

// findKVByKeyBytes searches payload for a TTC AUTH_* key name and returns the value
// that follows. Expected layout: <DLC(valLen)> <CLR(val)>, starting immediately after
// the key's last byte. CLR is either <len><bytes> for short values or 0xFE<chunks>
// for long values.
func findKVByKeyBytes(payload, key []byte) string {
	idx := indexOf(payload, key)
	if idx < 0 {
		return ""
	}

	// After the key name, skip DLC (valLen) compressed int.
	tail := payload[idx+len(key):]

	_, dlcSize := readCompressedInt(tail)
	if dlcSize == 0 || dlcSize >= len(tail) {
		return ""
	}

	val, _ := readCLR(tail[dlcSize:])

	return string(val)
}

// findKVByKeyBytesWide is the OCI (4-byte little-endian) counterpart of
// findKVByKeyBytes: after the key name the value length is a fixed 4-byte LE
// integer, followed by the CLR-encoded value.
func findKVByKeyBytesWide(payload, key []byte) string {
	idx := indexOf(payload, key)
	if idx < 0 {
		return ""
	}

	tail := payload[idx+len(key):]
	if len(tail) < 4 {
		return ""
	}

	val, _ := readCLR(tail[4:]) // skip 4-byte LE valLen

	return string(val)
}

// indexOf is a simple byte-slice search.
func indexOf(haystack, needle []byte) int {
	if len(needle) == 0 {
		return 0
	}

	for i := 0; i+len(needle) <= len(haystack); i++ {
		match := true
		for j := 0; j < len(needle); j++ {
			if haystack[i+j] != needle[j] {
				match = false

				break
			}
		}

		if match {
			return i
		}
	}

	return -1
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
