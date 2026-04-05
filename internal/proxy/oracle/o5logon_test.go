package oracle

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestO5Logon_VerifierGeneration(t *testing.T) {
	t.Parallel()
	salt, verifier, err := GenerateO5LogonVerifier("test_password")
	require.NoError(t, err)
	assert.Len(t, salt, o5LogonSaltLength)
	assert.Len(t, verifier, o5LogonVerifierKeyLength) // SHA-1 output zero-padded to 24 bytes
}

func TestO5Logon_VerifierDeterministic(t *testing.T) {
	t.Parallel()
	// Same password + salt should produce the same verifier
	salt := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	key1 := deriveVerifierKey("test_password", salt)
	key2 := deriveVerifierKey("test_password", salt)
	assert.Equal(t, key1, key2)

	// Different password should produce different verifier
	key3 := deriveVerifierKey("other_password", salt)
	assert.NotEqual(t, key1, key3)
}

func TestO5Logon_RoundTrip(t *testing.T) {
	t.Parallel()
	password := "dbb_test1234abcdefghijklmnopqrs"

	// Generate verifier from known password
	salt, verifierKey, err := GenerateO5LogonVerifier(password)
	require.NoError(t, err)

	// Server generates challenge
	server := NewO5LogonServer(salt, verifierKey)
	encSessKey, vfrData, err := server.GenerateChallenge()
	require.NoError(t, err)

	// Verify AUTH_VFR_DATA format: hex(salt) + "6949"
	assert.True(t, strings.HasSuffix(vfrData, o5LogonVerifierType))
	saltHex := vfrData[:len(vfrData)-len(o5LogonVerifierType)]
	decodedSalt, err := hex.DecodeString(saltHex)
	require.NoError(t, err)
	assert.Equal(t, salt, decodedSalt)

	// Simulate client response
	clientEncSessKey, clientEncPassword := simulateO5LogonClient(t, password, encSessKey, vfrData)

	// Server decrypts password
	decrypted, err := server.DecryptPassword(clientEncSessKey, clientEncPassword)
	require.NoError(t, err)
	assert.Equal(t, password, decrypted)
}

func TestO5Logon_WrongPassword(t *testing.T) {
	t.Parallel()
	correctPassword := "dbb_correct_password_12345678"
	wrongPassword := "dbb_wrong_password_123456789"

	salt, verifierKey, err := GenerateO5LogonVerifier(correctPassword)
	require.NoError(t, err)

	server := NewO5LogonServer(salt, verifierKey)
	encSessKey, vfrData, err := server.GenerateChallenge()
	require.NoError(t, err)

	// Client uses wrong password — this will produce a client session key
	// encrypted with the wrong verifier, so the server can't decrypt it properly.
	// We use simulateO5LogonClientRaw which doesn't fail on unpadding errors.
	clientEncSessKey, clientEncPassword, simErr := simulateO5LogonClientRaw(wrongPassword, encSessKey, vfrData)
	if simErr != nil {
		// Client-side decryption of server session key failed (expected with wrong password)
		return
	}

	// Server tries to decrypt — should either error or produce wrong password
	decrypted, err := server.DecryptPassword(clientEncSessKey, clientEncPassword)
	assert.True(t, err != nil || decrypted != correctPassword,
		"should not successfully decrypt to correct password with wrong password")
}

func TestO5Logon_MultipleRoundTrips(t *testing.T) {
	t.Parallel()
	// Test that each challenge produces different encrypted values
	password := "dbb_test_key_123456789012345"
	salt, verifierKey, err := GenerateO5LogonVerifier(password)
	require.NoError(t, err)

	server1 := NewO5LogonServer(salt, verifierKey)
	encSessKey1, _, err := server1.GenerateChallenge()
	require.NoError(t, err)

	server2 := NewO5LogonServer(salt, verifierKey)
	encSessKey2, _, err := server2.GenerateChallenge()
	require.NoError(t, err)

	// Different random session keys → different encrypted session keys
	assert.NotEqual(t, encSessKey1, encSessKey2)
}

func TestO5Logon_AES192CBC_RoundTrip(t *testing.T) {
	t.Parallel()
	key := make([]byte, 24) // AES-192
	_, err := rand.Read(key)
	require.NoError(t, err)

	plaintext := []byte("hello world, this is a test")

	encrypted, err := aes192CBCEncrypt(key, plaintext)
	require.NoError(t, err)

	decrypted, err := aes192CBCDecrypt(key, encrypted)
	require.NoError(t, err)

	// With proper PKCS7 unpadding, decrypted should match exactly
	assert.Equal(t, plaintext, decrypted)
}

func TestTrimNullBytes(t *testing.T) {
	t.Parallel()
	assert.Equal(t, []byte("hello"), trimNullBytes([]byte("hello\x00\x00\x00")))
	assert.Equal(t, []byte("hello"), trimNullBytes([]byte("hello")))
	assert.Nil(t, trimNullBytes([]byte{0, 0, 0}))
}

// simulateO5LogonClient simulates the Oracle client-side O5LOGON authentication.
// This mirrors what go-ora's auth_object.go does on the client side.
func simulateO5LogonClient(t *testing.T, password, encServerSessKey, authVfrData string) (string, string) {
	t.Helper()

	// Extract salt from AUTH_VFR_DATA (remove verifier type suffix)
	saltHex := authVfrData[:len(authVfrData)-len(o5LogonVerifierType)]
	salt, err := hex.DecodeString(saltHex)
	require.NoError(t, err)

	// Client derives verifier key from password + salt (same as server)
	verifierKey := deriveVerifierKey(password, salt)

	// Decrypt server session key using verifier key
	decKey := deriveAESKey(verifierKey)
	encServerSessKeyBytes, err := hex.DecodeString(encServerSessKey)
	require.NoError(t, err)

	serverSessionKey, err := aes192CBCDecrypt(decKey, encServerSessKeyBytes)
	require.NoError(t, err)

	// Generate client session key
	clientSessKey := make([]byte, o5LogonSessionKeyLength)
	_, err = rand.Read(clientSessKey)
	require.NoError(t, err)

	// Encrypt client session key with verifier-derived key
	encClientSessKeyBytes, err := aes192CBCEncrypt(decKey, clientSessKey)
	require.NoError(t, err)

	// Derive combined key from both session keys
	combinedKey := deriveCombinedKey(serverSessionKey, clientSessKey)

	// Encrypt password: random_prefix(16 bytes) + password
	prefix := make([]byte, o5LogonPasswordPrefixLen)
	_, err = rand.Read(prefix)
	require.NoError(t, err)

	passwordPayload := make([]byte, 0, len(prefix)+len(password))
	passwordPayload = append(passwordPayload, prefix...)
	passwordPayload = append(passwordPayload, []byte(password)...)
	encPasswordBytes, err := aes192CBCEncrypt(combinedKey, passwordPayload)
	require.NoError(t, err)

	return strings.ToUpper(hex.EncodeToString(encClientSessKeyBytes)),
		strings.ToUpper(hex.EncodeToString(encPasswordBytes))
}

// simulateO5LogonClientRaw is like simulateO5LogonClient but returns errors instead of failing.
func simulateO5LogonClientRaw(password, encServerSessKey, authVfrData string) (string, string, error) {
	saltHex := authVfrData[:len(authVfrData)-len(o5LogonVerifierType)]
	salt, err := hex.DecodeString(saltHex)
	if err != nil {
		return "", "", err
	}

	verifierKey := deriveVerifierKey(password, salt)
	decKey := deriveAESKey(verifierKey)

	encServerSessKeyBytes, err := hex.DecodeString(encServerSessKey)
	if err != nil {
		return "", "", err
	}

	serverSessionKey, err := aes192CBCDecrypt(decKey, encServerSessKeyBytes)
	if err != nil {
		return "", "", err
	}

	clientSessKey := make([]byte, o5LogonSessionKeyLength)
	if _, err = rand.Read(clientSessKey); err != nil {
		return "", "", err
	}

	encClientSessKeyBytes, err := aes192CBCEncrypt(decKey, clientSessKey)
	if err != nil {
		return "", "", err
	}

	combinedKey := deriveCombinedKey(serverSessionKey, clientSessKey)

	prefix := make([]byte, o5LogonPasswordPrefixLen)
	if _, err = rand.Read(prefix); err != nil {
		return "", "", err
	}

	passwordPayload := make([]byte, 0, len(prefix)+len(password))
	passwordPayload = append(passwordPayload, prefix...)
	passwordPayload = append(passwordPayload, []byte(password)...)
	encPasswordBytes, err := aes192CBCEncrypt(combinedKey, passwordPayload)
	if err != nil {
		return "", "", err
	}

	return strings.ToUpper(hex.EncodeToString(encClientSessKeyBytes)),
		strings.ToUpper(hex.EncodeToString(encPasswordBytes)),
		nil
}
