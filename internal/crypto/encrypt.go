package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
)

// Encryption errors.
var (
	ErrInvalidKeySize     = errors.New("key must be 32 bytes")
	ErrCiphertextTooShort = errors.New("ciphertext too short")
)

// Encrypt encrypts plaintext using AES-256-GCM with the provided key.
// The ciphertext includes the nonce prefix.
// Optional aad (Additional Authenticated Data) binds the ciphertext to a context,
// preventing the ciphertext from being used in a different context.
func Encrypt(plaintext []byte, key []byte, aad []byte) ([]byte, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("%w: got %d bytes", ErrInvalidKeySize, len(key))
	}

	// Create AES cipher
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("failed to create cipher: %w", err)
	}

	// Create GCM mode
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("failed to create GCM: %w", err)
	}

	// Generate nonce
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("failed to generate nonce: %w", err)
	}

	// Encrypt and prepend nonce, with optional AAD binding
	ciphertext := gcm.Seal(nonce, nonce, plaintext, aad)

	return ciphertext, nil
}

// Decrypt decrypts ciphertext using AES-256-GCM with the provided key.
// The ciphertext must include the nonce prefix.
// The aad must match the value used during encryption, or be nil for legacy data.
func Decrypt(ciphertext []byte, key []byte, aad []byte) ([]byte, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("%w: got %d bytes", ErrInvalidKeySize, len(key))
	}

	// Create AES cipher
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("failed to create cipher: %w", err)
	}

	// Create GCM mode
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("failed to create GCM: %w", err)
	}

	// Check minimum size
	nonceSize := gcm.NonceSize()
	if len(ciphertext) < nonceSize {
		return nil, ErrCiphertextTooShort
	}

	// Extract nonce and ciphertext
	nonce, ciphertext := ciphertext[:nonceSize], ciphertext[nonceSize:]

	// Decrypt with AAD verification
	plaintext, err := gcm.Open(nil, nonce, ciphertext, aad)
	if err != nil {
		return nil, fmt.Errorf("failed to decrypt: %w", err)
	}

	return plaintext, nil
}

// DatabaseAAD returns the AAD for encrypting database credentials.
// This binds the ciphertext to a specific database UID, preventing
// credential transplant attacks where encrypted passwords are swapped
// between database rows.
func DatabaseAAD(databaseUID string) []byte {
	return []byte(fmt.Sprintf("database:%s", databaseUID))
}
