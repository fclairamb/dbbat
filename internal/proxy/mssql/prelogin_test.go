package mssql

import (
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// synthPrelogin builds a PRELOGIN payload the way a client would: a 5-byte
// entry per option, a 0xFF terminator, then the blobs.
func synthPrelogin(opts []preloginOption) []byte {
	tableSize := len(opts)*optionEntrySize + 1

	table := make([]byte, 0, tableSize)
	blobs := make([]byte, 0, 32)
	offset := tableSize

	for _, opt := range opts {
		var entry [optionEntrySize]byte

		entry[0] = opt.Token
		binary.BigEndian.PutUint16(entry[1:3], uint16(offset))
		binary.BigEndian.PutUint16(entry[3:5], uint16(len(opt.Data)))

		table = append(table, entry[:]...)
		blobs = append(blobs, opt.Data...)
		offset += len(opt.Data)
	}

	table = append(table, preloginTerminator)

	return append(table, blobs...)
}

// realClientPrelogin is a byte-for-byte PRELOGIN as emitted by a typical
// driver: VERSION, ENCRYPTION(off), INSTOPT("MSSQLServer"), THREADID, MARS(off).
func realClientPrelogin() []byte {
	return synthPrelogin([]preloginOption{
		{Token: preloginVersion, Data: []byte{0x08, 0x00, 0x01, 0x55, 0x00, 0x00}},
		{Token: preloginEncryption, Data: []byte{encryptOff}},
		{Token: preloginInstOpt, Data: append([]byte("MSSQLServer"), 0x00)},
		{Token: preloginThreadID, Data: []byte{0x00, 0x00, 0x00, 0x00}},
		{Token: preloginMARS, Data: []byte{0x00}},
	})
}

func TestParsePrelogin(t *testing.T) {
	t.Parallel()

	msg, err := parsePrelogin(realClientPrelogin())
	require.NoError(t, err)

	require.Len(t, msg.Options, 5)
	assert.Equal(t, encryptOff, msg.Encryption())
	assert.Equal(t, "MSSQLServer", msg.InstanceName())
	assert.False(t, msg.MARSRequested())
	assert.True(t, msg.has(preloginThreadID))
	assert.False(t, msg.has(preloginFedAuthRequired))
	assert.Equal(t,
		[]string{"VERSION", "ENCRYPTION", "INSTOPT", "THREADID", "MARS"},
		msg.optionNames())
}

func TestParsePreloginErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		payload []byte
		wantErr error
	}{
		{
			name:    "empty payload",
			payload: nil,
			wantErr: ErrPreloginNoTerminator,
		},
		{
			name:    "table runs off the end without a terminator",
			payload: []byte{preloginEncryption, 0x00, 0x00, 0x00, 0x00},
			wantErr: ErrPreloginNoTerminator,
		},
		{
			name:    "entry truncated mid-way",
			payload: []byte{preloginVersion, 0x00, 0x0B},
			wantErr: ErrPreloginTruncated,
		},
		{
			name: "blob offset points past the payload",
			payload: []byte{
				preloginEncryption, 0xFF, 0x00, 0x00, 0x01,
				preloginTerminator,
			},
			wantErr: ErrPreloginTruncated,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := parsePrelogin(tc.payload)
			require.ErrorIs(t, err, tc.wantErr)
		})
	}
}

func TestPreloginRoundTrip(t *testing.T) {
	t.Parallel()

	original := realClientPrelogin()

	msg, err := parsePrelogin(original)
	require.NoError(t, err)

	reserialized := msg.serialize()
	assert.Equal(t, original, reserialized, "serialize must reproduce the exact bytes it parsed")

	again, err := parsePrelogin(reserialized)
	require.NoError(t, err)
	assert.Equal(t, msg.Options, again.Options)
}

func TestPreloginResponseRoundTrip(t *testing.T) {
	t.Parallel()

	client, err := parsePrelogin(realClientPrelogin())
	require.NoError(t, err)

	resp := buildPreloginResponse(client, encryptOff)

	parsed, err := parsePrelogin(resp.serialize())
	require.NoError(t, err)

	version, ok := parsed.get(preloginVersion)
	require.True(t, ok)
	require.Len(t, version, 6)
	assert.Equal(t, advertisedVersionMajor, version[0])
	assert.Equal(t, advertisedVersionBuild, binary.BigEndian.Uint16(version[2:4]))

	assert.Equal(t, encryptOff, parsed.Encryption())

	inst, ok := parsed.get(preloginInstOpt)
	require.True(t, ok)
	assert.Equal(t, []byte{0x00}, inst, "0x00 = the requested instance is accepted")

	thread, ok := parsed.get(preloginThreadID)
	require.True(t, ok)
	assert.Empty(t, thread, "a server has no client thread id to report")

	mars, ok := parsed.get(preloginMARS)
	require.True(t, ok)
	assert.Equal(t, []byte{0x00}, mars, "MARS is refused in v1")

	assert.False(t, parsed.has(preloginFedAuthRequired),
		"FEDAUTHREQUIRED must not be sent unsolicited")
}

func TestPreloginResponseEchoesFedAuthAndTraceOnlyWhenOffered(t *testing.T) {
	t.Parallel()

	raw := synthPrelogin([]preloginOption{
		{Token: preloginEncryption, Data: []byte{encryptOn}},
		{Token: preloginTraceID, Data: make([]byte, 36)},
		{Token: preloginFedAuthRequired, Data: []byte{0x01}},
	})

	client, err := parsePrelogin(raw)
	require.NoError(t, err)

	parsed, err := parsePrelogin(buildPreloginResponse(client, encryptOn).serialize())
	require.NoError(t, err)

	fedAuth, ok := parsed.get(preloginFedAuthRequired)
	require.True(t, ok)
	assert.Equal(t, []byte{0x00}, fedAuth, "federated auth is declined: SQL auth only in v1")

	assert.True(t, parsed.has(preloginTraceID))
}

func TestPreloginMARSRequested(t *testing.T) {
	t.Parallel()

	raw := synthPrelogin([]preloginOption{
		{Token: preloginEncryption, Data: []byte{encryptOn}},
		{Token: preloginMARS, Data: []byte{0x01}},
	})

	client, err := parsePrelogin(raw)
	require.NoError(t, err)
	assert.True(t, client.MARSRequested())

	// Whatever the client asked, the response always refuses.
	parsed, err := parsePrelogin(buildPreloginResponse(client, encryptOn).serialize())
	require.NoError(t, err)

	mars, ok := parsed.get(preloginMARS)
	require.True(t, ok)
	assert.Equal(t, []byte{0x00}, mars)
}

func TestPreloginEncryptionDefaultsToNotSupportedWhenAbsent(t *testing.T) {
	t.Parallel()

	client, err := parsePrelogin(synthPrelogin([]preloginOption{
		{Token: preloginVersion, Data: make([]byte, 6)},
	}))
	require.NoError(t, err)
	assert.Equal(t, encryptNotSup, client.Encryption())
	assert.Empty(t, client.InstanceName())
}

func TestNegotiateEncryption(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		client       byte
		tlsAvailable bool
		wantResponse byte
		wantMode     encryptionMode
		wantErr      bool
	}{
		{"off with tls", encryptOff, true, encryptOff, encryptionLoginOnly, false},
		{"on with tls", encryptOn, true, encryptOn, encryptionFull, false},
		{"req with tls", encryptReq, true, encryptReq, encryptionFull, false},
		{"not-sup with tls", encryptNotSup, true, encryptNotSup, encryptionNone, false},
		{"off without tls", encryptOff, false, encryptNotSup, encryptionNone, false},
		{"not-sup without tls", encryptNotSup, false, encryptNotSup, encryptionNone, false},
		{"on without tls", encryptOn, false, encryptNotSup, encryptionNone, true},
		{"req without tls", encryptReq, false, encryptNotSup, encryptionNone, true},
		{"unknown value with tls", 0x7F, true, encryptNotSup, encryptionNone, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			resp, mode, err := negotiateEncryption(tc.client, tc.tlsAvailable)
			if tc.wantErr {
				require.ErrorIs(t, err, ErrEncryptionNotSupported)
			} else {
				require.NoError(t, err)
			}

			assert.Equal(t, tc.wantResponse, resp)
			assert.Equal(t, tc.wantMode, mode)
		})
	}
}

func TestEncryptionNames(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "ENCRYPT_OFF", encryptionName(encryptOff))
	assert.Equal(t, "ENCRYPT_ON", encryptionName(encryptOn))
	assert.Equal(t, "ENCRYPT_REQ", encryptionName(encryptReq))
	assert.Equal(t, "ENCRYPT_NOT_SUP", encryptionName(encryptNotSup))
	assert.Contains(t, encryptionName(0x42), "0x42")

	assert.Equal(t, "none", encryptionNone.String())
	assert.Equal(t, "login-only", encryptionLoginOnly.String())
	assert.Equal(t, "full", encryptionFull.String())

	assert.Equal(t, "NONCEOPT", preloginOptionName(preloginNonceOpt))
	assert.Equal(t, "TERMINATOR", preloginOptionName(preloginTerminator))
	assert.Equal(t, "0x33", preloginOptionName(0x33))
}
