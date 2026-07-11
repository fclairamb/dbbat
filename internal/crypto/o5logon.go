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

	// O5Logon18453SaltLength is the salt length for the verifier-18453 (12c)
	// PBKDF2 path. Real Oracle uses 16 bytes.
	O5Logon18453SaltLength = 16
	// O5Logon18453KeyLength is the length of the derived verifier-18453 key.
	O5Logon18453KeyLength = 32
	// O5Logon18453VgenCount is the PBKDF2 verifier-generation iteration count. It
	// must match what the O5LOGON server advertises as AUTH_PBKDF2_VGEN_COUNT,
	// since the client re-derives the same key from it.
	O5Logon18453VgenCount = 4096
	pbkdf2SpeedyKeyLabel   = "AUTH_PBKDF2_SPEEDY_KEY"
)

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

// GenerateO5Logon18453Verifier creates a fresh salt and the verifier-18453 key
// for the given password. Stored at API key creation so the Oracle proxy can
// answer OCI (verifier-18453) O5LOGON challenges.
func GenerateO5Logon18453Verifier(password string) (salt, verifierKey []byte, err error) {
	salt = make([]byte, O5Logon18453SaltLength)
	if _, err := rand.Read(salt); err != nil {
		return nil, nil, fmt.Errorf("failed to generate 18453 salt: %w", err)
	}

	return salt, DeriveO5Logon18453Key(password, salt), nil
}

// DeriveO5Logon18453Key computes the verifier-18453 key:
//
//	speedy = PBKDF2-HMAC-SHA512(password, salt || "AUTH_PBKDF2_SPEEDY_KEY", vgen)
//	key    = SHA512(speedy || salt)[:32]
//
// Mirrors go-ora's AuthObject verifier-18453 derivation. Uses the fixed
// O5Logon18453VgenCount so the value stays consistent with what the server
// advertises.
func DeriveO5Logon18453Key(password string, salt []byte) []byte {
	message := append(append([]byte{}, salt...), []byte(pbkdf2SpeedyKeyLabel)...)
	speedy := pbkdf2SpeedyKeySHA512(message, []byte(password), O5Logon18453VgenCount)

	buf := append(append([]byte{}, speedy...), salt...)
	full := sha512.Sum512(buf)

	return full[:O5Logon18453KeyLength]
}

// pbkdf2SpeedyKeySHA512 is Oracle's PBKDF2 variant (chained HMAC-SHA512 with the
// XOR fold) used to derive the "speedy key". Matches the oracle proxy's
// pbkdf2SpeedyKey; kept here so key creation (store package) needs no proxy
// dependency.
func pbkdf2SpeedyKeySHA512(buffer, key []byte, turns int) []byte {
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
