package crypto

import (
	"crypto/hkdf"
	"crypto/sha256"
	"fmt"
)

// AuditChainInfo is the HKDF info string that separates the audit-chain MAC key
// from every other use of the master encryption key.
//
// It is versioned on purpose: a future change to the canonical serialization
// bumps the info string, which changes the key, which makes the two chain
// generations trivially distinguishable instead of silently interchangeable.
const AuditChainInfo = "dbbat-audit-chain-v1"

// DeriveSubkey derives a 32-byte purpose-bound subkey from the master
// encryption key (DBB_KEY / DBB_KEYFILE) with HKDF-SHA256.
//
// The salt is deliberately empty: dbbat has no second secret to salt with, and
// HKDF is defined for an empty salt (it degrades to HMAC with a zero key,
// which is the standard construction). The separation between purposes comes
// from info, which must be distinct per purpose.
//
// The result must never be written to the database or to a log. Its whole
// point is that reading the store is not enough to forge what it protects.
func DeriveSubkey(masterKey []byte, info string) ([]byte, error) {
	if len(masterKey) != 32 {
		return nil, fmt.Errorf("%w: got %d bytes", ErrInvalidKeySize, len(masterKey))
	}

	if info == "" {
		return nil, ErrEmptyDeriveInfo
	}

	key, err := hkdf.Key(sha256.New, masterKey, nil, info, 32)
	if err != nil {
		return nil, fmt.Errorf("failed to derive subkey: %w", err)
	}

	return key, nil
}

// DeriveAuditChainKey derives the HMAC-SHA256 key that seals the audit and
// query chains. See DeriveSubkey.
func DeriveAuditChainKey(masterKey []byte) ([]byte, error) {
	return DeriveSubkey(masterKey, AuditChainInfo)
}
