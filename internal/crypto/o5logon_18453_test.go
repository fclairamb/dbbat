package crypto

import (
	"bytes"
	"testing"
)

func TestDeriveO5Logon18453Key_Deterministic(t *testing.T) {
	t.Parallel()

	salt := []byte("0123456789abcdef") // 16 bytes

	k1 := DeriveO5Logon18453Key("dbb_secret", salt)
	k2 := DeriveO5Logon18453Key("dbb_secret", salt)

	if !bytes.Equal(k1, k2) {
		t.Fatal("derivation is not deterministic")
	}

	if len(k1) != O5Logon18453KeyLength {
		t.Fatalf("key length = %d, want %d", len(k1), O5Logon18453KeyLength)
	}

	// A different password must yield a different key.
	if bytes.Equal(k1, DeriveO5Logon18453Key("other", salt)) {
		t.Fatal("different passwords produced the same key")
	}
}

func TestGenerateO5Logon18453Verifier(t *testing.T) {
	t.Parallel()

	salt, key, err := GenerateO5Logon18453Verifier("dbb_key")
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	if len(salt) != O5Logon18453SaltLength {
		t.Errorf("salt length = %d, want %d", len(salt), O5Logon18453SaltLength)
	}

	// The stored key must equal a fresh derivation from the same salt — this is
	// what lets an OCI client re-derive it from AUTH_VFR_DATA + the password.
	if !bytes.Equal(key, DeriveO5Logon18453Key("dbb_key", salt)) {
		t.Fatal("stored key does not match re-derivation from salt")
	}
}
