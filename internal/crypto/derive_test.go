package crypto

import (
	"bytes"
	"encoding/hex"
	"errors"
	"testing"
)

// katMasterKey is the fixed master key the known-answer vectors below were
// produced with. It is a test vector, not a secret.
var katMasterKey = bytes.Repeat([]byte{0x01, 0x02, 0x03, 0x04}, 8)

// TestDeriveAuditChainKeyKnownAnswer pins the derivation. If this test ever
// fails, every chain written by an older build has become unverifiable — the
// derivation is part of the on-disk format, not an implementation detail.
func TestDeriveAuditChainKeyKnownAnswer(t *testing.T) {
	t.Parallel()

	const want = "ab3b1d6a187d3fe1f27b6f4c3296606b134738d87926d823ee6720dd2b026049"

	key, err := DeriveAuditChainKey(katMasterKey)
	if err != nil {
		t.Fatalf("DeriveAuditChainKey() error = %v", err)
	}

	if got := hex.EncodeToString(key); got != want {
		t.Errorf("DeriveAuditChainKey() = %s, want %s", got, want)
	}

	if len(key) != 32 {
		t.Errorf("len(key) = %d, want 32", len(key))
	}
}

func TestDeriveSubkeyKnownAnswer(t *testing.T) {
	t.Parallel()

	const want = "de22248965e2ae4773c15b0ce75cfc281122237f751ef42f08fa5cbec4b7e684"

	key, err := DeriveSubkey(katMasterKey, "dbbat-test-info")
	if err != nil {
		t.Fatalf("DeriveSubkey() error = %v", err)
	}

	if got := hex.EncodeToString(key); got != want {
		t.Errorf("DeriveSubkey() = %s, want %s", got, want)
	}
}

// TestDeriveSubkeySeparation is the property the info string exists for: the
// same master key must not yield the same subkey for two purposes, and the
// chain key must not be the master key itself.
func TestDeriveSubkeySeparation(t *testing.T) {
	t.Parallel()

	chain, err := DeriveAuditChainKey(katMasterKey)
	if err != nil {
		t.Fatalf("DeriveAuditChainKey() error = %v", err)
	}

	other, err := DeriveSubkey(katMasterKey, "dbbat-something-else-v1")
	if err != nil {
		t.Fatalf("DeriveSubkey() error = %v", err)
	}

	if bytes.Equal(chain, other) {
		t.Error("two info strings derived the same subkey")
	}

	if bytes.Equal(chain, katMasterKey) {
		t.Error("the derived subkey is the master key")
	}
}

func TestDeriveSubkeyRejectsBadInput(t *testing.T) {
	t.Parallel()

	if _, err := DeriveSubkey([]byte("too short"), "info"); !errors.Is(err, ErrInvalidKeySize) {
		t.Errorf("DeriveSubkey(short key) error = %v, want ErrInvalidKeySize", err)
	}

	if _, err := DeriveSubkey(katMasterKey, ""); !errors.Is(err, ErrEmptyDeriveInfo) {
		t.Errorf("DeriveSubkey(empty info) error = %v, want ErrEmptyDeriveInfo", err)
	}
}
