package oracle

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/fclairamb/dbbat/internal/crypto"
)

// cryptoRandRead reads random bytes from crypto/rand.
var cryptoRandRead = rand.Read

// hexDecodeBytes decodes a hex string (case-insensitive) to bytes.
func hexDecodeBytes(s string) ([]byte, error) {
	return hex.DecodeString(strings.ToLower(s))
}

// hexEncode encodes bytes to an uppercase hex string.
func hexEncode(b []byte) string {
	return strings.ToUpper(hex.EncodeToString(b))
}

// encryptO5LogonVerifier encrypts an O5LOGON verifier key with the dbbat master key.
func encryptO5LogonVerifier(verifierKey, encryptionKey []byte, keyPrefix string) ([]byte, error) {
	aad := crypto.APIKeyAAD(keyPrefix)

	encrypted, err := crypto.Encrypt(verifierKey, encryptionKey, aad)
	if err != nil {
		return nil, fmt.Errorf("failed to encrypt O5LOGON verifier: %w", err)
	}

	return encrypted, nil
}

// decryptO5LogonVerifier decrypts an O5LOGON verifier key with the dbbat master key.
func decryptO5LogonVerifier(encVerifier, encryptionKey []byte, keyPrefix string) ([]byte, error) {
	aad := crypto.APIKeyAAD(keyPrefix)

	decrypted, err := crypto.Decrypt(encVerifier, encryptionKey, aad)
	if err != nil {
		return nil, fmt.Errorf("failed to decrypt O5LOGON verifier: %w", err)
	}

	return decrypted, nil
}
