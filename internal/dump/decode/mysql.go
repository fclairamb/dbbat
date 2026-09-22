package decode

import (
	"fmt"

	gomysql "github.com/go-mysql-org/go-mysql/mysql"

	"github.com/fclairamb/dbbat/internal/dump"
)

// MySQL framing constants.
const (
	// mysqlHeaderLen is the 3-byte little-endian payload length plus the
	// one-byte sequence id every MySQL packet carries.
	mysqlHeaderLen = 4

	// mysqlMaxPayload is the largest payload one packet can hold. A payload of
	// exactly this size is continued by the next packet, so the splitter
	// concatenates until it meets a shorter one.
	mysqlMaxPayload = 0xFFFFFF

	// mysqlSSLRequestLen is the fixed size of the abbreviated handshake
	// response that asks for TLS: capabilities, max packet size, charset and
	// 23 filler bytes, and no username.
	mysqlSSLRequestLen = 32

	// mysqlEOFMaxLen bounds a packet that ends a result set. A text row may
	// also begin with 0xFE — as the prefix of an 8-byte-counted value — so the
	// length is what tells the two apart, which is how every MySQL client does
	// it too.
	mysqlEOFMaxLen = 9

	// mysqlTrueEOFMaxLen separates the two things that share the 0xFE header at
	// the end of a result set: a real EOF packet (0xFE plus two int16s, or a
	// bare byte on a pre-4.1 server) and, under CLIENT_DEPRECATE_EOF, an OK
	// packet wearing the EOF header, whose body is two length-encoded integers
	// and two int16s and is therefore longer. Going by the shape means a
	// capture that starts after authentication — every dbbat capture does — is
	// read correctly without knowing what capabilities were negotiated.
	mysqlTrueEOFMaxLen = 5

	// mysqlPrepareOKLen is the fixed body of a COM_STMT_PREPARE answer: status,
	// statement id, column count, parameter count, a filler and the warnings.
	mysqlPrepareOKLen = 12

	// mysqlProtocolVersion10 is the first byte of the server greeting.
	mysqlProtocolVersion10 = 0x0A
)

// mysqlClientPhase is what the next client packet is expected to be.
type mysqlClientPhase int

const (
	// mysqlClientCommand is the steady state and the *initial* one: dbbat's tap
	// is installed after authentication (internal/proxy/mysql/session.go), so
	// the first frame of a dbbat capture is a command packet with sequence 0.
	// A handshake is picked up only if one actually shows up on the wire.
	mysqlClientCommand mysqlClientPhase = iota
	mysqlClientHandshake
	mysqlClientAuth
	mysqlClientInfile
)

// mysqlServerPhase is where the server stream is in answering a command.
type mysqlServerPhase int

const (
	mysqlServerIdle mysqlServerPhase = iota
	mysqlServerHandshake
	mysqlServerAuth
	mysqlServerResponse
	mysqlServerColumnDefs
	mysqlServerPrepareDefs
	mysqlServerRows
)

// mysqlSplitter cuts both directions of a MySQL/MariaDB session into messages.
//
// Framing is the easy half — a 3-byte little-endian length, a sequence id, and
// a payload continued into the next packet when it is exactly 0xFFFFFF bytes.
// The hard half is that a MySQL packet carries no type field: what a payload
// means depends on what was asked for. So the splitter tracks the command in
// flight and where its answer is (column definitions, rows, the terminator),
// the way a client driver does.
//
// Redaction follows the PostgreSQL splitter: statement text is printed, result
// rows collapse to counts unless Options.ShowRows is set, and authentication
// payloads are never printed either way.
//
// Binary-protocol rows and COM_STMT_EXECUTE parameters are counted even under
// --rows, and the reason is worth stating precisely for whoever extends this:
// it is not that the types are out of reach. A binary result set re-sends its
// column definitions, type byte included, ahead of its rows exactly as a text
// one does — serverDefinition parses that very packet and keeps only the name
// — and a COM_STMT_EXECUTE carries its own parameter types whenever the
// new-params-bound flag is set, inheriting the previous execute's when it is
// not. What is missing is the reader: a NULL bitmap plus a decoder per MySQL
// type, which this pass did not write.
type mysqlSplitter struct {
	opts Options

	clientBuf, serverBuf []byte
	// clientLarge/serverLarge accumulate a payload split across the 16MB
	// continuation boundary.
	clientLarge, serverLarge []byte

	clientPhase mysqlClientPhase
	serverPhase mysqlServerPhase

	// encrypted is set once the client asked for TLS inside the capture:
	// everything after that is TLS records. dbbat's own tap sits above TLS, so
	// this only happens on a capture made some other way.
	encrypted bool

	capabilities uint32

	// pendingCmd is the command byte the server is currently answering.
	pendingCmd byte

	columns  int
	defsLeft int
	// pendingDefsEOF marks the boundary EOF that a server which did not
	// negotiate CLIENT_DEPRECATE_EOF sends between the column definitions and
	// the rows. It is cleared by that EOF or by the first row, whichever comes.
	pendingDefsEOF bool
	binaryRows     bool
}

func newMySQLSplitter(opts Options) *mysqlSplitter {
	return &mysqlSplitter{opts: opts}
}

// Feed consumes one packet and returns the messages it completed.
func (m *mysqlSplitter) Feed(packet *dump.Packet) ([]Message, error) {
	if m.encrypted {
		return nil, nil
	}

	if packet.Direction == dump.DirServerToClient {
		m.serverBuf = append(m.serverBuf, packet.Data...)

		return m.drain(packet.RelativeNs, dump.DirServerToClient)
	}

	m.clientBuf = append(m.clientBuf, packet.Data...)

	return m.drain(packet.RelativeNs, dump.DirClientToServer)
}

// drain cuts every complete packet out of one direction's buffer and renders
// the ones that complete a message.
func (m *mysqlSplitter) drain(relativeNs int64, direction byte) ([]Message, error) {
	buf, large := &m.clientBuf, &m.clientLarge
	if direction == dump.DirServerToClient {
		buf, large = &m.serverBuf, &m.serverLarge
	}

	var out []Message

	for {
		payload, ok := cutMySQLPacket(buf)
		if !ok {
			return out, nil
		}

		if len(payload) == mysqlMaxPayload {
			// Not a message yet: the next packet continues this payload.
			*large = append(*large, payload...)

			continue
		}

		if len(*large) > 0 {
			payload = append(*large, payload...)
			*large = nil
		}

		var (
			text string
			err  error
		)

		if direction == dump.DirServerToClient {
			text, err = m.serverMessage(payload)
		} else {
			text, err = m.clientMessage(payload)
		}

		if err != nil {
			return out, err
		}

		out = append(out, Message{relativeNs, direction, text})

		if m.encrypted {
			return out, nil
		}
	}
}

// cutMySQLPacket cuts one packet's payload off the front of buf. ok is false
// when the packet is not fully buffered yet.
func cutMySQLPacket(buf *[]byte) ([]byte, bool) {
	if len(*buf) < mysqlHeaderLen {
		return nil, false
	}

	length := int((*buf)[0]) | int((*buf)[1])<<8 | int((*buf)[2])<<16

	total := mysqlHeaderLen + length
	if len(*buf) < total {
		return nil, false
	}

	payload := (*buf)[mysqlHeaderLen:total]
	*buf = (*buf)[total:]

	return payload, true
}

// clientMessage renders one client-to-server packet.
func (m *mysqlSplitter) clientMessage(payload []byte) (string, error) {
	switch m.clientPhase {
	case mysqlClientHandshake:
		return m.clientHandshake(payload), nil
	case mysqlClientAuth:
		// A password, a scramble or a public-key request. Named, never
		// printed: the rendering is a free function precisely so that no
		// Options is reachable from it and --rows cannot grow a way in.
		m.clientPhase = mysqlClientCommand

		return formatMySQLAuthResponse(payload), nil
	case mysqlClientInfile:
		return m.clientInfile(payload), nil
	case mysqlClientCommand:
		return m.clientCommand(payload)
	default:
		return m.clientCommand(payload)
	}
}

// clientInfile reads the file body a LOCAL INFILE request pulled out of the
// client. It is table data, so it is counted and never printed — not even
// under --rows, which opts into individual values, not a bulk export.
func (m *mysqlSplitter) clientInfile(payload []byte) string {
	if len(payload) == 0 {
		m.clientPhase = mysqlClientCommand
		m.serverPhase = mysqlServerResponse

		return "LocalInfileEnd"
	}

	return fmt.Sprintf("LocalInfileData(%d bytes)", len(payload))
}

// clientHandshake reads the reply to the server greeting. The abbreviated
// 32-byte form is a TLS request, after which nothing else in the capture is a
// MySQL frame.
func (m *mysqlSplitter) clientHandshake(payload []byte) string {
	if len(payload) == mysqlSSLRequestLen {
		m.encrypted = true

		return "SSLRequest (accepted, rest of the capture is TLS)"
	}

	m.capabilities = mysqlCapabilities(payload)
	m.clientPhase = mysqlClientAuth
	m.serverPhase = mysqlServerAuth

	return formatMySQLHandshakeResponse(payload, m.capabilities)
}

// clientCommand reads a command packet: one command byte, then its arguments.
func (m *mysqlSplitter) clientCommand(payload []byte) (string, error) {
	if len(payload) == 0 {
		return "", fmt.Errorf("%w: empty MySQL command packet", ErrOutOfSync)
	}

	m.finishCommand()
	m.pendingCmd = payload[0]
	m.serverPhase = mysqlServerResponse
	m.clientPhase = mysqlClientCommand

	if payload[0] == gomysql.COM_CHANGE_USER {
		// Re-authentication mid-session: the rest of the packet is a username
		// and an auth response, and the server answers on the auth path.
		m.serverPhase = mysqlServerAuth
	}

	return formatMySQLCommand(payload[0], payload[1:]), nil
}

// serverMessage renders one server-to-client packet.
func (m *mysqlSplitter) serverMessage(payload []byte) (string, error) {
	if len(payload) == 0 {
		return "", fmt.Errorf("%w: empty MySQL server packet", ErrOutOfSync)
	}

	switch m.serverPhase {
	case mysqlServerIdle, mysqlServerHandshake:
		if payload[0] == mysqlProtocolVersion10 {
			m.clientPhase = mysqlClientHandshake
			m.serverPhase = mysqlServerHandshake

			return formatMySQLGreeting(payload), nil
		}

		return m.serverResponse(payload), nil
	case mysqlServerAuth:
		return m.serverAuth(payload), nil
	case mysqlServerResponse:
		return m.serverResponse(payload), nil
	case mysqlServerColumnDefs, mysqlServerPrepareDefs:
		return m.serverDefinition(payload), nil
	case mysqlServerRows:
		return m.serverRow(payload), nil
	default:
		return m.serverResponse(payload), nil
	}
}

// serverAuth covers the packets exchanged while a login (or a COM_CHANGE_USER)
// is settled. Their payloads are salts, scrambles and public keys.
func (m *mysqlSplitter) serverAuth(payload []byte) string {
	switch payload[0] {
	case gomysql.OK_HEADER:
		m.finishCommand()
		m.clientPhase = mysqlClientCommand

		return "AuthOK"
	case gomysql.ERR_HEADER:
		m.finishCommand()
		m.clientPhase = mysqlClientCommand

		return formatMySQLErr(payload)
	case gomysql.EOF_HEADER:
		m.clientPhase = mysqlClientAuth

		return formatMySQLAuthSwitch(payload)
	case gomysql.MORE_DATE_HEADER:
		m.clientPhase = mysqlClientAuth

		return formatMySQLAuthMoreData(payload)
	default:
		m.clientPhase = mysqlClientAuth

		return formatMySQLAuthPacket(payload)
	}
}

// serverResponse reads the first packet of a command's answer, which is what
// says whether the rest is a result set, an error, or nothing at all.
func (m *mysqlSplitter) serverResponse(payload []byte) string {
	switch {
	case m.pendingCmd == gomysql.COM_STATISTICS:
		m.finishCommand()

		return fmt.Sprintf("Statistics(%d bytes)", len(payload))
	case m.pendingCmd == gomysql.COM_FIELD_LIST && payload[0] != gomysql.ERR_HEADER:
		// COM_FIELD_LIST answers with bare column definitions and no header.
		m.serverPhase = mysqlServerColumnDefs
		m.defsLeft = -1

		return m.serverDefinition(payload)
	case payload[0] == gomysql.OK_HEADER && m.pendingCmd == gomysql.COM_STMT_PREPARE:
		return m.serverPrepareOK(payload)
	case payload[0] == gomysql.OK_HEADER:
		m.finishCommand()

		return formatMySQLOK(payload)
	case payload[0] == gomysql.ERR_HEADER:
		m.finishCommand()

		return formatMySQLErr(payload)
	case payload[0] == gomysql.LocalInFile_HEADER:
		m.clientPhase = mysqlClientInfile
		m.serverPhase = mysqlServerIdle

		return "LocalInfileRequest " + quote(string(payload[1:]))
	case payload[0] == gomysql.EOF_HEADER && len(payload) < mysqlEOFMaxLen:
		m.finishCommand()

		return formatMySQLResultEnd(payload)
	default:
		return m.serverResultSet(payload)
	}
}

// serverResultSet opens a result set: a length-encoded column count, then that
// many column definitions, then the rows.
func (m *mysqlSplitter) serverResultSet(payload []byte) string {
	count, _, ok := mysqlLenEncInt(payload)
	if !ok {
		return fmt.Sprintf("ResultSet(unreadable column count, %d bytes)", len(payload))
	}

	m.columns = int(count)
	m.defsLeft = int(count)
	m.serverPhase = mysqlServerColumnDefs
	m.binaryRows = m.pendingCmd == gomysql.COM_STMT_EXECUTE || m.pendingCmd == gomysql.COM_STMT_FETCH

	if m.defsLeft == 0 {
		m.serverPhase = mysqlServerRows
		m.pendingDefsEOF = true
	}

	return fmt.Sprintf("ResultSet(%d cols)", m.columns)
}

// serverPrepareOK reads the answer to COM_STMT_PREPARE, which is followed by
// one definition per parameter and then one per column.
func (m *mysqlSplitter) serverPrepareOK(payload []byte) string {
	if len(payload) < mysqlPrepareOKLen {
		m.finishCommand()

		return formatMySQLOK(payload)
	}

	stmtID := uint32(payload[1]) | uint32(payload[2])<<8 | uint32(payload[3])<<16 | uint32(payload[4])<<24
	columns := int(payload[5]) | int(payload[6])<<8
	params := int(payload[7]) | int(payload[8])<<8

	m.defsLeft = columns + params
	m.serverPhase = mysqlServerPrepareDefs

	if m.defsLeft == 0 {
		m.finishCommand()
	}

	return fmt.Sprintf("PrepareOK stmt=%d (%d params, %d cols)", stmtID, params, columns)
}

// serverDefinition reads one column definition, or an EOF closing a run of
// them.
func (m *mysqlSplitter) serverDefinition(payload []byte) string {
	if payload[0] == gomysql.EOF_HEADER && len(payload) < mysqlEOFMaxLen {
		return m.definitionsEnd(payload)
	}

	if m.defsLeft > 0 {
		m.defsLeft--
	}

	if m.defsLeft == 0 {
		if m.serverPhase == mysqlServerPrepareDefs {
			m.finishCommand()
		} else {
			m.serverPhase = mysqlServerRows
			m.pendingDefsEOF = true
		}
	}

	return formatMySQLColumnDefinition(payload, m.opts)
}

// definitionsEnd handles an EOF arriving while definitions are still expected:
// the parameter/column boundary of a prepare, or the end of a COM_FIELD_LIST.
func (m *mysqlSplitter) definitionsEnd(payload []byte) string {
	if m.serverPhase == mysqlServerPrepareDefs {
		if m.defsLeft <= 0 {
			m.finishCommand()
		}

		return formatMySQLResultEnd(payload)
	}

	// A column-definition run cut short, which for COM_FIELD_LIST is the
	// normal ending.
	m.finishCommand()

	return formatMySQLResultEnd(payload)
}

// serverRow reads one result row, or the packet that ends the result set.
func (m *mysqlSplitter) serverRow(payload []byte) string {
	switch {
	case payload[0] == gomysql.ERR_HEADER:
		m.finishCommand()

		return formatMySQLErr(payload)
	case payload[0] == gomysql.EOF_HEADER && len(payload) < mysqlEOFMaxLen:
		if m.pendingDefsEOF && len(payload) <= mysqlTrueEOFMaxLen {
			// The boundary EOF between the definitions and the rows, sent by
			// a server that did not negotiate CLIENT_DEPRECATE_EOF.
			m.pendingDefsEOF = false

			return formatMySQLResultEnd(payload)
		}

		m.finishCommand()

		return formatMySQLResultEnd(payload)
	default:
		m.pendingDefsEOF = false

		return formatMySQLRow(payload, m.columns, m.binaryRows, m.opts)
	}
}

// finishCommand returns the server stream to rest between commands.
func (m *mysqlSplitter) finishCommand() {
	m.serverPhase = mysqlServerIdle
	m.pendingCmd = 0
	m.columns = 0
	m.defsLeft = 0
	m.pendingDefsEOF = false
	m.binaryRows = false
}
