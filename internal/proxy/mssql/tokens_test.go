package mssql

import (
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// buildInfoToken renders an INFO (0xAB) token, which shares the ERROR body
// layout. Used to prove the walker does not mistake an informational message
// for a login failure.
func buildInfoToken(number int32, message string) []byte {
	token := buildErrorToken(number, 1, 0, message, "", 1)
	token[0] = tokenInfo

	return token
}

// buildLoginAckToken renders a LOGINACK (0xAD) token the way a server does.
//
// The TDS version inside LOGINACK is big-endian, unlike the little-endian one
// in LOGIN7 — one of the asymmetries that makes hand-rolled TDS interesting.
func buildLoginAckToken(progName string) []byte {
	body := []byte{0x01} // Interface: the server accepted the requested SQL dialect.

	var version [4]byte

	binary.BigEndian.PutUint32(version[:], tdsVersion74)
	body = append(body, version[:]...)
	body = writeBVarchar(body, progName)
	body = append(body, 16, 0, 0x0F, 0xA3) // major, minor, build hi, build lo

	token := []byte{tokenLoginAck}

	var length [2]byte

	binary.LittleEndian.PutUint16(length[:], uint16(len(body)))
	token = append(token, length[:]...)

	return append(token, body...)
}

// buildEnvChangeToken renders an ENVCHANGE (0xE3) token with an opaque body,
// which is all the walker needs to skip one.
func buildEnvChangeToken(kind byte, body []byte) []byte {
	full := append([]byte{kind}, body...)

	token := []byte{tokenEnvChange}

	var length [2]byte

	binary.LittleEndian.PutUint16(length[:], uint16(len(full)))
	token = append(token, length[:]...)

	return append(token, full...)
}

// buildFeatureExtAckToken renders a FEATUREEXTACK (0xEE): one feature entry
// then the 0xFF terminator.
func buildFeatureExtAckToken(featureID byte, data []byte) []byte {
	token := []byte{tokenFeatureExtAck, featureID}

	var length [4]byte

	binary.LittleEndian.PutUint32(length[:], uint32(len(data)))
	token = append(token, length[:]...)
	token = append(token, data...)

	return append(token, featureExtAckTerminator)
}

// buildAcceptedLoginResponse is the token stream a real server answers a good
// LOGIN7 with: a database ENVCHANGE, an informational message about it, the
// LOGINACK, and DONE.
func buildAcceptedLoginResponse() []byte {
	stream := buildEnvChangeToken(0x01, []byte{0x02, 'm', 0x00, 0x02, 'x', 0x00})
	stream = append(stream, buildInfoToken(5701, "Changed database context to 'master'.")...)
	stream = append(stream, buildLoginAckToken("Microsoft SQL Server")...)
	stream = append(stream, buildDoneToken(0, 0)...)

	return stream
}

func TestScanLoginResponseAcceptsAGoodLogin(t *testing.T) {
	t.Parallel()

	outcome := scanLoginResponse(buildAcceptedLoginResponse())

	assert.True(t, outcome.Acked, "LOGINACK must be found past the ENVCHANGE and INFO tokens")
	assert.Nil(t, outcome.Failure, "an INFO token is not a failure")
}

func TestScanLoginResponseDetectsARejectedLogin(t *testing.T) {
	t.Parallel()

	outcome := scanLoginResponse(buildLoginFailure(18456, "Login failed for user 'sa'."))

	require.NotNil(t, outcome.Failure)
	assert.False(t, outcome.Acked)
	assert.Equal(t, int32(18456), outcome.Failure.Number)
	assert.Equal(t, "Login failed for user 'sa'.", outcome.Failure.Message)
	assert.Equal(t, serverNameToken, outcome.Failure.ServerName)
	assert.Contains(t, outcome.Failure.Error(), "error 18456")
}

func TestScanLoginResponseKeepsTheFirstError(t *testing.T) {
	t.Parallel()

	stream := buildErrorToken(18456, 1, 14, "first", "", 1)
	stream = append(stream, buildErrorToken(4060, 1, 11, "second", "", 1)...)
	stream = append(stream, buildDoneToken(doneError, 0)...)

	outcome := scanLoginResponse(stream)

	require.NotNil(t, outcome.Failure)
	assert.Equal(t, "first", outcome.Failure.Message)
}

func TestScanLoginResponseSkipsTokensItDoesNotModel(t *testing.T) {
	t.Parallel()

	// SESSIONSTATE carries a DWORD length, FEATUREEXTACK a terminated feature
	// list — get either wrong and the LOGINACK behind them is never found.
	sessionState := []byte{tokenSessionState, 0x04, 0x00, 0x00, 0x00, 1, 2, 3, 4}

	stream := buildFeatureExtAckToken(0x0A, []byte{0x01})
	stream = append(stream, sessionState...)
	stream = append(stream, buildLoginAckToken("Microsoft SQL Server")...)
	stream = append(stream, buildDoneToken(0, 0)...)

	outcome := scanLoginResponse(stream)

	assert.True(t, outcome.Acked)
	assert.Nil(t, outcome.Failure)
}

func TestScanLoginResponseStopsOnAnUnknownToken(t *testing.T) {
	t.Parallel()

	// 0x81 is COLMETADATA — a real token, but not one this walk models. It must
	// stop rather than guess at a length and mis-read whatever follows.
	stream := append([]byte{0x81, 0xFF, 0xFF}, buildErrorToken(18456, 1, 14, "unreachable", "", 1)...)

	outcome := scanLoginResponse(stream)

	assert.False(t, outcome.Acked)
	assert.Nil(t, outcome.Failure, "nothing past an unmodelled token may be decoded")
}

func TestScanLoginResponseSurvivesTruncation(t *testing.T) {
	t.Parallel()

	full := buildAcceptedLoginResponse()

	for cut := 0; cut < len(full); cut++ {
		// The only requirement is that it terminates and never panics.
		_ = scanLoginResponse(full[:cut])
	}
}

func TestParseInfoBodyRejectsAShortBody(t *testing.T) {
	t.Parallel()

	_, err := parseInfoBody([]byte{0x01, 0x02})
	require.ErrorIs(t, err, ErrTokenTruncated)
}

func TestParseInfoBodyRoundTripsNonASCII(t *testing.T) {
	t.Parallel()

	const message = "Base de données « comptabilité » indisponible"

	token := buildErrorToken(4060, 3, 11, message, "sp_déclencheur", 42)

	// Strip the token byte and the USHORT length to get at the body.
	decoded, err := parseInfoBody(token[3:])
	require.NoError(t, err)

	assert.Equal(t, message, decoded.Message)
	assert.Equal(t, "sp_déclencheur", decoded.ProcName)
	assert.Equal(t, uint32(42), decoded.LineNumber)
	assert.Equal(t, byte(3), decoded.State)
}
