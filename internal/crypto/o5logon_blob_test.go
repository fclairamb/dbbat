package crypto

import (
	"bytes"
	"testing"
)

func TestO5LogonVerifierBlob_RoundTrip(t *testing.T) {
	t.Parallel()

	verifier := bytes.Repeat([]byte{0xAA}, O5LogonVerifierKeyLength)
	salt := bytes.Repeat([]byte{0xBB}, O5Logon18453SaltLength)
	key := bytes.Repeat([]byte{0xCC}, O5Logon18453KeyLength)

	fields := DecodeO5LogonVerifierBlob(EncodeO5LogonVerifierBlob(verifier, salt, key))
	if len(fields) != 3 {
		t.Fatalf("got %d fields, want 3", len(fields))
	}

	if !bytes.Equal(fields[0], verifier) || !bytes.Equal(fields[1], salt) || !bytes.Equal(fields[2], key) {
		t.Fatal("round-trip mismatch")
	}
}

func TestO5LogonVerifierBlob_LegacyRawVerifier(t *testing.T) {
	t.Parallel()

	// A legacy row holds a bare 6949 verifier key with no magic prefix.
	legacy := bytes.Repeat([]byte{0x11}, O5LogonVerifierKeyLength)

	fields := DecodeO5LogonVerifierBlob(legacy)
	if len(fields) != 1 {
		t.Fatalf("got %d fields, want 1 for legacy blob", len(fields))
	}

	if !bytes.Equal(fields[0], legacy) {
		t.Fatal("legacy verifier not returned verbatim")
	}
}
