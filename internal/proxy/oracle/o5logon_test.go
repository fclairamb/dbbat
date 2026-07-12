package oracle

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	dbbcrypto "github.com/fclairamb/dbbat/internal/crypto"
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

	// Verify AUTH_VFR_DATA format: hex(salt) — verifier type is sent as KV flag, not suffix
	decodedSalt, err := hex.DecodeString(vfrData)
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
	salt, err := hex.DecodeString(authVfrData)
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
	salt, err := hex.DecodeString(authVfrData)
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

// --- Per-user-salt multi-candidate tests -----------------------------------
//
// With user-shared salts, all of a user's API keys answer the same challenge:
// the challenge ciphertext goes out encrypted under ONE key's verifier (the
// most recently created), and Phase 2 tries each candidate by rebuilding the
// server state as a client holding that key saw it (CloneForCandidate). The
// client role below is computeUpstreamAuthSecrets — dbbat's own go-ora-mirror
// client implementation — so the round trip is authentic for both verifier
// types.

// TestO5Logon_MultiCandidate_6949 covers the legacy 6949 flow: the client
// authenticates with key A while the challenge was built from key B.
func TestO5Logon_MultiCandidate_6949(t *testing.T) {
	t.Parallel()

	userSalt := make([]byte, o5LogonSaltLength)
	_, err := rand.Read(userSalt)
	require.NoError(t, err)

	keyA := "dbb_multicand_aaaa11112222333344"
	keyB := "dbb_multicand_bbbb55556666777788"

	verifierA := deriveVerifierKey(keyA, userSalt)
	verifierB := deriveVerifierKey(keyB, userSalt)

	// Challenge committed to key B's verifier (most recently created).
	server := NewO5LogonServer(userSalt, verifierB)
	encSessKey, vfrData, err := server.GenerateChallenge()
	require.NoError(t, err)

	salt, err := hex.DecodeString(vfrData)
	require.NoError(t, err)
	assert.Equal(t, userSalt, salt, "challenge must carry the shared user salt")

	// Client logs in with key A (authentic client role).
	sec := &upstreamAuthSecrets{verifierType: VerifierType6949, salt: userSalt}
	require.NoError(t, computeUpstreamAuthSecrets(keyA, encSessKey, sec))

	// Primary candidate (key B) must not decrypt to key A.
	if pw, err := server.DecryptPassword(sec.encClientSessKey, sec.encPassword); err == nil {
		assert.NotEqual(t, keyA, pw, "wrong candidate must not recover the password")
	}

	// The clone for key A must recover the password exactly.
	clone, err := server.CloneForCandidate(userSalt, verifierA, encSessKey)
	require.NoError(t, err)

	pw, err := clone.DecryptPassword(sec.encClientSessKey, sec.encPassword)
	require.NoError(t, err)
	assert.Equal(t, keyA, pw)
	assert.NotEmpty(t, clone.CombinedKey, "winning candidate must expose the client's combined key")
}

// TestO5Logon_MultiCandidate_18453 covers the modern PBKDF2/HMAC-SHA512 flow
// (python-oracledb thin, JDBC thin / SQLcl, sqlplus against 12c+/23ai).
func TestO5Logon_MultiCandidate_18453(t *testing.T) {
	t.Parallel()

	userSalt := make([]byte, 16)
	_, err := rand.Read(userSalt)
	require.NoError(t, err)

	keyA := "dbb_multicand_cccc11112222333344"
	keyB := "dbb_multicand_dddd55556666777788"

	verifierA := dbbcryptoDerive18453(keyA, userSalt)
	verifierB := dbbcryptoDerive18453(keyB, userSalt)

	server := NewO5LogonServer(nil, nil)
	server.UseVerifier18453(userSalt, verifierB)

	encSessKey, vfrData, err := server.GenerateChallenge()
	require.NoError(t, err)

	salt, err := hex.DecodeString(vfrData)
	require.NoError(t, err)

	chkSalt, err := hex.DecodeString(server.PBKDF2ChkSalt())
	require.NoError(t, err)

	sec := &upstreamAuthSecrets{
		verifierType:    VerifierType18453,
		customHash:      true,
		salt:            salt,
		pbkdf2ChkSalt:   chkSalt,
		pbkdf2VgenCount: server.PBKDF2VgenCount(),
		pbkdf2SderCount: server.PBKDF2SderCount(),
	}
	require.NoError(t, computeUpstreamAuthSecrets(keyA, encSessKey, sec))

	// Primary candidate (key B) must not decrypt to key A.
	if pw, err := server.DecryptPassword(sec.encClientSessKey, sec.encPassword); err == nil {
		assert.NotEqual(t, keyA, pw, "wrong candidate must not recover the password")
	}

	clone, err := server.CloneForCandidate(userSalt, verifierA, encSessKey)
	require.NoError(t, err)

	pw, err := clone.DecryptPassword(sec.encClientSessKey, sec.encPassword)
	require.NoError(t, err)
	assert.Equal(t, keyA, pw)
}

// TestO5Logon_MultiCandidate_WrongPassword: a password that matches NO key
// must fail against every candidate.
func TestO5Logon_MultiCandidate_WrongPassword(t *testing.T) {
	t.Parallel()

	userSalt := make([]byte, o5LogonSaltLength)
	_, err := rand.Read(userSalt)
	require.NoError(t, err)

	keyA := "dbb_multicand_eeee11112222333344"
	keyB := "dbb_multicand_ffff55556666777788"
	wrong := "dbb_multicand_0000wrongwrongwron"

	verifierA := deriveVerifierKey(keyA, userSalt)
	verifierB := deriveVerifierKey(keyB, userSalt)

	server := NewO5LogonServer(userSalt, verifierB)
	encSessKey, _, err := server.GenerateChallenge()
	require.NoError(t, err)

	sec := &upstreamAuthSecrets{verifierType: VerifierType6949, salt: userSalt}
	require.NoError(t, computeUpstreamAuthSecrets(wrong, encSessKey, sec))

	for name, verifier := range map[string][]byte{"A": verifierA, "B": verifierB} {
		clone, err := server.CloneForCandidate(userSalt, verifier, encSessKey)
		require.NoError(t, err)

		if pw, err := clone.DecryptPassword(sec.encClientSessKey, sec.encPassword); err == nil {
			assert.NotEqual(t, keyA, pw, "candidate %s must not recover key A from a wrong password", name)
			assert.NotEqual(t, keyB, pw, "candidate %s must not recover key B from a wrong password", name)
		}
	}
}

// TestO5Logon_CloneForCandidate_PrimaryEquivalence: cloning with the PRIMARY
// key's own verifier must reproduce the original server session key, so the
// candidate loop can treat every candidate uniformly.
func TestO5Logon_CloneForCandidate_PrimaryEquivalence(t *testing.T) {
	t.Parallel()

	password := "dbb_clone_equiv_1234567890123456"

	salt, verifierKey, err := GenerateO5LogonVerifier(password)
	require.NoError(t, err)

	server := NewO5LogonServer(salt, verifierKey)
	encSessKey, _, err := server.GenerateChallenge()
	require.NoError(t, err)

	clone, err := server.CloneForCandidate(salt, verifierKey, encSessKey)
	require.NoError(t, err)

	assert.Equal(t, server.serverSessionKey, clone.serverSessionKey)
}

// dbbcryptoDerive18453 derives a verifier-18453 key (thin wrapper to keep the
// tests readable).
func dbbcryptoDerive18453(password string, salt []byte) []byte {
	return dbbcrypto.DeriveO5LogonVerifier18453Key(password, salt)
}
