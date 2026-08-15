package oracle

import (
	"github.com/fclairamb/dbbat/internal/store"
)

// decryptO5LogonVerifier decrypts an O5LOGON verifier key with the dbbat master
// key. It forwards to store.DecryptO5LogonVerifier so the proxy and the REST
// API's `oracle_capable` field never disagree about whether a key's verifier
// material is usable.
func decryptO5LogonVerifier(encVerifier, encryptionKey []byte, keyPrefix string) ([]byte, error) {
	return store.DecryptO5LogonVerifier(encVerifier, encryptionKey, keyPrefix)
}
