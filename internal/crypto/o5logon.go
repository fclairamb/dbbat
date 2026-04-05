package crypto

import (
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // O5LOGON protocol requires SHA-1
	"fmt"
)

const (
	// O5LogonSaltLength is the length of the O5LOGON salt.
	O5LogonSaltLength = 10
	// O5LogonVerifierKeyLength is the length of the O5LOGON verifier key (SHA-1 zero-padded to 24).
	O5LogonVerifierKeyLength = 24
)

// GenerateO5LogonVerifier creates salt + verifier key from a plaintext password.
// This is used at API key creation time to store O5LOGON verifier data for Oracle proxy auth.
//
// verifier_key = SHA1(password || salt), zero-padded to 24 bytes.
func GenerateO5LogonVerifier(password string) (salt, verifierKey []byte, err error) {
	salt = make([]byte, O5LogonSaltLength)
	if _, err = rand.Read(salt); err != nil {
		return nil, nil, fmt.Errorf("failed to generate salt: %w", err)
	}

	verifierKey = DeriveO5LogonVerifierKey(password, salt)

	return salt, verifierKey, nil
}

// DeriveO5LogonVerifierKey computes the O5LOGON verifier key from password and salt.
func DeriveO5LogonVerifierKey(password string, salt []byte) []byte {
	h := sha1.New() //nolint:gosec // O5LOGON protocol requires SHA-1
	h.Write([]byte(password))
	h.Write(salt)
	hash := h.Sum(nil) // 20 bytes

	key := make([]byte, O5LogonVerifierKeyLength)
	copy(key, hash)

	return key
}
