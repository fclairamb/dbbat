package oracle

import (
	"fmt"

	"github.com/fclairamb/dbbat/internal/crypto"
)

// decryptO5LogonVerifier decrypts an O5LOGON verifier key with the dbbat master key.
func decryptO5LogonVerifier(encVerifier, encryptionKey []byte, keyPrefix string) ([]byte, error) {
	aad := crypto.APIKeyAAD(keyPrefix)

	decrypted, err := crypto.Decrypt(encVerifier, encryptionKey, aad)
	if err != nil {
		return nil, fmt.Errorf("failed to decrypt O5LOGON verifier: %w", err)
	}

	return decrypted, nil
}
