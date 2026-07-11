package oracle

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"

	dbbcrypto "github.com/fclairamb/dbbat/internal/crypto"
)

const (
	o5LogonSaltLength        = 10
	o5LogonVerifierKeyLength = 24     // SHA-1 (20 bytes) zero-padded to 24
	o5LogonSessionKeyLength  = 48     // 48 bytes needed for non-customHash key derivation (XOR bytes 16-39)
	o5LogonVerifierType         = "6949" // SHA-1 based verifier (legacy, used in value suffix)
	o5LogonVerifierTypeNum      = 6949   // Verifier type sent as KV pair flag
	o5LogonVerifierType18453Num = 18453  // 12c PBKDF2 verifier type flag
	o5Logon18453SessionKeyLen   = 32     // 12c session-key length
	o5LogonPasswordPrefixLen    = 16     // Random prefix prepended to password by client

	// o5LogonPbkdf2ChkSaltLen is the length of AUTH_PBKDF2_CSK_SALT in bytes
	// (32-character hex on the wire). go-ora rejects values whose hex form is
	// any other length (`network.NewOracleError(28041)`), so this is fixed.
	o5LogonPbkdf2ChkSaltLen = 16

	// Iteration counts advertised in AUTH_PBKDF2_VGEN_COUNT / SDER_COUNT.
	// Matches what real Oracle 19c sends and what go-ora's auth_object.go
	// clamps to. dbbat-as-server doesn't actually iterate VGEN itself (the
	// verifier_key is precomputed in legacy 6949 form at API key creation
	// time), but advertising 4096 keeps clients from clamping to a different
	// minimum which would mismatch our derivation if they cached the value.
	o5LogonDefaultPbkdf2VgenCount = 4096
	o5LogonDefaultPbkdf2SderCount = 3
)

// O5LogonServer implements server-side O5LOGON authentication.
// This is the mirror of the client-side implementation in go-ora.
type O5LogonServer struct {
	salt             []byte // 10-byte salt (from stored verifier)
	verifierKey      []byte // 24-byte key derived from password+salt
	serverSessionKey []byte // 48-byte random (generated per auth)

	// customHash, when true, has GenerateChallenge advertise AUTH_PBKDF2_*
	// fields and DecryptPassword switch to the PBKDF2-derived combined-key
	// path. Mirrors the customHash branch dbbat already implements on the
	// upstream-as-client side; matches what real Oracle 19c does on the
	// wire when caps[4]&0x20 is set in the Set Protocol response. Without
	// this we have to strip the bit before forwarding to the client and
	// the upstream then downgrades to verifier 6949 with no PBKDF2 fields,
	// breaking SQLcl Phase 2.
	customHash bool

	// pbkdf2ChkSalt is generated per session when customHash is true, sent
	// as AUTH_PBKDF2_CSK_SALT in the challenge, and reused in
	// DecryptPassword to derive the combined key.
	pbkdf2ChkSalt []byte

	pbkdf2VgenCount int
	pbkdf2SderCount int

	// verifier18453, when true, switches the server to the 12c PBKDF2 verifier:
	// 32-byte session keys encrypted with verifierKey18453 (AES-256, no
	// padding), AUTH_VFR_DATA=salt18453 flagged 18453, and the 18453 combined
	// key. Used for OCI thick clients (sqlplus), which resolve a customHash
	// challenge as verifier 18453. Implies customHash.
	verifier18453    bool
	salt18453        []byte
	verifierKey18453 []byte

	// CombinedKey is set by DecryptPassword to the negotiated combined key
	// — either MD5/XOR (legacy) or the PBKDF2-customHash derivation. It is
	// the AES key the client expects for AUTH_SVR_RESPONSE in the AUTH OK
	// forwarded back.
	CombinedKey []byte
}

// GenerateO5LogonVerifier creates salt + verifier key from a plaintext password.
// Called at API key creation time; the results are stored in the database.
// Delegates to the crypto package for the core computation.
func GenerateO5LogonVerifier(password string) ([]byte, []byte, error) {
	return dbbcrypto.GenerateO5LogonVerifier(password)
}

// deriveVerifierKey computes the O5LOGON verifier key from password and salt.
// Delegates to the crypto package.
func deriveVerifierKey(password string, salt []byte) []byte {
	return dbbcrypto.DeriveO5LogonVerifierKey(password, salt)
}

// NewO5LogonServer creates a server from stored verifier data.
func NewO5LogonServer(salt, verifierKey []byte) *O5LogonServer {
	return &O5LogonServer{
		salt:            salt,
		verifierKey:     verifierKey,
		pbkdf2VgenCount: o5LogonDefaultPbkdf2VgenCount,
		pbkdf2SderCount: o5LogonDefaultPbkdf2SderCount,
	}
}

// EnableCustomHash switches the server into customHash mode. GenerateChallenge
// will then advertise per-session AUTH_PBKDF2_CSK_SALT / VGEN_COUNT / SDER_COUNT
// fields and DecryptPassword will use the PBKDF2-customHash combined-key
// derivation. Used when the upstream's Set Protocol response had caps[4]&0x20
// set — keeping the bit on the client side avoids the verifier-6949 downgrade
// that real Oracle emits when the client signals "no customHash support".
func (s *O5LogonServer) EnableCustomHash() {
	s.customHash = true
}

// CustomHashEnabled reports whether the server is in customHash mode. Used by
// the challenge builder to decide whether to include AUTH_PBKDF2_* fields.
func (s *O5LogonServer) CustomHashEnabled() bool {
	return s.customHash
}

// EnableVerifier18453 switches the server to the 12c PBKDF2 verifier using the
// stored 18453 salt and key. Implies customHash.
func (s *O5LogonServer) EnableVerifier18453(salt18453, verifierKey18453 []byte) {
	s.verifier18453 = true
	s.customHash = true
	s.salt18453 = salt18453
	s.verifierKey18453 = verifierKey18453
}

// VerifierType returns the verifier type flag the challenge advertises on
// AUTH_VFR_DATA: 18453 in 12c mode, otherwise 6949.
func (s *O5LogonServer) VerifierType() int {
	if s.verifier18453 {
		return o5LogonVerifierType18453Num
	}

	return o5LogonVerifierTypeNum
}

// PBKDF2ChkSalt returns the per-session salt as an uppercase hex string.
// Empty until GenerateChallenge has run in customHash mode.
func (s *O5LogonServer) PBKDF2ChkSalt() string {
	return strings.ToUpper(hex.EncodeToString(s.pbkdf2ChkSalt))
}

// PBKDF2VgenCount returns the verifier-generation iteration count advertised
// in the challenge as AUTH_PBKDF2_VGEN_COUNT.
func (s *O5LogonServer) PBKDF2VgenCount() int { return s.pbkdf2VgenCount }

// PBKDF2SderCount returns the session-key-derivation iteration count
// advertised in the challenge as AUTH_PBKDF2_SDER_COUNT.
func (s *O5LogonServer) PBKDF2SderCount() int { return s.pbkdf2SderCount }

// GenerateChallenge produces the AUTH_SESSKEY and AUTH_VFR_DATA for the client.
// Returns hex-encoded encrypted server session key and auth verifier data.
//
// In customHash mode it also generates a fresh AUTH_PBKDF2_CSK_SALT (accessible
// via PBKDF2ChkSalt) so the caller can include the matching KV fields in the
// Phase 1 challenge sent on the wire.
func (s *O5LogonServer) GenerateChallenge() (string, string, error) {
	if s.customHash {
		s.pbkdf2ChkSalt = make([]byte, o5LogonPbkdf2ChkSaltLen)
		if _, err := rand.Read(s.pbkdf2ChkSalt); err != nil {
			return "", "", fmt.Errorf("failed to generate AUTH_PBKDF2_CSK_SALT: %w", err)
		}
	}

	if s.verifier18453 {
		return s.generateChallenge18453()
	}

	// Generate random server session key
	s.serverSessionKey = make([]byte, o5LogonSessionKeyLength)
	if _, err := rand.Read(s.serverSessionKey); err != nil {
		return "", "", fmt.Errorf("failed to generate server session key: %w", err)
	}

	// Derive encryption key from verifier key
	encKey := deriveAESKey(s.verifierKey)

	// Encrypt server session key with AES-192-CBC (truncated to original length)
	encrypted, err := aes192CBCEncryptTruncated(encKey, s.serverSessionKey)
	if err != nil {
		return "", "", fmt.Errorf("failed to encrypt server session key: %w", err)
	}

	encSessKey := strings.ToUpper(hex.EncodeToString(encrypted))

	// AUTH_VFR_DATA = hex(salt). The verifier type (6949) is sent as the
	// KV pair flag, not as a value suffix (go-ora reads VerifierType from the flag).
	vfrData := strings.ToUpper(hex.EncodeToString(s.salt))

	return encSessKey, vfrData, nil
}

// generateChallenge18453 produces the 12c-verifier challenge: a 32-byte server
// session key encrypted with the 18453 key (AES-256-CBC, zero IV, no padding),
// and AUTH_VFR_DATA = hex(salt18453).
func (s *O5LogonServer) generateChallenge18453() (string, string, error) {
	s.serverSessionKey = make([]byte, o5Logon18453SessionKeyLen)
	if _, err := rand.Read(s.serverSessionKey); err != nil {
		return "", "", fmt.Errorf("failed to generate server session key: %w", err)
	}

	encrypted, err := aesCBCEncryptZeroIV(s.verifierKey18453, s.serverSessionKey, false)
	if err != nil {
		return "", "", fmt.Errorf("failed to encrypt server session key (18453): %w", err)
	}

	encSessKey := strings.ToUpper(hex.EncodeToString(encrypted))
	vfrData := strings.ToUpper(hex.EncodeToString(s.salt18453))

	return encSessKey, vfrData, nil
}

// DecryptPassword extracts the plaintext password from the client's AUTH Phase 2.
// The client encrypts: random_prefix(16 bytes) + password using AES-192-CBC
// with a combined key derived from both session keys.
//
// In customHash mode the server attempts both the customHash combined-key
// derivation (matching go-ora and JDBC, where the server-advertised customHash
// bit and the AUTH_PBKDF2_* fields are honored) and the legacy MD5/XOR path
// (matching python-oracledb thin, which always picks legacy when the
// session_key is 48 bytes regardless of caps[4]&0x20). The first candidate
// that yields a printable plaintext password wins. Out of customHash mode,
// only the legacy path is tried.
//
// Side-effect: on success, the negotiated combined key is stored on the
// receiver as CombinedKey so callers can reuse it (e.g. to encrypt
// AUTH_SVR_RESPONSE in the AUTH OK payload forwarded to the client).
func (s *O5LogonServer) DecryptPassword(encClientSessKey, encPassword string) (string, error) {
	// Decode client session key
	encClientSessKeyBytes, err := hex.DecodeString(encClientSessKey)
	if err != nil {
		return "", fmt.Errorf("failed to decode client session key: %w", err)
	}

	// Decrypt client session key using the verifier key. 18453 uses the 32-byte
	// key (AES-256, no padding); 6949 uses the 24-byte key (AES-192).
	var clientSessionKey []byte

	if s.verifier18453 {
		clientSessionKey, err = aesCBCDecryptZeroIV(s.verifierKey18453, encClientSessKeyBytes, false)
	} else {
		clientSessionKey, err = aes192CBCDecrypt(deriveAESKey(s.verifierKey), encClientSessKeyBytes)
	}

	if err != nil {
		return "", fmt.Errorf("failed to decrypt client session key: %w", err)
	}

	encPasswordBytes, err := hex.DecodeString(encPassword)
	if err != nil {
		return "", fmt.Errorf("failed to decode encrypted password: %w", err)
	}

	candidates := s.combinedKeyCandidates(clientSessionKey)

	for _, key := range candidates {
		password, ok := tryDecryptPasswordWithKey(key, encPasswordBytes)
		if !ok {
			continue
		}

		s.CombinedKey = key

		return password, nil
	}

	return "", ErrDecryptedPasswordTooShort
}

// DeriveCombinedKey computes the combined-key the client used (the first
// candidate per combinedKeyCandidates) and stores it on the receiver, without
// requiring an AUTH_PASSWORD plaintext to verify against. Used by the
// empty-AUTH_PASSWORD branch (SQLcl / JDBC thin 23c+) where the proxy has no
// way to validate by password decryption — but still needs the combined key
// so it can re-encrypt AUTH_SVR_RESPONSE before forwarding the upstream's
// AUTH OK to the client.
//
// In customHash mode the customHash PBKDF2 derivation is preferred (matches
// JDBC and go-ora). Otherwise the legacy MD5/XOR combined key is used.
func (s *O5LogonServer) DeriveCombinedKey(encClientSessKey string) error {
	encClientSessKeyBytes, err := hex.DecodeString(encClientSessKey)
	if err != nil {
		return fmt.Errorf("decode client session key: %w", err)
	}

	decKey := deriveAESKey(s.verifierKey)

	clientSessionKey, err := aes192CBCDecrypt(decKey, encClientSessKeyBytes)
	if err != nil {
		return fmt.Errorf("decrypt client session key: %w", err)
	}

	candidates := s.combinedKeyCandidates(clientSessionKey)
	if len(candidates) == 0 {
		return ErrNoCombinedKeyCandidate
	}

	s.CombinedKey = candidates[0]

	return nil
}

// combinedKeyCandidates returns the combined-key derivations to try, in order
// of likelihood for the current mode.
//
//   - customHash mode: customHash PBKDF2 derivation first (go-ora, JDBC), then
//     legacy MD5/XOR (python-oracledb thin's verifier-6949 path is hard-coded
//     to legacy regardless of customHash advertisement).
//   - non-customHash mode: legacy only.
func (s *O5LogonServer) combinedKeyCandidates(clientSessionKey []byte) [][]byte {
	var candidates [][]byte

	if s.customHash {
		// customHash 18453 (12c) derivation: HMAC-SHA512 over hex(clientKey ||
		// serverKey) keyed with pbkdf2ChkSalt, using the full keys — matches
		// go-ora's generatePasswordEncKey verifier-18453 branch. OCI thick
		// clients drive this with 32-byte session keys.
		if k := s.customHashCombinedKey(clientSessionKey, len(clientSessionKey)); k != nil {
			candidates = append(candidates, k)
		}

		// customHash 6949 derivation: same HMAC but over the first 24 bytes of
		// each key (go-ora / JDBC 6949 branch).
		if k := s.customHashCombinedKey(clientSessionKey, 24); k != nil {
			candidates = append(candidates, k)
		}
	}

	// Legacy MD5/XOR path (python-oracledb thin's 6949 route, and non-customHash).
	if legacy := deriveCombinedKey(s.serverSessionKey, clientSessionKey); legacy != nil {
		candidates = append(candidates, legacy)
	}

	return candidates
}

// customHashCombinedKey derives the customHash combined key from the first
// keyLen bytes of each session key: HMAC-SHA512 chain (pbkdf2SpeedyKey) over
// hex(clientKey[:keyLen] || serverKey[:keyLen]) keyed with pbkdf2ChkSalt,
// truncated to keyLen. Returns nil if either key is shorter than keyLen.
func (s *O5LogonServer) customHashCombinedKey(clientSessionKey []byte, keyLen int) []byte {
	if keyLen <= 0 || len(clientSessionKey) < keyLen || len(s.serverSessionKey) < keyLen {
		return nil
	}

	joined := append(append([]byte{}, clientSessionKey[:keyLen]...), s.serverSessionKey[:keyLen]...)
	hexJoined := strings.ToUpper(hex.EncodeToString(joined))

	return pbkdf2SpeedyKey(s.pbkdf2ChkSalt, []byte(hexJoined), s.pbkdf2SderCount)[:keyLen]
}

// tryDecryptPasswordWithKey attempts AES-192-CBC decryption of the encrypted
// password with the supplied combined key. Returns the recovered password
// (with prefix and null padding stripped) plus ok=true on success. ok=false
// signals the candidate key produced an obviously wrong plaintext (too short
// or non-printable).
func tryDecryptPasswordWithKey(key, encPasswordBytes []byte) (string, bool) {
	plaintext, err := aes192CBCDecrypt(key, encPasswordBytes)
	if err != nil {
		return "", false
	}

	if len(plaintext) <= o5LogonPasswordPrefixLen {
		return "", false
	}

	password := plaintext[o5LogonPasswordPrefixLen:]
	password = trimNullBytes(password)

	if len(password) == 0 || !isPrintableASCII(password) {
		return "", false
	}

	return string(password), true
}

// deriveAESKey returns the verifier key zero-padded to 24 bytes for use as AES-192 key.
// The verifier key is already SHA1(password + salt) = 20 bytes; we just pad to 24.
// go-ora uses the verifier key directly (no additional hashing).
func deriveAESKey(verifierKey []byte) []byte {
	key := make([]byte, o5LogonVerifierKeyLength)
	copy(key, verifierKey)

	return key
}

// deriveCombinedKey derives the password encryption key from both session keys.
// Matches go-ora's non-customHash O5LOGON (6949) path:
// XOR server[16:40] ^ client[16:40], then MD5(first_16) + MD5(last_8), truncated to 24 bytes.
func deriveCombinedKey(serverKey, clientKey []byte) []byte {
	start := 16

	// The XOR window is [16:40]; keys shorter than 40 bytes (e.g. 32-byte 12c
	// session keys) don't use this legacy path.
	if len(serverKey) < start+24 || len(clientKey) < start+24 {
		return nil
	}

	buffer := make([]byte, 24)

	for i := range 24 {
		buffer[i] = serverKey[i+start] ^ clientKey[i+start]
	}

	h1 := md5.New()
	h1.Write(buffer[:16])
	ret := h1.Sum(nil) // 16 bytes

	h2 := md5.New()
	h2.Write(buffer[16:])
	ret = append(ret, h2.Sum(nil)...) // 32 bytes total

	return ret[:24] // truncate to 24 bytes for AES-192
}

// aes192CBCEncrypt encrypts data using AES-192-CBC with a zero IV and PKCS7 padding.
func aes192CBCEncrypt(key, plaintext []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("failed to create cipher: %w", err)
	}

	padded := pkcs7Pad(plaintext, aes.BlockSize)

	iv := make([]byte, aes.BlockSize)
	mode := cipher.NewCBCEncrypter(block, iv)

	ciphertext := make([]byte, len(padded))
	mode.CryptBlocks(ciphertext, padded)

	return ciphertext, nil
}

// aes192CBCEncryptTruncated encrypts with PKCS7 padding then truncates to the original
// input length. Matches go-ora's encryptSessionKey behavior for O5LOGON session keys.
func aes192CBCEncryptTruncated(key, plaintext []byte) ([]byte, error) {
	ciphertext, err := aes192CBCEncrypt(key, plaintext)
	if err != nil {
		return nil, err
	}

	return ciphertext[:len(plaintext)], nil
}

// aes192CBCDecrypt decrypts data using AES-192-CBC with a zero IV.
// PKCS7 padding is removed.
func aes192CBCDecrypt(key, ciphertext []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("failed to create cipher: %w", err)
	}

	if len(ciphertext)%aes.BlockSize != 0 {
		return nil, ErrCiphertextNotAligned
	}

	// Use zero IV (as per O5LOGON protocol)
	iv := make([]byte, aes.BlockSize)
	mode := cipher.NewCBCDecrypter(block, iv)

	plaintext := make([]byte, len(ciphertext))
	mode.CryptBlocks(plaintext, ciphertext)

	// Try to strip PKCS7 padding if present. If the last byte doesn't look like
	// valid PKCS7 padding, return the plaintext as-is (block-aligned data may not be padded).
	if unpadded, err := pkcs7Unpad(plaintext); err == nil {
		return unpadded, nil
	}

	return plaintext, nil
}

// pkcs7Pad applies PKCS#7 padding to the data.
func pkcs7Pad(data []byte, blockSize int) []byte {
	padding := blockSize - len(data)%blockSize
	padText := make([]byte, padding)
	for i := range padText {
		padText[i] = byte(padding)
	}

	return append(data, padText...)
}

// pkcs7Unpad removes PKCS#7 padding from decrypted data.
func pkcs7Unpad(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, ErrInvalidPadding
	}

	padding := int(data[len(data)-1])
	if padding == 0 || padding > aes.BlockSize || padding > len(data) {
		return nil, ErrInvalidPadding
	}

	for i := len(data) - padding; i < len(data); i++ {
		if data[i] != byte(padding) {
			return nil, ErrInvalidPadding
		}
	}

	return data[:len(data)-padding], nil
}

// trimNullBytes removes trailing null bytes from a byte slice.
func trimNullBytes(data []byte) []byte {
	for i := len(data) - 1; i >= 0; i-- {
		if data[i] != 0 {
			return data[:i+1]
		}
	}

	return nil
}
