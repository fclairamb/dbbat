package oracle

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// authSvrResponsePlaintextLen is the length of the AUTH_SVR_RESPONSE plaintext
// the client expects: 16 random prefix bytes followed by the literal ASCII
// marker "SERVER_TO_CLIENT".
const authSvrResponsePlaintextLen = 32

// authSvrResponseHexLen is the on-wire hex length of AUTH_SVR_RESPONSE — 96
// characters encoding 48 bytes (3 AES blocks: 32-byte plaintext plus a full
// PKCS#5 pad block).
const authSvrResponseHexLen = 96

// authSvrResponseMarker is the literal trailing plaintext the client checks
// after AES decryption to confirm the proxy holds the negotiated session key.
var authSvrResponseMarker = []byte("SERVER_TO_CLIENT")

// authSvrResponseKey is the KV key the upstream's AUTH OK packet uses for
// the encrypted server-to-client confirmation field.
var authSvrResponseKey = []byte("AUTH_SVR_RESPONSE")

// patchAuthSvrResponse rewrites the AUTH_SVR_RESPONSE field inside an Oracle
// AUTH OK packet so its ciphertext decrypts under the supplied key. Both
// modern Oracle clients (python-oracledb thin, JDBC thin / SQLcl) verify
// AUTH_SVR_RESPONSE against the O5LOGON combined key they negotiated with the
// proxy; without this patch they reject the AUTH OK with DPY-4035 / ORA-17401
// because the upstream's value is encrypted with the proxy's combined key
// against the upstream, not the client's.
//
// The packet layout (post-flag bytes) for that field is:
//
//	[01 11 11] AUTH_SVR_RESPONSE(17B) [01 60 60] <96 hex chars> 00
//
// The hex value is replaced in place; surrounding bytes (key, length
// prefixes, KV flag) are preserved. Returns a fresh slice.
func patchAuthSvrResponse(authOK []byte, combinedKey []byte) ([]byte, error) {
	if len(authOK) == 0 {
		return nil, ErrAuthSvrResponseKeyNotFound
	}

	keyIdx := bytes.Index(authOK, authSvrResponseKey)
	if keyIdx < 0 {
		return nil, ErrAuthSvrResponseKeyNotFound
	}

	hexStart, err := findAuthSvrResponseHexStart(authOK, keyIdx+len(authSvrResponseKey))
	if err != nil {
		return nil, err
	}

	if hexStart+authSvrResponseHexLen > len(authOK) {
		return nil, ErrAuthSvrResponseTruncated
	}

	freshHex, err := buildAuthSvrResponseValue(combinedKey)
	if err != nil {
		return nil, err
	}

	if len(freshHex) != authSvrResponseHexLen {
		return nil, fmt.Errorf("%w: got %d, want %d", ErrAuthSvrResponseBadLength, len(freshHex), authSvrResponseHexLen)
	}

	out := make([]byte, len(authOK))
	copy(out, authOK)
	copy(out[hexStart:hexStart+authSvrResponseHexLen], freshHex)

	return out, nil
}

// findAuthSvrResponseHexStart locates the first byte of the 96-character hex
// value following the AUTH_SVR_RESPONSE key. The wire format encodes the
// preceding length tag using a TTC compressed integer (`01 60 60`), so we
// scan from `after` until we find a run of 96 uppercase-hex bytes.
func findAuthSvrResponseHexStart(authOK []byte, after int) (int, error) {
	for i := after; i+authSvrResponseHexLen <= len(authOK); i++ {
		if isHexRun(authOK, i, authSvrResponseHexLen) {
			return i, nil
		}
	}

	return 0, fmt.Errorf("%w: no %d-char hex run found near AUTH_SVR_RESPONSE", ErrAuthSvrResponseNotHex, authSvrResponseHexLen)
}

// isHexRun reports whether buf[start:start+length] is composed entirely of
// uppercase hex bytes (0-9, A-F).
func isHexRun(buf []byte, start, length int) bool {
	if start+length > len(buf) {
		return false
	}

	for i := start; i < start+length; i++ {
		c := buf[i]
		if (c < '0' || c > '9') && (c < 'A' || c > 'F') {
			return false
		}
	}

	return true
}

// buildAuthSvrResponseValue computes a fresh AUTH_SVR_RESPONSE hex string
// encrypted under the supplied combined key. The plaintext is 16 random
// prefix bytes followed by the ASCII marker "SERVER_TO_CLIENT"; AES-CBC
// with a zero IV and PKCS#5 padding produces 48 bytes of ciphertext, hex
// encoded as 96 uppercase characters.
func buildAuthSvrResponseValue(combinedKey []byte) ([]byte, error) {
	plaintext := make([]byte, authSvrResponsePlaintextLen)
	if _, err := rand.Read(plaintext[:16]); err != nil {
		return nil, fmt.Errorf("rand AUTH_SVR_RESPONSE prefix: %w", err)
	}

	copy(plaintext[16:], authSvrResponseMarker)

	blk, err := aes.NewCipher(combinedKey)
	if err != nil {
		return nil, fmt.Errorf("aes new cipher: %w", err)
	}

	padded := pkcs5Pad(plaintext, blk.BlockSize())

	enc := cipher.NewCBCEncrypter(blk, make([]byte, blk.BlockSize()))

	out := make([]byte, len(padded))
	enc.CryptBlocks(out, padded)

	hexed := strings.ToUpper(hex.EncodeToString(out))

	return []byte(hexed), nil
}

// AUTH_SVR_RESPONSE patcher errors.
var (
	ErrAuthSvrResponseKeyNotFound = errors.New("AUTH_SVR_RESPONSE key not found in AUTH OK")
	ErrAuthSvrResponseTruncated   = errors.New("AUTH_SVR_RESPONSE value truncated")
	ErrAuthSvrResponseNotHex      = errors.New("AUTH_SVR_RESPONSE value not uppercase hex")
	ErrAuthSvrResponseBadLength   = errors.New("AUTH_SVR_RESPONSE rebuilt with wrong length")
)
