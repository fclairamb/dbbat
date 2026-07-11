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

// patchAuthSvrResponseMultiPacket patches AUTH_SVR_RESPONSE in a raw upstream
// AUTH OK that may span several concatenated TNS Data packets. Because the value
// can straddle a packet boundary (interrupted by the next packet's TNS header +
// data flags), it reassembles the TTC stream with a byte-position map, locates
// and rewrites the value in the contiguous stream, then writes each patched byte
// back to its original position in allRaw — preserving the exact packet framing.
func patchAuthSvrResponseMultiPacket(allRaw, combinedKey []byte) ([]byte, error) {
	ttc, posMap, err := reassembleTTC(allRaw)
	if err != nil {
		return nil, err
	}

	keyIdx := bytes.Index(ttc, authSvrResponseKey)
	if keyIdx < 0 {
		return nil, ErrAuthSvrResponseKeyNotFound
	}

	hexStart, err := findAuthSvrResponseHexStart(ttc, keyIdx+len(authSvrResponseKey))
	if err != nil {
		return nil, err
	}

	freshHex, err := buildAuthSvrResponseValue(combinedKey)
	if err != nil {
		return nil, err
	}

	if len(freshHex) != authSvrResponseHexLen {
		return nil, fmt.Errorf("%w: got %d, want %d", ErrAuthSvrResponseBadLength, len(freshHex), authSvrResponseHexLen)
	}

	out := make([]byte, len(allRaw))
	copy(out, allRaw)

	for j := 0; j < authSvrResponseHexLen; j++ {
		out[posMap[hexStart+j]] = freshHex[j]
	}

	return out, nil
}

// reassembleTTC walks concatenated TNS Data packets and returns the reassembled
// TTC stream (each packet's payload after its 2-byte data-flags prefix) plus a
// map from TTC byte index to the byte's offset in allRaw.
func reassembleTTC(allRaw []byte) ([]byte, []int, error) {
	const tnsHdr = 8

	var (
		ttc    []byte
		posMap []int
	)

	off := 0
	for off+tnsHdr <= len(allRaw) {
		pktLen := int(allRaw[off])<<24 | int(allRaw[off+1])<<16 | int(allRaw[off+2])<<8 | int(allRaw[off+3])
		if pktLen < tnsHdr+ttcDataFlagsSize || off+pktLen > len(allRaw) {
			return nil, nil, ErrAuthSvrResponseTruncated
		}

		// Payload starts after the 8-byte TNS header; skip the 2 data-flag bytes.
		start := off + tnsHdr + ttcDataFlagsSize
		for i := start; i < off+pktLen; i++ {
			ttc = append(ttc, allRaw[i])
			posMap = append(posMap, i)
		}

		off += pktLen
	}

	if len(ttc) == 0 {
		return nil, nil, ErrAuthSvrResponseTruncated
	}

	return ttc, posMap, nil
}

// authSvrResponse18453HexLen is the on-wire hex length of the verifier-18453
// AUTH_SVR_RESPONSE: 64 chars = 32 bytes (16 random prefix + "SERVER_TO_CLIENT",
// AES-256-CBC with no padding).
const authSvrResponse18453HexLen = 64

// patchAuthSvrResponseWide rewrites the AUTH_SVR_RESPONSE of an OCI wide
// verifier-18453 AUTH OK. Unlike the compact form, the 12c AUTH OK leads with
// the bare 64-char hex response value (no "AUTH_SVR_RESPONSE" key), so this
// locates the first isolated 64-char uppercase-hex run and replaces it with a
// value the client can verify under its combined key.
func patchAuthSvrResponseWide(authOK, combinedKey []byte) ([]byte, error) {
	idx := -1

	for i := 0; i+authSvrResponse18453HexLen <= len(authOK); i++ {
		if !isHexRun(authOK, i, authSvrResponse18453HexLen) {
			continue
		}

		// Require the run to be isolated (a KV/framing boundary before and after)
		// so we don't land inside a longer hex field.
		if i > 0 && isHexByte(authOK[i-1]) {
			continue
		}

		if i+authSvrResponse18453HexLen < len(authOK) && isHexByte(authOK[i+authSvrResponse18453HexLen]) {
			continue
		}

		idx = i

		break
	}

	if idx < 0 {
		return nil, ErrAuthSvrResponseKeyNotFound
	}

	fresh, err := buildAuthSvrResponse18453Value(combinedKey)
	if err != nil {
		return nil, err
	}

	if len(fresh) != authSvrResponse18453HexLen {
		return nil, fmt.Errorf("%w: got %d, want %d", ErrAuthSvrResponseBadLength, len(fresh), authSvrResponse18453HexLen)
	}

	out := make([]byte, len(authOK))
	copy(out, authOK)
	copy(out[idx:idx+authSvrResponse18453HexLen], fresh)

	return out, nil
}

// buildAuthSvrResponse18453Value builds the verifier-18453 AUTH_SVR_RESPONSE:
// AES-256-CBC (zero IV, no padding) of 16 random bytes + "SERVER_TO_CLIENT",
// hex-encoded to 64 uppercase chars.
func buildAuthSvrResponse18453Value(combinedKey []byte) ([]byte, error) {
	plaintext := make([]byte, authSvrResponsePlaintextLen)
	if _, err := rand.Read(plaintext[:16]); err != nil {
		return nil, fmt.Errorf("rand AUTH_SVR_RESPONSE prefix: %w", err)
	}

	copy(plaintext[16:], authSvrResponseMarker)

	blk, err := aes.NewCipher(combinedKey)
	if err != nil {
		return nil, fmt.Errorf("aes new cipher: %w", err)
	}

	enc := cipher.NewCBCEncrypter(blk, make([]byte, blk.BlockSize()))
	out := make([]byte, len(plaintext))
	enc.CryptBlocks(out, plaintext)

	return []byte(strings.ToUpper(hex.EncodeToString(out))), nil
}

// isHexByte reports whether c is an uppercase hex digit.
func isHexByte(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'A' && c <= 'F')
}

// AUTH_SVR_RESPONSE patcher errors.
var (
	ErrAuthSvrResponseKeyNotFound = errors.New("AUTH_SVR_RESPONSE key not found in AUTH OK")
	ErrAuthSvrResponseTruncated   = errors.New("AUTH_SVR_RESPONSE value truncated")
	ErrAuthSvrResponseNotHex      = errors.New("AUTH_SVR_RESPONSE value not uppercase hex")
	ErrAuthSvrResponseBadLength   = errors.New("AUTH_SVR_RESPONSE rebuilt with wrong length")
)
