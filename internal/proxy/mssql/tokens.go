package mssql

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// TDS token types the proxy *reads* (MS-TDS 2.2.5.x). The two it writes —
// ERROR and DONE — are declared in errors.go next to their builders.
const (
	tokenInfo          byte = 0xAB
	tokenLoginAck      byte = 0xAD
	tokenEnvChange     byte = 0xE3
	tokenSessionState  byte = 0xE4
	tokenFeatureExtAck byte = 0xEE
	tokenDoneProc      byte = 0xFE
	tokenDoneInProc    byte = 0xFF
)

// featureExtAckTerminator ends the FEATUREEXTACK token's feature list.
const featureExtAckTerminator byte = 0xFF

// doneTokenBodyLen is the fixed body of a DONE/DONEPROC/DONEINPROC token on TDS
// 7.2+: status (2), current command (2) and a 64-bit row count (8).
const doneTokenBodyLen = 12

// ErrTokenTruncated — a token's declared length runs past the end of the
// stream. It means the message was mis-framed, not that the server said
// something the proxy does not model.
var ErrTokenTruncated = errors.New("mssql: truncated TDS token stream")

// tdsMessage is a decoded ERROR (0xAA) or INFO (0xAB) token. The two share a
// body layout; only the token type says whether it is fatal.
//
// It carries String() rather than Error(): it is a decoded token, and the
// caller decides whether an ERROR token becomes a Go error.
type tdsMessage struct {
	Number     int32
	State      byte
	Class      byte
	Message    string
	ServerName string
	ProcName   string
	LineNumber uint32
}

// String renders the message the way a client would show it, so it can be
// wrapped straight into a Go error.
func (m tdsMessage) String() string {
	return fmt.Sprintf("%s (error %d, state %d, class %d)", m.Message, m.Number, m.State, m.Class)
}

// parseInfoBody decodes the body of an ERROR or INFO token.
func parseInfoBody(body []byte) (tdsMessage, error) {
	const fixedHead = 6 // Number (4) + State (1) + Class (1)

	if len(body) < fixedHead {
		return tdsMessage{}, ErrTokenTruncated
	}

	msg := tdsMessage{
		Number: int32(binary.LittleEndian.Uint32(body[0:4])),
		State:  body[4],
		Class:  body[5],
	}

	pos := fixedHead

	text, pos, err := readUSVarchar(body, pos)
	if err != nil {
		return tdsMessage{}, err
	}

	msg.Message = text

	if msg.ServerName, pos, err = readBVarchar(body, pos); err != nil {
		return tdsMessage{}, err
	}

	if msg.ProcName, pos, err = readBVarchar(body, pos); err != nil {
		return tdsMessage{}, err
	}

	// LineNumber is 4 bytes on TDS 7.2+. A server that truncates it is not
	// worth failing the login over, so it is best-effort.
	if pos+4 <= len(body) {
		msg.LineNumber = binary.LittleEndian.Uint32(body[pos : pos+4])
	}

	return msg, nil
}

// readUSVarchar reads a USHORT character count followed by that many UCS-2LE
// characters, returning the decoded text and the position just past it.
func readUSVarchar(buf []byte, pos int) (string, int, error) {
	if pos+2 > len(buf) {
		return "", 0, ErrTokenTruncated
	}

	chars := int(binary.LittleEndian.Uint16(buf[pos : pos+2]))
	pos += 2

	if pos+chars*2 > len(buf) {
		return "", 0, ErrTokenTruncated
	}

	return ucs2ToString(buf[pos : pos+chars*2]), pos + chars*2, nil
}

// readBVarchar reads a single-byte character count followed by that many
// UCS-2LE characters.
func readBVarchar(buf []byte, pos int) (string, int, error) {
	if pos >= len(buf) {
		return "", 0, ErrTokenTruncated
	}

	chars := int(buf[pos])
	pos++

	if pos+chars*2 > len(buf) {
		return "", 0, ErrTokenTruncated
	}

	return ucs2ToString(buf[pos : pos+chars*2]), pos + chars*2, nil
}

// loginOutcome is what a walk of a LOGIN7 response found.
type loginOutcome struct {
	// Acked is true when a LOGINACK token was seen, i.e. the server accepted
	// the login.
	Acked bool
	// Failure is the first ERROR token in the stream, nil when there was none.
	Failure *tdsMessage
}

// scanLoginResponse walks the token stream a server answers LOGIN7 with and
// reports whether the login was accepted.
//
// It is deliberately a partial decoder. dbbat forwards the login response to
// the client byte for byte, so the walk exists only to answer one question —
// did the upstream let us in — and every token it does not need is skipped by
// its length. An unrecognized token stops the walk rather than guessing at a
// length and mis-reading the rest: whatever has been learned so far is still
// true, and the bytes are relayed untouched either way.
//
// Stage 3 grows this into the result accountant (COLMETADATA / ROW / DONE).
func scanLoginResponse(payload []byte) loginOutcome {
	var outcome loginOutcome

	pos := 0

	for pos < len(payload) {
		token := payload[pos]
		pos++

		next, stop := scanToken(token, payload, pos, &outcome)
		if stop {
			return outcome
		}

		pos = next
	}

	return outcome
}

// scanToken advances past one token, recording anything the outcome cares
// about. It returns the position of the next token and whether the walk must
// stop (truncation, or a token whose length cannot be known).
func scanToken(token byte, payload []byte, pos int, outcome *loginOutcome) (int, bool) {
	switch token {
	case tokenError, tokenInfo, tokenLoginAck, tokenEnvChange:
		// USHORT-length tokens.
		if pos+2 > len(payload) {
			return pos, true
		}

		length := int(binary.LittleEndian.Uint16(payload[pos : pos+2]))
		pos += 2

		if pos+length > len(payload) {
			return pos, true
		}

		recordToken(token, payload[pos:pos+length], outcome)

		return pos + length, false

	case tokenSessionState:
		// SESSIONSTATE is the one length-prefixed token with a DWORD length.
		if pos+4 > len(payload) {
			return pos, true
		}

		length := int(binary.LittleEndian.Uint32(payload[pos : pos+4]))
		pos += 4

		if length < 0 || pos+length > len(payload) {
			return pos, true
		}

		return pos + length, false

	case tokenDone, tokenDoneProc, tokenDoneInProc:
		if pos+doneTokenBodyLen > len(payload) {
			return pos, true
		}

		return pos + doneTokenBodyLen, false

	case tokenFeatureExtAck:
		return scanFeatureExtAck(payload, pos)

	default:
		// Not a token this walk models. Stop rather than guess.
		return pos, true
	}
}

// recordToken folds a decoded token into the outcome.
func recordToken(token byte, body []byte, outcome *loginOutcome) {
	switch token {
	case tokenLoginAck:
		outcome.Acked = true
	case tokenError:
		if outcome.Failure != nil {
			return
		}

		if msg, err := parseInfoBody(body); err == nil {
			outcome.Failure = &msg
		}
	}
}

// scanFeatureExtAck skips a FEATUREEXTACK token: a run of
// {feature id, DWORD length, data} entries closed by feature id 0xFF.
func scanFeatureExtAck(payload []byte, pos int) (int, bool) {
	for {
		if pos >= len(payload) {
			return pos, true
		}

		id := payload[pos]
		pos++

		if id == featureExtAckTerminator {
			return pos, false
		}

		if pos+4 > len(payload) {
			return pos, true
		}

		length := int(binary.LittleEndian.Uint32(payload[pos : pos+4]))
		pos += 4

		if length < 0 || pos+length > len(payload) {
			return pos, true
		}

		pos += length
	}
}
