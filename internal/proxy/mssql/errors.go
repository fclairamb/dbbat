package mssql

import (
	"encoding/binary"
	"errors"
)

// TDS token types the proxy emits (MS-TDS 2.2.5.x). Stage 1 only ever needs to
// say "no", so ERROR and DONE are the whole vocabulary.
const (
	tokenError byte = 0xAA
	tokenDone  byte = 0xFD
)

// DONE token status bits (MS-TDS 2.2.7.6). DONE_FINAL is the zero value and
// needs no name; DONE_ERROR is the one stage 1 ever sets.
const doneError uint16 = 0x0002

// Error numbers dbbat reports. SQL Server's own numbering is dense and
// well-known to clients, so the proxy reuses the numbers whose meaning matches:
//
//   - 18456 "Login failed for user" is what every client already handles as an
//     authentication failure, including the ones that key retry logic off it.
//   - 50000 is the first user-defined number, which is what a message from
//     something that is not SQL Server should use.
const (
	errNumberLoginFailed  int32 = 18456
	errNumberProxyMessage int32 = 50000
)

// Severity classes (MS-TDS: 0-10 informational, 11-16 user-correctable errors,
// 17+ resource/fatal). 14 is what SQL Server uses for a login failure, and it
// is the right register for "your connection was refused, and you can do
// something about it".
const errSeverityUserError byte = 14

// serverNameToken is the server name dbbat reports in its error tokens.
const serverNameToken = "dbbat"

// ErrNotWiredThrough is the stage-1 stub: the handshake completed, and there is
// deliberately nothing behind it yet.
var ErrNotWiredThrough = errors.New("mssql: the SQL Server proxy is not wired through to an upstream yet")

// stubMessage is the text the stub error puts in front of a human. It names the
// stage explicitly so nobody spends an afternoon looking for a misconfiguration
// that is really just an unfinished feature.
const stubMessage = "dbbat: the SQL Server proxy accepted the handshake but is not wired " +
	"through to an upstream database yet (stage 1 of 3). Use the PostgreSQL, Oracle, " +
	"MySQL or MongoDB proxy in the meantime."

// writeUSVarchar appends a US_VARCHAR: a little-endian USHORT character count
// followed by the UCS-2LE text.
func writeUSVarchar(buf []byte, s string) []byte {
	encoded := stringToUCS2(s)

	var count [2]byte

	binary.LittleEndian.PutUint16(count[:], uint16(len(encoded)/2))

	buf = append(buf, count[:]...)

	return append(buf, encoded...)
}

// writeBVarchar appends a B_VARCHAR: a single-byte character count followed by
// the UCS-2LE text. The count is a byte, so the text is truncated at 255
// characters rather than overflowing into the next field.
func writeBVarchar(buf []byte, s string) []byte {
	encoded := stringToUCS2(s)

	const maxChars = 255
	if len(encoded)/2 > maxChars {
		encoded = encoded[:maxChars*2]
	}

	buf = append(buf, byte(len(encoded)/2))

	return append(buf, encoded...)
}

// buildErrorToken renders an ERROR token (0xAA).
//
// Layout: type, USHORT length of everything after it, then Number, State,
// Class, the message, the server name, the procedure name, and a 4-byte line
// number (TDS 7.2+ widened it from 2).
func buildErrorToken(number int32, state, class byte, message, procName string, lineNumber uint32) []byte {
	body := make([]byte, 0, 64)

	var scalar [4]byte

	binary.LittleEndian.PutUint32(scalar[:], uint32(number))
	body = append(body, scalar[:]...)
	body = append(body, state, class)

	body = writeUSVarchar(body, message)
	body = writeBVarchar(body, serverNameToken)
	body = writeBVarchar(body, procName)

	binary.LittleEndian.PutUint32(scalar[:], lineNumber)
	body = append(body, scalar[:]...)

	token := make([]byte, 0, len(body)+3)
	token = append(token, tokenError)

	var length [2]byte

	binary.LittleEndian.PutUint16(length[:], uint16(len(body)))
	token = append(token, length[:]...)

	return append(token, body...)
}

// buildDoneToken renders a DONE token (0xFD): status, current command, and a
// 64-bit row count (TDS 7.2+ widened it from 32).
func buildDoneToken(status uint16, rowCount uint64) []byte {
	token := make([]byte, 13)
	token[0] = tokenDone
	binary.LittleEndian.PutUint16(token[1:3], status)
	binary.LittleEndian.PutUint16(token[3:5], 0) // CurCmd
	binary.LittleEndian.PutUint64(token[5:13], rowCount)

	return token
}

// buildLoginFailure builds the token stream for a rejected login: an ERROR
// token followed by a DONE token flagged as an error, which is exactly the
// shape SQL Server sends and therefore the shape every client understands.
func buildLoginFailure(number int32, message string) []byte {
	stream := buildErrorToken(number, 1, errSeverityUserError, message, "", 1)

	return append(stream, buildDoneToken(doneError, 0)...)
}

// buildStubResponse is the stage-1 reply to a completed handshake.
func buildStubResponse() []byte {
	return buildLoginFailure(errNumberProxyMessage, stubMessage)
}

// buildLoginRejected wraps a handshake-time refusal (unsupported auth mode,
// MARS, a malformed login) in the login-failure shape.
func buildLoginRejected(message string) []byte {
	return buildLoginFailure(errNumberLoginFailed, message)
}
