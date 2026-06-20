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
	o5LogonSessionKey18453Length = 32 // verifier-18453 session key (AES-256), matches Oracle 12c+/23ai

	o5LogonVerifierType      = "6949" // SHA-1 based verifier (legacy, used in value suffix)
	o5LogonVerifierTypeNum   = 6949   // Verifier type sent as KV pair flag
	o5LogonPasswordPrefixLen = 16     // Random prefix prepended to password by client

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

	// CombinedKey is set by DecryptPassword to the negotiated combined key
	// — either MD5/XOR (legacy) or the PBKDF2-customHash derivation. It is
	// the AES key the client expects for AUTH_SVR_RESPONSE in the AUTH OK
	// forwarded back.
	CombinedKey []byte

	// verifierType selects the O5LOGON verifier: VerifierType6949 (SHA-1, legacy,
	// AES-192) or VerifierType18453 (12c PBKDF2/HMAC-SHA512, AES-256). 18453 is
	// what modern thin clients require on Oracle 12c+/23ai; the verifierKey/salt
	// then hold the 18453 material and challenges advertise type 18453.
	verifierType int
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

// NewO5LogonServer creates a server from stored verifier data (legacy 6949).
func NewO5LogonServer(salt, verifierKey []byte) *O5LogonServer {
	return &O5LogonServer{
		salt:            salt,
		verifierKey:     verifierKey,
		verifierType:    VerifierType6949,
		pbkdf2VgenCount: o5LogonDefaultPbkdf2VgenCount,
		pbkdf2SderCount: o5LogonDefaultPbkdf2SderCount,
	}
}

// UseVerifier18453 switches the server to the modern verifier-18453 (12c PBKDF2 /
// HMAC-SHA512) using the supplied 16-byte salt and 32-byte verifier key. This
// implies customHash mode (the challenge carries AUTH_PBKDF2_* fields and the
// combined key uses the PBKDF2 derivation). Use it when the client negotiates
// customHash (Oracle 12c+/23ai) so python-oracledb thin, JDBC thin / SQLcl, and
// sqlplus — which reject the legacy 6949 challenge — can authenticate.
func (s *O5LogonServer) UseVerifier18453(salt, verifierKey []byte) {
	s.salt = salt
	s.verifierKey = verifierKey
	s.verifierType = VerifierType18453
	s.customHash = true
	s.pbkdf2VgenCount = dbbcrypto.O5LogonPbkdf2VgenCount
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
	// Generate random server session key. verifier-18453 uses a 32-byte key
	// (AES-256, matching what real Oracle 12c+/23ai emits); legacy 6949 uses 48
	// bytes (its MD5/XOR derivation reads bytes 16-39).
	sessKeyLen := o5LogonSessionKeyLength
	if s.verifierType == VerifierType18453 {
		sessKeyLen = o5LogonSessionKey18453Length
	}

	s.serverSessionKey = make([]byte, sessKeyLen)
	if _, err := rand.Read(s.serverSessionKey); err != nil {
		return "", "", fmt.Errorf("failed to generate server session key: %w", err)
	}

	if s.customHash {
		s.pbkdf2ChkSalt = make([]byte, o5LogonPbkdf2ChkSaltLen)
		if _, err := rand.Read(s.pbkdf2ChkSalt); err != nil {
			return "", "", fmt.Errorf("failed to generate AUTH_PBKDF2_CSK_SALT: %w", err)
		}
	}

	// Encrypt the server session key. verifier-18453 uses the 32-byte verifier
	// key directly with AES-256-CBC (zero IV, pad block truncated); legacy 6949
	// derives an AES-192 key from the 24-byte verifier.
	var (
		encrypted []byte
		err       error
	)

	if s.verifierType == VerifierType18453 {
		encrypted, err = aesCBCEncryptZeroIV(s.verifierKey, s.serverSessionKey, false)
	} else {
		encrypted, err = aes192CBCEncryptTruncated(deriveAESKey(s.verifierKey), s.serverSessionKey)
	}

	if err != nil {
		return "", "", fmt.Errorf("failed to encrypt server session key: %w", err)
	}

	encSessKey := strings.ToUpper(hex.EncodeToString(encrypted))

	// AUTH_VFR_DATA = hex(salt). The verifier type is sent as the KV pair flag
	// (see buildAuthChallenge), not as a value suffix — go-ora and the thin
	// clients read VerifierType from the flag.
	vfrData := strings.ToUpper(hex.EncodeToString(s.salt))

	return encSessKey, vfrData, nil
}

// VerifierType returns the O5LOGON verifier type the server is configured for
// (6949 or 18453), used by the challenge builder for the AUTH_VFR_DATA flag.
func (s *O5LogonServer) VerifierType() int { return s.verifierType }

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

	// Decrypt client session key using verifier key (AES-256 for 18453, AES-192 for 6949)
	clientSessionKey, err := s.decryptSessionKey(encClientSessKeyBytes)
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

	clientSessionKey, err := s.decryptSessionKey(encClientSessKeyBytes)
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
	// verifier-18453: combined key = chained HMAC-SHA512 over hex(FULL
	// clientSessionKey || FULL serverSessionKey) keyed with pbkdf2ChkSalt for
	// sderCount turns, truncated to 32 bytes (AES-256). Mirrors the 18453 branch
	// of derivePasswordEncKey. customHash-only; no legacy fallback.
	if s.verifierType == VerifierType18453 {
		joined := append(append([]byte{}, clientSessionKey...), s.serverSessionKey...)
		hexJoined := strings.ToUpper(hex.EncodeToString(joined))

		return [][]byte{pbkdf2SpeedyKey(s.pbkdf2ChkSalt, []byte(hexJoined), s.pbkdf2SderCount)[:32]}
	}

	legacy := deriveCombinedKey(s.serverSessionKey, clientSessionKey)

	if !s.customHash {
		return [][]byte{legacy}
	}

	// customHash branch mirrors derivePasswordEncKey in upstream_auth_crypto.go:
	// for verifier 6949 + customHash, joined = clientSessionKey[:24] ||
	// serverSessionKey[:24], then chained HMAC-SHA512 over hex(joined) keyed
	// with pbkdf2ChkSalt for sderCount turns, truncated to 24 bytes.
	joined := append(append([]byte{}, clientSessionKey[:24]...), s.serverSessionKey[:24]...)
	hexJoined := strings.ToUpper(hex.EncodeToString(joined))
	customHashKey := pbkdf2SpeedyKey(s.pbkdf2ChkSalt, []byte(hexJoined), s.pbkdf2SderCount)[:24]

	return [][]byte{customHashKey, legacy}
}

// decryptSessionKey decrypts a hex-decoded O5LOGON session key with the verifier
// key — AES-256 zero-IV (no padding) for verifier-18453, AES-192 for legacy 6949.
func (s *O5LogonServer) decryptSessionKey(enc []byte) ([]byte, error) {
	if s.verifierType == VerifierType18453 {
		return aesCBCDecryptZeroIV(s.verifierKey, enc, false)
	}

	return aes192CBCDecrypt(deriveAESKey(s.verifierKey), enc)
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
