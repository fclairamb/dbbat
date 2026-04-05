package oracle

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
)

// cryptoRandRead reads random bytes from crypto/rand.
var cryptoRandRead = rand.Read

// hexDecodeBytes decodes a hex string (case-insensitive) to bytes.
func hexDecodeBytes(s string) ([]byte, error) {
	return hex.DecodeString(strings.ToLower(s))
}

// hexEncode encodes bytes to an uppercase hex string.
func hexEncode(b []byte) string {
	return strings.ToUpper(hex.EncodeToString(b))
}
