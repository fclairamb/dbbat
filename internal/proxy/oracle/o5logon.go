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
	o5LogonVerifierType      = "6949" // SHA-1 based verifier (legacy, used in value suffix)
	o5LogonVerifierTypeNum   = 6949   // Verifier type sent as KV pair flag
	o5LogonPasswordPrefixLen = 16     // Random prefix prepended to password by client
)

// O5LogonServer implements server-side O5LOGON authentication.
// This is the mirror of the client-side implementation in go-ora.
type O5LogonServer struct {
	salt             []byte // 10-byte salt (from stored verifier)
	verifierKey      []byte // 24-byte key derived from password+salt
	serverSessionKey []byte // 48-byte random (generated per auth)
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
		salt:        salt,
		verifierKey: verifierKey,
	}
}

// GenerateChallenge produces the AUTH_SESSKEY and AUTH_VFR_DATA for the client.
// Returns hex-encoded encrypted server session key and auth verifier data.
func (s *O5LogonServer) GenerateChallenge() (string, string, error) {
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

// DecryptPassword extracts the plaintext password from the client's AUTH Phase 2.
// The client encrypts: random_prefix(16 bytes) + password using AES-192-CBC
// with a key derived from MD5(server_session_key || client_session_key).
func (s *O5LogonServer) DecryptPassword(encClientSessKey, encPassword string) (string, error) {
	// Decode client session key
	encClientSessKeyBytes, err := hex.DecodeString(encClientSessKey)
	if err != nil {
		return "", fmt.Errorf("failed to decode client session key: %w", err)
	}

	// Decrypt client session key using verifier key
	decKey := deriveAESKey(s.verifierKey)

	clientSessionKey, err := aes192CBCDecrypt(decKey, encClientSessKeyBytes)
	if err != nil {
		return "", fmt.Errorf("failed to decrypt client session key: %w", err)
	}

	// Derive combined key from both session keys
	combinedKey := deriveCombinedKey(s.serverSessionKey, clientSessionKey)

	// Decode and decrypt the password
	encPasswordBytes, err := hex.DecodeString(encPassword)
	if err != nil {
		return "", fmt.Errorf("failed to decode encrypted password: %w", err)
	}

	decryptedPassword, err := aes192CBCDecrypt(combinedKey, encPasswordBytes)
	if err != nil {
		return "", fmt.Errorf("failed to decrypt password: %w", err)
	}

	// Strip the 16-byte random prefix
	if len(decryptedPassword) <= o5LogonPasswordPrefixLen {
		return "", ErrDecryptedPasswordTooShort
	}

	// Remove random prefix and any null padding
	password := decryptedPassword[o5LogonPasswordPrefixLen:]
	// Trim null bytes from the end (PKCS padding remnants)
	password = trimNullBytes(password)

	return string(password), nil
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
func pkcs7Pad(data []byte, blockSize int) []byte { //nolint:unparam
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
