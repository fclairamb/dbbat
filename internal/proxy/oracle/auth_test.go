package oracle

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExtractUsernameFromAuth_LengthPrefixed(t *testing.T) {
	// Simulate: data flags (2 bytes) + func code (1 byte) + skip bytes + length + "SCOTT"
	payload := make([]byte, 0, 20)
	payload = append(payload, 0x00, 0x00) // data flags
	payload = append(payload, 0x73)       // AUTH function code
	payload = append(payload, 0x01, 0x00) // sequence, flags
	payload = append(payload, 0x05)       // length = 5
	payload = append(payload, "SCOTT"...) // username
	payload = append(payload, 0x00, 0x00) // trailing

	username, err := extractUsernameFromAuth(payload)
	require.NoError(t, err)
	assert.Equal(t, "SCOTT", username)
}

func TestExtractUsernameFromAuth_Lowercase(t *testing.T) {
	payload := make([]byte, 0, 20)
	payload = append(payload, 0x00, 0x00)
	payload = append(payload, 0x73)
	payload = append(payload, 0x01, 0x00)
	payload = append(payload, 0x05)
	payload = append(payload, "scott"...)
	payload = append(payload, 0x00)

	username, err := extractUsernameFromAuth(payload)
	require.NoError(t, err)
	assert.Equal(t, "SCOTT", username) // Oracle usernames are uppercased
}

func TestExtractUsernameFromAuth_TooShort(t *testing.T) {
	_, err := extractUsernameFromAuth([]byte{0x00, 0x00})
	assert.ErrorIs(t, err, ErrTTCPayloadTooShort)
}

func TestExtractUsernameFromAuth_Empty(t *testing.T) {
	// Payload with no plausible username
	payload := []byte{0x00, 0x00, 0x73, 0x00, 0x00, 0x00, 0x00}
	_, err := extractUsernameFromAuth(payload)
	assert.ErrorIs(t, err, ErrEmptyUsername)
}

func TestIsPlausibleUsername(t *testing.T) {
	assert.True(t, isPlausibleUsername("SCOTT"))
	assert.True(t, isPlausibleUsername("admin"))
	assert.True(t, isPlausibleUsername("SYS$USER"))
	assert.True(t, isPlausibleUsername("user_123"))
	assert.True(t, isPlausibleUsername("C##ADMIN"))

	assert.False(t, isPlausibleUsername(""))
	assert.False(t, isPlausibleUsername("123user"))        // starts with digit
	assert.False(t, isPlausibleUsername("user name"))      // has space
	assert.False(t, isPlausibleUsername("user@host"))      // has @
	assert.False(t, isPlausibleUsername("SELECT * FROM")) // SQL, not username
}

func TestExtractLengthPrefixedString(t *testing.T) {
	// Simple case: length byte + string
	payload := []byte{0x04, 'T', 'E', 'S', 'T', 0x00}
	result := extractLengthPrefixedString(payload)
	assert.Equal(t, "TEST", result)
}

func TestExtractPrintableASCII(t *testing.T) {
	payload := []byte{0x00, 0x01, 0x02, 'S', 'Y', 'S', 0x00, 0x03}
	result := extractPrintableASCII(payload)
	assert.Equal(t, "SYS", result)
}

func TestExtractPrintableASCII_LongestWins(t *testing.T) {
	payload := []byte{0x00, 'A', 'B', 0x00, 'S', 'C', 'O', 'T', 'T', 0x00}
	result := extractPrintableASCII(payload)
	assert.Equal(t, "SCOTT", result)
}
