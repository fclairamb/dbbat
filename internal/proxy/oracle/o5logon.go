package oracle

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5" //nolint:gosec // O5LOGON protocol requires MD5
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // O5LOGON protocol requires SHA-1
	"encoding/hex"
	"fmt"
	"strings"
)

const (
	o5LogonSaltLength       = 10
	o5LogonVerifierKeyLength = 24 // SHA-1 (20 bytes) zero-padded to 24
	o5LogonSessionKeyLength  = 48
	o5LogonVerifierType      = "6949" // SHA-1 based verifier
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
func GenerateO5LogonVerifier(password string) (salt, verifierKey []byte, err error) {
	salt = make([]byte, o5LogonSaltLength)
	if _, err = rand.Read(salt); err != nil {
		return nil, nil, fmt.Errorf("failed to generate salt: %w", err)
	}

	verifierKey = deriveVerifierKey(password, salt)

	return salt, verifierKey, nil
}

// deriveVerifierKey computes the O5LOGON verifier key from password and salt.
// verifier_key = SHA1(password || salt), zero-padded to 24 bytes.
func deriveVerifierKey(password string, salt []byte) []byte {
	h := sha1.New() //nolint:gosec // O5LOGON protocol requires SHA-1
	h.Write([]byte(password))
	h.Write(salt)
	hash := h.Sum(nil) // 20 bytes

	// Zero-pad to 24 bytes for AES-192
	key := make([]byte, o5LogonVerifierKeyLength)
	copy(key, hash)

	return key
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
func (s *O5LogonServer) GenerateChallenge() (encServerSessKey string, authVfrData string, err error) {
	// Generate random server session key
	s.serverSessionKey = make([]byte, o5LogonSessionKeyLength)
	if _, err = rand.Read(s.serverSessionKey); err != nil {
		return "", "", fmt.Errorf("failed to generate server session key: %w", err)
	}

	// Derive encryption key from verifier key
	encKey := deriveAESKey(s.verifierKey)

	// Encrypt server session key with AES-192-CBC
	encrypted, err := aes192CBCEncrypt(encKey, s.serverSessionKey)
	if err != nil {
		return "", "", fmt.Errorf("failed to encrypt server session key: %w", err)
	}

	encServerSessKey = strings.ToUpper(hex.EncodeToString(encrypted))

	// AUTH_VFR_DATA = hex(salt) + verifier type suffix
	authVfrData = strings.ToUpper(hex.EncodeToString(s.salt)) + o5LogonVerifierType

	return encServerSessKey, authVfrData, nil
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
		return "", fmt.Errorf("decrypted password too short")
	}

	// Remove random prefix and any null padding
	password := decryptedPassword[o5LogonPasswordPrefixLen:]
	// Trim null bytes from the end (PKCS padding remnants)
	password = trimNullBytes(password)

	return string(password), nil
}

// deriveAESKey derives a 24-byte AES-192 key from the verifier key.
// Uses the first 24 bytes of SHA-1(verifier_key), zero-padded.
func deriveAESKey(verifierKey []byte) []byte {
	h := sha1.New() //nolint:gosec // O5LOGON protocol requires SHA-1
	h.Write(verifierKey)
	hash := h.Sum(nil) // 20 bytes

	key := make([]byte, o5LogonVerifierKeyLength)
	copy(key, hash)

	return key
}

// deriveCombinedKey derives the password encryption key from both session keys.
// combined_key = MD5(server_session_key || client_session_key), zero-padded to 24 bytes.
func deriveCombinedKey(serverKey, clientKey []byte) []byte {
	h := md5.New() //nolint:gosec // O5LOGON protocol requires MD5
	h.Write(serverKey)
	h.Write(clientKey)
	hash := h.Sum(nil) // 16 bytes

	key := make([]byte, o5LogonVerifierKeyLength)
	copy(key, hash)

	return key
}

// aes192CBCEncrypt encrypts data using AES-192-CBC with a zero IV.
// PKCS7 padding is applied.
func aes192CBCEncrypt(key, plaintext []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("failed to create cipher: %w", err)
	}

	// Apply PKCS7 padding
	padded := pkcs7Pad(plaintext, aes.BlockSize)

	// Use zero IV (as per O5LOGON protocol)
	iv := make([]byte, aes.BlockSize)
	mode := cipher.NewCBCEncrypter(block, iv)

	ciphertext := make([]byte, len(padded))
	mode.CryptBlocks(ciphertext, padded)

	return ciphertext, nil
}

// aes192CBCDecrypt decrypts data using AES-192-CBC with a zero IV.
// PKCS7 padding is removed.
func aes192CBCDecrypt(key, ciphertext []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("failed to create cipher: %w", err)
	}

	if len(ciphertext)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("ciphertext is not a multiple of block size")
	}

	// Use zero IV (as per O5LOGON protocol)
	iv := make([]byte, aes.BlockSize)
	mode := cipher.NewCBCDecrypter(block, iv)

	plaintext := make([]byte, len(ciphertext))
	mode.CryptBlocks(plaintext, ciphertext)

	// Strip PKCS7 padding
	plaintext, err = pkcs7Unpad(plaintext)
	if err != nil {
		return nil, fmt.Errorf("failed to unpad: %w", err)
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
		return nil, fmt.Errorf("empty data")
	}

	padding := int(data[len(data)-1])
	if padding == 0 || padding > aes.BlockSize || padding > len(data) {
		return nil, fmt.Errorf("invalid padding value: %d", padding)
	}

	for i := len(data) - padding; i < len(data); i++ {
		if data[i] != byte(padding) {
			return nil, fmt.Errorf("invalid padding byte at position %d", i)
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
