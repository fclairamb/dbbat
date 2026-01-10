package crypto

import (
	"bytes"
	"crypto/rand"
	"errors"
	"testing"
)

func generateKey() []byte {
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	return key
}

func TestEncryptDecrypt(t *testing.T) {
	t.Parallel()

	key := generateKey()

	tests := []struct {
		name      string
		plaintext []byte
	}{
		{name: "simple text", plaintext: []byte("hello world")},
		{name: "empty data", plaintext: []byte{}},
		{name: "binary data", plaintext: []byte{0x00, 0x01, 0x02, 0xFF, 0xFE}},
		{name: "long data", plaintext: bytes.Repeat([]byte("a"), 10000)},
		{name: "unicode data", plaintext: []byte("你好世界 🌍")}, //nolint:gosmopolitan // testing unicode
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ciphertext, err := Encrypt(tt.plaintext, key, nil)
			if err != nil {
				t.Fatalf("Encrypt() error = %v", err)
			}

			// Ciphertext should be different from plaintext
			if bytes.Equal(ciphertext, tt.plaintext) && len(tt.plaintext) > 0 {
				t.Error("Encrypt() ciphertext equals plaintext")
			}

			decrypted, err := Decrypt(ciphertext, key, nil)
			if err != nil {
				t.Fatalf("Decrypt() error = %v", err)
			}

			if !bytes.Equal(decrypted, tt.plaintext) {
				t.Errorf("Decrypt() = %v, want %v", decrypted, tt.plaintext)
			}
		})
	}
}

func TestEncryptInvalidKey(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		keyLen  int
		wantErr bool
	}{
		{name: "valid key 32 bytes", keyLen: 32, wantErr: false},
		{name: "too short 16 bytes", keyLen: 16, wantErr: true},
		{name: "too long 64 bytes", keyLen: 64, wantErr: true},
		{name: "empty key", keyLen: 0, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			key := make([]byte, tt.keyLen)
			_, _ = rand.Read(key)

			_, err := Encrypt([]byte("test"), key, nil)
			if (err != nil) != tt.wantErr {
				t.Errorf("Encrypt() error = %v, wantErr %v", err, tt.wantErr)
			}

			if tt.wantErr && err != nil && !errors.Is(err, ErrInvalidKeySize) {
				t.Errorf("Encrypt() error should wrap ErrInvalidKeySize, got %v", err)
			}
		})
	}
}

func TestDecryptInvalidKey(t *testing.T) {
	t.Parallel()

	key := generateKey()
	plaintext := []byte("secret data")

	ciphertext, err := Encrypt(plaintext, key, nil)
	if err != nil {
		t.Fatalf("Encrypt() error = %v", err)
	}

	// Try decrypting with wrong key
	wrongKey := generateKey()
	_, err = Decrypt(ciphertext, wrongKey, nil)
	if err == nil {
		t.Error("Decrypt() should fail with wrong key")
	}

	// Try decrypting with invalid key size
	shortKey := make([]byte, 16)
	_, err = Decrypt(ciphertext, shortKey, nil)
	if err == nil {
		t.Error("Decrypt() should fail with short key")
	}
}

func TestDecryptTooShortCiphertext(t *testing.T) {
	t.Parallel()

	key := generateKey()

	// GCM nonce is 12 bytes, so anything shorter should fail
	shortCiphertext := make([]byte, 10)

	_, err := Decrypt(shortCiphertext, key, nil)
	if err == nil {
		t.Error("Decrypt() should fail with too short ciphertext")
	}

	if !errors.Is(err, ErrCiphertextTooShort) {
		t.Errorf("Decrypt() error should be ErrCiphertextTooShort, got %v", err)
	}
}

func TestEncryptUnique(t *testing.T) {
	t.Parallel()

	key := generateKey()
	plaintext := []byte("same data")

	ciphertext1, err := Encrypt(plaintext, key, nil)
	if err != nil {
		t.Fatalf("Encrypt() error = %v", err)
	}

	ciphertext2, err := Encrypt(plaintext, key, nil)
	if err != nil {
		t.Fatalf("Encrypt() error = %v", err)
	}

	// Same plaintext should produce different ciphertexts (due to random nonce)
	if bytes.Equal(ciphertext1, ciphertext2) {
		t.Error("Encrypt() produced same ciphertext for same plaintext (nonce should be random)")
	}

	// Both should decrypt to the same plaintext
	decrypted1, err := Decrypt(ciphertext1, key, nil)
	if err != nil || !bytes.Equal(decrypted1, plaintext) {
		t.Error("First ciphertext should decrypt correctly")
	}

	decrypted2, err := Decrypt(ciphertext2, key, nil)
	if err != nil || !bytes.Equal(decrypted2, plaintext) {
		t.Error("Second ciphertext should decrypt correctly")
	}
}

func TestEncryptDecryptWithAAD(t *testing.T) {
	t.Parallel()

	key := generateKey()
	plaintext := []byte("secret-password-123")
	aad := []byte("database:42")

	ciphertext, err := Encrypt(plaintext, key, aad)
	if err != nil {
		t.Fatalf("Encrypt() error = %v", err)
	}

	// Decrypt with correct AAD
	decrypted, err := Decrypt(ciphertext, key, aad)
	if err != nil {
		t.Fatalf("Decrypt() with correct AAD error = %v", err)
	}
	if !bytes.Equal(decrypted, plaintext) {
		t.Errorf("Decrypt() = %q, want %q", decrypted, plaintext)
	}
}

func TestDecryptWithWrongAADFails(t *testing.T) {
	t.Parallel()

	key := generateKey()
	plaintext := []byte("secret-password-123")
	aad := []byte("database:42")
	wrongAAD := []byte("database:99")

	ciphertext, err := Encrypt(plaintext, key, aad)
	if err != nil {
		t.Fatalf("Encrypt() error = %v", err)
	}

	// Decrypt with wrong AAD should fail
	_, err = Decrypt(ciphertext, key, wrongAAD)
	if err == nil {
		t.Fatal("Decrypt() should fail when AAD doesn't match")
	}
}

func TestDecryptWithNilAADWhenEncryptedWithAADFails(t *testing.T) {
	t.Parallel()

	key := generateKey()
	plaintext := []byte("secret-password-123")
	aad := []byte("database:42")

	ciphertext, err := Encrypt(plaintext, key, aad)
	if err != nil {
		t.Fatalf("Encrypt() error = %v", err)
	}

	// Decrypt with nil AAD should fail when encrypted with AAD
	_, err = Decrypt(ciphertext, key, nil)
	if err == nil {
		t.Fatal("Decrypt() should fail when decrypting AAD-bound ciphertext with nil AAD")
	}
}

func TestDecryptWithAADWhenEncryptedWithoutAADFails(t *testing.T) {
	t.Parallel()

	key := generateKey()
	plaintext := []byte("secret-password-123")

	// Encrypt without AAD
	ciphertext, err := Encrypt(plaintext, key, nil)
	if err != nil {
		t.Fatalf("Encrypt() error = %v", err)
	}

	// Decrypt with AAD should fail
	_, err = Decrypt(ciphertext, key, []byte("database:42"))
	if err == nil {
		t.Fatal("Decrypt() should fail when decrypting non-AAD ciphertext with AAD")
	}
}

func TestDatabaseAAD(t *testing.T) {
	t.Parallel()

	tests := []struct {
		uid      string
		expected string
	}{
		{"550e8400-e29b-41d4-a716-446655440000", "database:550e8400-e29b-41d4-a716-446655440000"},
		{"6ba7b810-9dad-11d1-80b4-00c04fd430c8", "database:6ba7b810-9dad-11d1-80b4-00c04fd430c8"},
		{"test-uuid-123", "database:test-uuid-123"},
	}

	for _, tt := range tests {
		aad := DatabaseAAD(tt.uid)
		if string(aad) != tt.expected {
			t.Errorf("DatabaseAAD(%s) = %q, want %q", tt.uid, aad, tt.expected)
		}
	}
}
