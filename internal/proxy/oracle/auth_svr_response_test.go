package oracle

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// buildFakeAuthOK returns a minimal AUTH OK byte slice shaped like the real one
// around the AUTH_SVR_RESPONSE field: a KV flag, the key, a TTC length tag, a
// 96-char uppercase-hex placeholder value, then a trailing terminator.
func buildFakeAuthOK() []byte {
	b := make([]byte, 0, 13+len(authSvrResponseKey)+authSvrResponseHexLen)
	b = append(b, 0xDE, 0xAD, 0xBE, 0xEF) // leading noise
	b = append(b, 0x01, 0x11, 0x11)       // KV flag + key length tags
	b = append(b, authSvrResponseKey...)
	b = append(b, 0x01, 0x60, 0x60)                                    // TTC length tag (non-hex bytes)
	b = append(b, bytes.Repeat([]byte("0"), authSvrResponseHexLen)...) // placeholder hex
	b = append(b, 0x00, 0xCA, 0xFE)                                    // terminator + trailing noise

	return b
}

func TestPatchAuthSvrResponse_DecryptsToMarker(t *testing.T) {
	t.Parallel()

	authOK := buildFakeAuthOK()
	combinedKey := bytes.Repeat([]byte{0xAB}, 24) // 24-byte AES-192 key

	patched, err := patchAuthSvrResponse(authOK, combinedKey)
	require.NoError(t, err)
	require.Len(t, patched, len(authOK), "patch must preserve packet length")

	// Surrounding bytes must be untouched — only the hex value changes.
	keyIdx := bytes.Index(patched, authSvrResponseKey)
	hexStart, err := findAuthSvrResponseHexStart(patched, keyIdx+len(authSvrResponseKey))
	require.NoError(t, err)
	assert.Equal(t, authOK[:hexStart], patched[:hexStart], "prefix changed")
	assert.Equal(t, authOK[hexStart+authSvrResponseHexLen:], patched[hexStart+authSvrResponseHexLen:], "suffix changed")
	assert.NotEqual(t, authOK[hexStart:hexStart+authSvrResponseHexLen], patched[hexStart:hexStart+authSvrResponseHexLen], "hex value not rewritten")

	// Decrypt the rewritten value and confirm the SERVER_TO_CLIENT marker.
	ciphertext, err := hex.DecodeString(string(patched[hexStart : hexStart+authSvrResponseHexLen]))
	require.NoError(t, err)

	blk, err := aes.NewCipher(combinedKey)
	require.NoError(t, err)

	plaintext := make([]byte, len(ciphertext))
	cipher.NewCBCDecrypter(blk, make([]byte, blk.BlockSize())).CryptBlocks(plaintext, ciphertext)

	assert.Equal(t, authSvrResponseMarker, plaintext[16:authSvrResponsePlaintextLen],
		"plaintext[16:32] must be the SERVER_TO_CLIENT marker")
}

func TestPatchAuthSvrResponse_FreshPerCall(t *testing.T) {
	t.Parallel()

	authOK := buildFakeAuthOK()
	combinedKey := bytes.Repeat([]byte{0xAB}, 24)

	first, err := patchAuthSvrResponse(authOK, combinedKey)
	require.NoError(t, err)
	second, err := patchAuthSvrResponse(authOK, combinedKey)
	require.NoError(t, err)

	// The 16-byte random prefix differs each call, so ciphertext must differ.
	assert.NotEqual(t, first, second, "AUTH_SVR_RESPONSE should be randomized per call")
}

func TestPatchAuthSvrResponse_KeyNotFound(t *testing.T) {
	t.Parallel()

	_, err := patchAuthSvrResponse([]byte("no relevant field here"), bytes.Repeat([]byte{0xAB}, 24))
	assert.ErrorIs(t, err, ErrAuthSvrResponseKeyNotFound)
}

func TestPatchAuthSvrResponse_Empty(t *testing.T) {
	t.Parallel()

	_, err := patchAuthSvrResponse(nil, bytes.Repeat([]byte{0xAB}, 24))
	assert.ErrorIs(t, err, ErrAuthSvrResponseKeyNotFound)
}
