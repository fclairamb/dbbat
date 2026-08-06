package mssql

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// PRELOGIN option tokens (MS-TDS 2.2.6.5).
const (
	preloginVersion         byte = 0x00
	preloginEncryption      byte = 0x01
	preloginInstOpt         byte = 0x02
	preloginThreadID        byte = 0x03
	preloginMARS            byte = 0x04
	preloginTraceID         byte = 0x05
	preloginFedAuthRequired byte = 0x06
	preloginNonceOpt        byte = 0x07
	preloginTerminator      byte = 0xFF
)

// preloginOptionName renders an option token for logs. Unknown tokens come
// back as hex: a client offering something dbbat does not model is worth seeing
// in a debug log, not worth failing on.
func preloginOptionName(token byte) string {
	switch token {
	case preloginVersion:
		return "VERSION"
	case preloginEncryption:
		return "ENCRYPTION"
	case preloginInstOpt:
		return "INSTOPT"
	case preloginThreadID:
		return "THREADID"
	case preloginMARS:
		return "MARS"
	case preloginTraceID:
		return "TRACEID"
	case preloginFedAuthRequired:
		return "FEDAUTHREQUIRED"
	case preloginNonceOpt:
		return "NONCEOPT"
	case preloginTerminator:
		return "TERMINATOR"
	default:
		return fmt.Sprintf("%#x", token)
	}
}

// optionNames lists the tokens a message carries, for a single log attribute.
func (m *preloginMessage) optionNames() []string {
	names := make([]string, 0, len(m.Options))
	for _, opt := range m.Options {
		names = append(names, preloginOptionName(opt.Token))
	}

	return names
}

// ENCRYPTION option values (MS-TDS 2.2.6.5).
const (
	// encryptOff — encryption is available but the peer would rather not use
	// it. Note the trap: TDS still runs a TLS handshake in this mode and
	// encrypts the *login packet only*, reverting to cleartext afterwards.
	encryptOff byte = 0x00
	// encryptOn — the peer wants the whole session encrypted.
	encryptOn byte = 0x01
	// encryptNotSup — the peer cannot do TLS at all.
	encryptNotSup byte = 0x02
	// encryptReq — the peer requires the whole session to be encrypted.
	encryptReq byte = 0x03
)

// encryptionName renders an ENCRYPTION value for logs and error text.
func encryptionName(v byte) string {
	switch v {
	case encryptOff:
		return "ENCRYPT_OFF"
	case encryptOn:
		return "ENCRYPT_ON"
	case encryptNotSup:
		return "ENCRYPT_NOT_SUP"
	case encryptReq:
		return "ENCRYPT_REQ"
	default:
		return fmt.Sprintf("ENCRYPT_UNKNOWN(%#x)", v)
	}
}

// encryptionMode is what the negotiation actually decided to do with the
// connection, as opposed to the byte put on the wire.
type encryptionMode int

const (
	// encryptionNone — no TLS handshake happens; the whole session is
	// cleartext.
	encryptionNone encryptionMode = iota
	// encryptionLoginOnly — a TLS handshake happens, the LOGIN7 packet travels
	// inside it, and the stream then reverts to cleartext TDS. This is what
	// `Encrypt=no` means on a SQL Server connection string, and it surprises
	// nearly everyone the first time.
	encryptionLoginOnly
	// encryptionFull — a TLS handshake happens and the session stays inside it.
	encryptionFull
)

// String renders the mode for logs.
func (m encryptionMode) String() string {
	switch m {
	case encryptionNone:
		return "none"
	case encryptionLoginOnly:
		return "login-only"
	case encryptionFull:
		return "full"
	default:
		return "unknown"
	}
}

// PRELOGIN parse errors.
var (
	// ErrPreloginTruncated — the option table or a blob it points at runs past
	// the end of the payload.
	ErrPreloginTruncated = errors.New("mssql: truncated PRELOGIN payload")
	// ErrPreloginNoTerminator — the option table never reached the 0xFF
	// terminator token.
	ErrPreloginNoTerminator = errors.New("mssql: PRELOGIN option table has no terminator")
	// ErrEncryptionNotSupported — the client cannot do TLS but the listener
	// requires it (or the reverse).
	ErrEncryptionNotSupported = errors.New("mssql: TLS encryption cannot be negotiated with this client")
)

// preloginOption is one entry of the PRELOGIN option list, with the blob its
// offset/length pair points at already resolved.
type preloginOption struct {
	Token byte
	Data  []byte
}

// preloginMessage is a parsed PRELOGIN payload. The option order is preserved
// because a few clients care about seeing their own options echoed back in the
// order they sent them.
type preloginMessage struct {
	Options []preloginOption
}

// get returns the blob for a token, and whether the token was present at all.
// A present-but-empty option is meaningful (a server's THREADID response, for
// instance), hence the second return value.
func (m *preloginMessage) get(token byte) ([]byte, bool) {
	for _, opt := range m.Options {
		if opt.Token == token {
			return opt.Data, true
		}
	}

	return nil, false
}

// has reports whether a token is present.
func (m *preloginMessage) has(token byte) bool {
	_, ok := m.get(token)

	return ok
}

// Encryption returns the client's ENCRYPTION byte. A client that omits the
// option entirely is treated as ENCRYPT_NOT_SUP, matching SQL Server.
func (m *preloginMessage) Encryption() byte {
	data, ok := m.get(preloginEncryption)
	if !ok || len(data) == 0 {
		return encryptNotSup
	}

	return data[0]
}

// MARSRequested reports whether the client asked for Multiple Active Result
// Sets. dbbat refuses MARS in v1: it multiplexes independent request streams
// over one connection, which the interception pipeline is not built for.
func (m *preloginMessage) MARSRequested() bool {
	data, ok := m.get(preloginMARS)

	return ok && len(data) > 0 && data[0] != 0x00
}

// InstanceName returns the null-terminated instance name the client asked for,
// if any.
func (m *preloginMessage) InstanceName() string {
	data, ok := m.get(preloginInstOpt)
	if !ok {
		return ""
	}

	for i, b := range data {
		if b == 0x00 {
			return string(data[:i])
		}
	}

	return string(data)
}

// optionEntrySize is the size of one entry in the PRELOGIN option table:
// a token byte plus a big-endian offset/length pair.
const optionEntrySize = 5

// parsePrelogin decodes a PRELOGIN payload: a table of 5-byte entries
// terminated by 0xFF, followed by the blobs those entries point at. Offsets are
// relative to the start of the payload, i.e. just past the TDS packet header.
func parsePrelogin(payload []byte) (*preloginMessage, error) {
	msg := &preloginMessage{}
	pos := 0

	for {
		if pos >= len(payload) {
			return nil, ErrPreloginNoTerminator
		}

		token := payload[pos]
		if token == preloginTerminator {
			return msg, nil
		}

		if pos+optionEntrySize > len(payload) {
			return nil, fmt.Errorf("%w: option entry at %d", ErrPreloginTruncated, pos)
		}

		offset := int(binary.BigEndian.Uint16(payload[pos+1 : pos+3]))
		length := int(binary.BigEndian.Uint16(payload[pos+3 : pos+5]))
		pos += optionEntrySize

		if offset < 0 || length < 0 || offset+length > len(payload) {
			return nil, fmt.Errorf("%w: option %#x at %d+%d", ErrPreloginTruncated, token, offset, length)
		}

		data := make([]byte, length)
		copy(data, payload[offset:offset+length])

		msg.Options = append(msg.Options, preloginOption{Token: token, Data: data})
	}
}

// serialize renders the option list back to wire form: the table first, with
// offsets computed once the table size is known, then the blobs.
func (m *preloginMessage) serialize() []byte {
	tableSize := len(m.Options)*optionEntrySize + 1 // + terminator

	table := make([]byte, 0, tableSize)
	blobs := make([]byte, 0, 64)
	offset := tableSize

	for _, opt := range m.Options {
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

// The version dbbat advertises in its PRELOGIN response. TDS 7.4 is the floor
// this proxy targets (SQL Server 2012+), so the advertised product version is
// one that speaks it — 16.0 (SQL Server 2022). Clients use this for feature
// gating, not for framing; the wire version is settled by LOGIN7.
const (
	advertisedVersionMajor byte   = 16
	advertisedVersionMinor byte   = 0
	advertisedVersionBuild uint16 = 4003
	advertisedSubBuild     uint16 = 0
)

// versionBlob renders the 6-byte VERSION option payload.
func versionBlob() []byte {
	blob := make([]byte, 6)
	blob[0] = advertisedVersionMajor
	blob[1] = advertisedVersionMinor
	binary.BigEndian.PutUint16(blob[2:4], advertisedVersionBuild)
	binary.BigEndian.PutUint16(blob[4:6], advertisedSubBuild)

	return blob
}

// buildPreloginResponse assembles the server's PRELOGIN reply.
//
// VERSION, ENCRYPTION, INSTOPT and MARS are always present; THREADID comes back
// empty (a server has no client thread to report) and FEDAUTHREQUIRED is echoed
// as "not required" only when the client offered it, because a client that did
// not ask will reject an unsolicited option.
//
// MARS is always answered 0x00 — refused. See docs/mssql.md.
func buildPreloginResponse(client *preloginMessage, encryption byte) *preloginMessage {
	resp := &preloginMessage{
		Options: []preloginOption{
			{Token: preloginVersion, Data: versionBlob()},
			{Token: preloginEncryption, Data: []byte{encryption}},
			{Token: preloginInstOpt, Data: []byte{0x00}},
			{Token: preloginThreadID, Data: []byte{}},
			{Token: preloginMARS, Data: []byte{0x00}},
		},
	}

	if client.has(preloginTraceID) {
		// Echoing an empty TRACEID acknowledges the option without inventing a
		// connection/activity id the proxy does not track.
		resp.Options = append(resp.Options, preloginOption{Token: preloginTraceID, Data: []byte{}})
	}

	if client.has(preloginFedAuthRequired) {
		// SQL authentication only in v1 (see docs/mssql.md): federated auth is
		// declined here so the client falls back rather than sending a
		// FEDAUTH token the proxy cannot honor.
		resp.Options = append(resp.Options, preloginOption{Token: preloginFedAuthRequired, Data: []byte{0x00}})
	}

	return resp
}

// negotiateEncryption applies the MS-TDS encryption matrix to the client's
// ENCRYPTION byte and returns the byte to answer with plus what the proxy
// should actually do with the stream.
//
// tlsAvailable is false when the listener has TLS termination disabled
// (DBB_MSSQL_TLS_DISABLE), which makes the proxy behave exactly like a SQL
// Server built without encryption support.
//
//	client          | tls off        | tls on
//	----------------+----------------+-------------------------------
//	ENCRYPT_OFF     | NOT_SUP, none  | OFF, login-only
//	ENCRYPT_ON      | NOT_SUP, error | ON, full
//	ENCRYPT_REQ     | NOT_SUP, error | REQ, full
//	ENCRYPT_NOT_SUP | NOT_SUP, none  | NOT_SUP, none
//
// The login-only row is the one that catches people out: a client connecting
// with `Encrypt=no` still performs a full TLS handshake, sends LOGIN7 inside
// it, and then drops back to cleartext.
func negotiateEncryption(clientValue byte, tlsAvailable bool) (byte, encryptionMode, error) {
	if !tlsAvailable {
		switch clientValue {
		case encryptOn, encryptReq:
			return encryptNotSup, encryptionNone, fmt.Errorf(
				"%w: client sent %s but TLS termination is disabled on this listener",
				ErrEncryptionNotSupported, encryptionName(clientValue))
		default:
			return encryptNotSup, encryptionNone, nil
		}
	}

	switch clientValue {
	case encryptOff:
		return encryptOff, encryptionLoginOnly, nil
	case encryptOn:
		return encryptOn, encryptionFull, nil
	case encryptReq:
		return encryptReq, encryptionFull, nil
	case encryptNotSup:
		// The proxy never *requires* encryption on the client leg — that is
		// the operator's call at the network layer — so a client that cannot
		// do TLS is allowed through in cleartext.
		return encryptNotSup, encryptionNone, nil
	default:
		return encryptNotSup, encryptionNone, fmt.Errorf(
			"%w: unrecognized ENCRYPTION value %s", ErrEncryptionNotSupported, encryptionName(clientValue))
	}
}
