package crypto

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" // O5LOGON protocol requires SHA-1
	"crypto/sha512"
	"fmt"
)

const (
	// O5LogonSaltLength is the length of the O5LOGON salt.
	O5LogonSaltLength = 10
	// O5LogonVerifierKeyLength is the length of the O5LOGON verifier key (SHA-1 zero-padded to 24).
	O5LogonVerifierKeyLength = 24

	// O5LogonPbkdf2SaltLength is the salt length for the modern verifier-18453
	// (12c PBKDF2 / HMAC-SHA512) O5LOGON used by python-oracledb thin, JDBC
	// thin / SQLcl, and sqlplus against Oracle 12c+ / 23ai.
	O5LogonPbkdf2SaltLength = 16
	// O5LogonVerifier18453KeyLength is the verifier-18453 key length (SHA-512 truncated to 32).
	O5LogonVerifier18453KeyLength = 32
	// O5LogonPbkdf2VgenCount is the AUTH_PBKDF2_VGEN_COUNT iteration count. The
	// value used to derive a stored verifier MUST equal the count advertised in
	// the challenge, so generation and the Oracle proxy's challenge builder both
	// reference this constant.
	O5LogonPbkdf2VgenCount = 4096
)

// pbkdf2SpeedyKey computes the "speedy key" for Oracle's PBKDF2 verifier 18453:
// HMAC-SHA512 keyed by the password, chained `turns` times, XORing each
// intermediate hash into the running accumulator. Mirrors generateSpeedyKey in
// go-ora/v2/auth_object.go.
func pbkdf2SpeedyKey(buffer, key []byte, turns int) []byte {
	mac := hmac.New(sha512.New, key)
	mac.Write(append(buffer, 0, 0, 0, 1))

	firstHash := mac.Sum(nil)
	tempHash := make([]byte, len(firstHash))
	copy(tempHash, firstHash)

	for i := 2; i <= turns; i++ {
		mac.Reset()
		mac.Write(tempHash)
		tempHash = mac.Sum(nil)

		for j := 0; j < 64; j++ {
			firstHash[j] ^= tempHash[j]
		}
	}

	return firstHash
}

// GenerateO5LogonVerifier18453 creates the salt + verifier key for the modern
// verifier-18453 O5LOGON from a plaintext password. Stored at API key creation
// so the Oracle proxy can issue PBKDF2 challenges that modern thin clients
// accept (legacy go-ora uses the 6949 verifier instead).
func GenerateO5LogonVerifier18453(password string) ([]byte, []byte, error) {
	salt := make([]byte, O5LogonPbkdf2SaltLength)
	if _, err := rand.Read(salt); err != nil {
		return nil, nil, fmt.Errorf("failed to generate salt: %w", err)
	}

	return salt, DeriveO5LogonVerifier18453Key(password, salt), nil
}

// DeriveO5LogonVerifier18453Key computes the verifier-18453 key from password
// and salt:
//
//	speedyKey   = pbkdf2SpeedyKey(salt || "AUTH_PBKDF2_SPEEDY_KEY", password, vgenCount)
//	verifierKey = SHA-512(speedyKey || salt)[:32]
func DeriveO5LogonVerifier18453Key(password string, salt []byte) []byte {
	message := append(append([]byte{}, salt...), []byte("AUTH_PBKDF2_SPEEDY_KEY")...)
	speedyKey := pbkdf2SpeedyKey(message, []byte(password), O5LogonPbkdf2VgenCount)

	h := sha512.New()
	h.Write(speedyKey)
	h.Write(salt)
	full := h.Sum(nil)

	return full[:O5LogonVerifier18453KeyLength]
}

// GenerateO5LogonVerifier creates salt + verifier key from a plaintext password.
// This is used at API key creation time to store O5LOGON verifier data for Oracle proxy auth.
//
// verifier_key = SHA1(password || salt), zero-padded to 24 bytes.
func GenerateO5LogonVerifier(password string) ([]byte, []byte, error) {
	salt := make([]byte, O5LogonSaltLength)
	if _, err := rand.Read(salt); err != nil {
		return nil, nil, fmt.Errorf("failed to generate salt: %w", err)
	}

	verifierKey := DeriveO5LogonVerifierKey(password, salt)

	return salt, verifierKey, nil
}

// DeriveO5LogonVerifierKey computes the O5LOGON verifier key from password and salt.
func DeriveO5LogonVerifierKey(password string, salt []byte) []byte {
	h := sha1.New() // O5LOGON protocol requires SHA-1
	h.Write([]byte(password))
	h.Write(salt)
	hash := h.Sum(nil) // 20 bytes

	key := make([]byte, O5LogonVerifierKeyLength)
	copy(key, hash)

	return key
}
