package mssql

import (
	"encoding/binary"
	"errors"
	"fmt"
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
//   - 4060 "Cannot open database requested by the login" is the number for
//     "your credentials are fine, that database is not available to you", which
//     is exactly what an unknown dbbat database name or a missing grant means.
//   - 50000 is the first user-defined number, which is what a message from
//     something that is not SQL Server should use.
const (
	errNumberLoginFailed      int32 = 18456
	errNumberCannotOpenDBName int32 = 4060
	errNumberProxyMessage     int32 = 50000
)

// Severity classes (MS-TDS: 0-10 informational, 11-16 user-correctable errors,
// 17+ resource/fatal). 14 is what SQL Server uses for a login failure, and it
// is the right register for "your connection was refused, and you can do
// something about it".
const errSeverityUserError byte = 14

// serverNameToken is the server name dbbat reports in its error tokens.
const serverNameToken = "dbbat"

// Client-leg refusals. Every one of them is reported to the client as a proper
// TDS login failure, so a driver surfaces the reason instead of a dropped
// socket.
var (
	// ErrAuthFailed — the username/password (or API key) did not check out.
	// One error for every cause on purpose: see clientMessageFor.
	ErrAuthFailed = errors.New("mssql: authentication failed")
	// ErrNoDatabaseRequested — the LOGIN7 carried no database name, so there is
	// nothing to resolve against the dbbat catalogue.
	ErrNoDatabaseRequested = errors.New("mssql: the login named no database")
	// ErrServerNotFound — no SQL Server target is registered in dbbat under the
	// requested name.
	ErrServerNotFound = errors.New("mssql: no SQL Server database registered under that name")
	// ErrNoActiveGrant — the user has no live grant on that database.
	ErrNoActiveGrant = errors.New("mssql: no active grant for this database")
	// ErrAPIKeyOwnerMismatch — the API key is valid but belongs to another
	// user than the one named in the login.
	ErrAPIKeyOwnerMismatch = errors.New("mssql: API key does not belong to this user")
	// ErrUpstreamConnect — dbbat could not open the upstream session.
	ErrUpstreamConnect = errors.New("mssql: upstream connection failed")
)

// authFailedMessage is what a client is told when authentication fails, no
// matter which half of the credential was wrong. Wording it once — and never
// interpolating the reason — is what makes "no such user" and "wrong password"
// indistinguishable, as everywhere else in dbbat.
const authFailedMessage = "Login failed for user '%s'. dbbat could not authenticate " +
	"this login: check the username and the password (or dbb_ API key)."

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

// buildLoginRejected wraps a handshake-time refusal (unsupported auth mode,
// MARS, a malformed login) in the login-failure shape.
func buildLoginRejected(message string) []byte {
	return buildLoginFailure(errNumberLoginFailed, message)
}

// clientMessageFor renders a refusal for the client: the SQL Server error
// number to report, and the text to put in front of a human.
//
// Everything the client is told is derived from the *class* of failure, never
// from the underlying error's own text. That keeps store errors, upstream
// hostnames and driver internals off the client leg, and it is what makes the
// authentication failures indistinguishable from one another.
func clientMessageFor(username string, reason error) (int32, string) {
	switch {
	case errors.Is(reason, ErrAuthFailed), errors.Is(reason, ErrAPIKeyOwnerMismatch):
		return errNumberLoginFailed, fmt.Sprintf(authFailedMessage, username)

	case errors.Is(reason, ErrNoDatabaseRequested):
		return errNumberCannotOpenDBName, "dbbat: this login named no database. Connect with the " +
			"name of the dbbat database entry you have a grant on (for example Database=my-target)."

	case errors.Is(reason, ErrServerNotFound):
		return errNumberCannotOpenDBName, "dbbat: no SQL Server database is registered under that " +
			"name. The Database in your connection string must be the name of the dbbat entry, " +
			"not the name of a database on the server."

	case errors.Is(reason, ErrNoActiveGrant):
		return errNumberCannotOpenDBName, "dbbat: you have no active grant on that database. " +
			"Request access in the dbbat UI and connect again once it is approved."

	case errors.Is(reason, ErrUpstreamConnect):
		return errNumberProxyMessage, "dbbat: could not open a session on the upstream SQL Server. " +
			"The proxy authenticated you, so this is a problem with the target or its stored " +
			"credentials — ask an administrator to run the connectivity check on it."

	default:
		return errNumberLoginFailed, "dbbat: " + reason.Error()
	}
}
