package store

import (
	"errors"
	"fmt"

	"github.com/fclairamb/dbbat/internal/crypto"
)

// ErrAPIKeyNoOracleVerifier is what an API key minted before Oracle support (or
// by a build that had no encryption key to hand) reports when asked for its
// O5LOGON material: there is none, and none can ever be recovered — the key
// itself is Argon2id-hashed, so the verifier only exists if it was derived at
// mint time.
var ErrAPIKeyNoOracleVerifier = errors.New("API key carries no O5LOGON verifier material")

// DecryptO5LogonVerifier decrypts one stored O5LOGON verifier blob with the
// dbbat master key, under the AAD storeO5LogonVerifiers bound it to (the key
// prefix). It lives here, next to the code that writes those blobs, so the
// Oracle proxy and the REST API read them exactly the same way.
func DecryptO5LogonVerifier(encVerifier, encryptionKey []byte, keyPrefix string) ([]byte, error) {
	decrypted, err := crypto.Decrypt(encVerifier, encryptionKey, crypto.APIKeyAAD(keyPrefix))
	if err != nil {
		return nil, fmt.Errorf("failed to decrypt O5LOGON verifier: %w", err)
	}

	return decrypted, nil
}

// DecryptedO5LogonVerifier6949 returns the key's legacy verifier-6949 material,
// decrypted with the given master key. It is the *one* implementation of "this
// key can serve an Oracle login": the proxy calls it to build the O5LOGON
// challenge and OracleLoginCapable below calls it to answer the same question
// for `GET /api/v1/keys`. A second, cheaper-looking predicate ("the column is
// non-empty") would drift from it the moment DBB_KEY changes — material that
// does not decrypt is exactly as unusable as material that was never written.
func (k *APIKey) DecryptedO5LogonVerifier6949(encryptionKey []byte) ([]byte, error) {
	oracleData := k.OracleData()
	if oracleData == nil || len(oracleData.O5LogonSalt6949) == 0 || len(oracleData.O5LogonVerifier6949) == 0 {
		return nil, ErrAPIKeyNoOracleVerifier
	}

	return DecryptO5LogonVerifier(oracleData.O5LogonVerifier6949, encryptionKey, k.KeyPrefix)
}

// OracleLoginCapable reports whether this key can be used as the Oracle
// password for an O5LOGON login: it carries verifier-6949 material AND that
// material decrypts under the master key this process runs with.
//
// Deliberately NOT a backfill point. A key presented over REST arrives in
// plaintext, so its verifier *could* be derived and written then — and that was
// weighed while implementing this (see docs/oracle.md, "Keys that cannot do
// Oracle") and rejected: verifier material is encrypted rather than hashed, so
// minting it for every legacy key on first REST use widens what a stolen store
// yields, to save the user one `dbbat key create`. Reporting the fact is the
// honest fix; the key stays unusable for Oracle until the user rotates it.
//
// Cost: one AES-GCM open (a few microseconds) per key that has material, and no
// crypto at all for one that has none. A listing is bounded by the number of
// keys a user holds, so calling this per row is cheaper than the query that
// fetched them.
func (k *APIKey) OracleLoginCapable(encryptionKey []byte) bool {
	_, err := k.DecryptedO5LogonVerifier6949(encryptionKey)

	return err == nil
}
