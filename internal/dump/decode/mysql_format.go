package decode

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"strings"

	gomysql "github.com/go-mysql-org/go-mysql/mysql"
)

// mysqlCommandNames names every command byte the wire protocol defines, so a
// trace says COM_STMT_EXECUTE rather than 0x17.
var mysqlCommandNames = map[byte]string{
	gomysql.COM_SLEEP:                              "COM_SLEEP",
	gomysql.COM_QUIT:                               "COM_QUIT",
	gomysql.COM_INIT_DB:                            "COM_INIT_DB",
	gomysql.COM_QUERY:                              "COM_QUERY",
	gomysql.COM_FIELD_LIST:                         "COM_FIELD_LIST",
	gomysql.COM_CREATE_DB:                          "COM_CREATE_DB",
	gomysql.COM_DROP_DB:                            "COM_DROP_DB",
	gomysql.COM_REFRESH:                            "COM_REFRESH",
	gomysql.COM_SHUTDOWN:                           "COM_SHUTDOWN",
	gomysql.COM_STATISTICS:                         "COM_STATISTICS",
	gomysql.COM_PROCESS_INFO:                       "COM_PROCESS_INFO",
	gomysql.COM_CONNECT:                            "COM_CONNECT",
	gomysql.COM_PROCESS_KILL:                       "COM_PROCESS_KILL",
	gomysql.COM_DEBUG:                              "COM_DEBUG",
	gomysql.COM_PING:                               "COM_PING",
	gomysql.COM_TIME:                               "COM_TIME",
	gomysql.COM_DELAYED_INSERT:                     "COM_DELAYED_INSERT",
	gomysql.COM_CHANGE_USER:                        "COM_CHANGE_USER",
	gomysql.COM_BINLOG_DUMP:                        "COM_BINLOG_DUMP",
	gomysql.COM_TABLE_DUMP:                         "COM_TABLE_DUMP",
	gomysql.COM_CONNECT_OUT:                        "COM_CONNECT_OUT",
	gomysql.COM_REGISTER_SLAVE:                     "COM_REGISTER_SLAVE",
	gomysql.COM_STMT_PREPARE:                       "COM_STMT_PREPARE",
	gomysql.COM_STMT_EXECUTE:                       "COM_STMT_EXECUTE",
	gomysql.COM_STMT_SEND_LONG_DATA:                "COM_STMT_SEND_LONG_DATA",
	gomysql.COM_STMT_CLOSE:                         "COM_STMT_CLOSE",
	gomysql.COM_STMT_RESET:                         "COM_STMT_RESET",
	gomysql.COM_SET_OPTION:                         "COM_SET_OPTION",
	gomysql.COM_STMT_FETCH:                         "COM_STMT_FETCH",
	gomysql.COM_DAEMON:                             "COM_DAEMON",
	gomysql.COM_BINLOG_DUMP_GTID:                   "COM_BINLOG_DUMP_GTID",
	gomysql.COM_RESET_CONNECTION:                   "COM_RESET_CONNECTION",
	gomysql.COM_CLONE:                              "COM_CLONE",
	gomysql.COM_SUBSCRIBE_GROUP_REPLICATION_STREAM: "COM_SUBSCRIBE_GROUP_REPLICATION_STREAM",
}

// mysqlCommandName names a command byte, falling back to its hex code.
func mysqlCommandName(cmd byte) string {
	if name, ok := mysqlCommandNames[cmd]; ok {
		return name
	}

	return fmt.Sprintf("COM_0x%02x", cmd)
}

// formatMySQLCommand renders a command packet. Statement text is printed —
// reading it is the point of the tool, and the queries table holds it anyway.
// Prepared-statement parameters are not: they are binary and typed by the
// prepare, which a capture starting mid-session has not seen.
func formatMySQLCommand(cmd byte, args []byte) string {
	name := mysqlCommandName(cmd)

	switch cmd {
	case gomysql.COM_QUERY, gomysql.COM_STMT_PREPARE:
		return name + " " + quote(string(args))
	case gomysql.COM_INIT_DB, gomysql.COM_CREATE_DB, gomysql.COM_DROP_DB, gomysql.COM_FIELD_LIST:
		return name + " " + quote(string(args))
	case gomysql.COM_STMT_EXECUTE, gomysql.COM_STMT_CLOSE, gomysql.COM_STMT_RESET:
		return fmt.Sprintf("%s stmt=%s", name, mysqlUint32(args))
	case gomysql.COM_STMT_FETCH:
		return fmt.Sprintf("%s stmt=%s rows=%s", name, mysqlUint32(args), mysqlUint32(sliceFrom(args, 4)))
	case gomysql.COM_STMT_SEND_LONG_DATA:
		return fmt.Sprintf("%s stmt=%s (%d bytes)", name, mysqlUint32(args), maxInt(len(args)-6, 0))
	case gomysql.COM_PROCESS_KILL:
		return fmt.Sprintf("%s connection=%s", name, mysqlUint32(args))
	case gomysql.COM_CHANGE_USER:
		user, _, _ := mysqlNullString(args)

		return fmt.Sprintf("%s user=%s (auth redacted)", name, quote(user))
	default:
		return name
	}
}

// formatMySQLGreeting renders the server's initial handshake packet. The
// scramble it carries is an authentication payload and never reaches a trace.
func formatMySQLGreeting(payload []byte) string {
	version, n, ok := mysqlNullString(payload[1:])
	if !ok {
		return "InitialHandshake (unreadable)"
	}

	rest := sliceFrom(payload[1:], n)
	if len(rest) < 4 {
		return "InitialHandshake server=" + quote(version) + " (salt redacted)"
	}

	return fmt.Sprintf("InitialHandshake server=%s connection=%d (salt redacted)",
		quote(version), binary.LittleEndian.Uint32(rest[:4]))
}

// mysqlCapabilities reads the client capability flags off a handshake response.
func mysqlCapabilities(payload []byte) uint32 {
	if len(payload) < 4 {
		return 0
	}

	return binary.LittleEndian.Uint32(payload[:4])
}

// formatMySQLHandshakeResponse prints the connection's identity — user and
// database — the way the PostgreSQL splitter prints a StartupMessage. The auth
// response that sits between them is never printed.
func formatMySQLHandshakeResponse(payload []byte, capabilities uint32) string {
	const headerLen = 32 // capabilities, max packet size, charset, 23 filler

	if len(payload) <= headerLen {
		return "HandshakeResponse (unreadable, auth redacted)"
	}

	body := payload[headerLen:]

	user, used, ok := mysqlNullString(body)
	if !ok {
		return "HandshakeResponse (unreadable, auth redacted)"
	}

	head := "HandshakeResponse user=" + quote(user)

	database, ok := mysqlHandshakeDatabase(sliceFrom(body, used), capabilities)
	if ok {
		head += " db=" + quote(database)
	}

	return head + " (auth redacted)"
}

// mysqlHandshakeDatabase steps over the auth response — whose length is encoded
// three different ways depending on the negotiated capabilities — to reach the
// initial database name.
func mysqlHandshakeDatabase(body []byte, capabilities uint32) (string, bool) {
	if capabilities&gomysql.CLIENT_CONNECT_WITH_DB == 0 {
		return "", false
	}

	var used int

	switch {
	case capabilities&gomysql.CLIENT_PLUGIN_AUTH_LENENC_CLIENT_DATA != 0:
		length, n, ok := mysqlLenEncInt(body)
		if !ok {
			return "", false
		}

		used = n + int(length)
	case capabilities&gomysql.CLIENT_SECURE_CONNECTION != 0:
		if len(body) == 0 {
			return "", false
		}

		used = 1 + int(body[0])
	default:
		_, n, ok := mysqlNullString(body)
		if !ok {
			return "", false
		}

		used = n
	}

	database, _, ok := mysqlNullString(sliceFrom(body, used))

	return database, ok
}

// formatMySQLAuthSwitch names the plugin the server wants used instead. The
// plugin data that follows is a fresh scramble and stays out of the trace.
func formatMySQLAuthSwitch(payload []byte) string {
	plugin, _, ok := mysqlNullString(payload[1:])
	if !ok {
		return "AuthSwitchRequest (redacted)"
	}

	return "AuthSwitchRequest plugin=" + quote(plugin) + " (data redacted)"
}

// formatMySQLOK renders an OK packet, including the one that wears the EOF
// header under CLIENT_DEPRECATE_EOF.
func formatMySQLOK(payload []byte) string {
	body := payload[1:]

	affected, n, ok := mysqlLenEncInt(body)
	if !ok {
		return "OK"
	}

	insertID, m, ok := mysqlLenEncInt(sliceFrom(body, n))
	if !ok {
		return fmt.Sprintf("OK affected=%d", affected)
	}

	rest := sliceFrom(body, n+m)
	if len(rest) < 4 {
		return fmt.Sprintf("OK affected=%d insertId=%d", affected, insertID)
	}

	return fmt.Sprintf("OK affected=%d insertId=%d status=0x%04x warnings=%d",
		affected, insertID, binary.LittleEndian.Uint16(rest[:2]), binary.LittleEndian.Uint16(rest[2:4]))
}

// formatMySQLErr renders an error packet. The server's own message is printed,
// as the PostgreSQL splitter prints an ErrorResponse.
func formatMySQLErr(payload []byte) string {
	body := payload[1:]
	if len(body) < 2 {
		return "ERR"
	}

	code := binary.LittleEndian.Uint16(body[:2])
	rest := body[2:]

	state := ""

	if len(rest) >= 6 && rest[0] == '#' {
		state = " (" + string(rest[1:6]) + ")"
		rest = rest[6:]
	}

	return fmt.Sprintf("ERR %d%s: %s", code, state, collapse(string(rest)))
}

// formatMySQLResultEnd renders whichever of the two shapes ends a result set.
// Telling them apart by length rather than by the negotiated capabilities is
// what lets a capture that starts after authentication be read at all.
func formatMySQLResultEnd(payload []byte) string {
	if len(payload) > mysqlTrueEOFMaxLen {
		return formatMySQLOK(payload)
	}

	if len(payload) < 5 {
		return "EOF"
	}

	return fmt.Sprintf("EOF warnings=%d status=0x%04x",
		binary.LittleEndian.Uint16(payload[1:3]), binary.LittleEndian.Uint16(payload[3:5]))
}

// formatMySQLColumnDefinition prints the column's name only when --rows opted
// into content, exactly as RowDescription does on PostgreSQL.
func formatMySQLColumnDefinition(payload []byte, opts Options) string {
	if !opts.ShowRows {
		return "ColumnDefinition"
	}

	name, ok := mysqlColumnName(payload)
	if !ok {
		return "ColumnDefinition"
	}

	return "ColumnDefinition " + name
}

// mysqlColumnName pulls the column's alias out of a protocol-41 column
// definition: the fifth length-encoded string, after catalog, schema, table
// and the original table name.
func mysqlColumnName(payload []byte) (string, bool) {
	const nameIndex = 4

	rest := payload

	for i := 0; i <= nameIndex; i++ {
		value, _, n, ok := mysqlLenEncString(rest)
		if !ok {
			return "", false
		}

		if i == nameIndex {
			return string(value), true
		}

		rest = sliceFrom(rest, n)
	}

	return "", false
}

// formatMySQLRow is the redaction that matters most: a capture is full of these
// and every one of them is customer data.
//
// A binary-protocol row (the answer to COM_STMT_EXECUTE) is counted and never
// printed: its values are typed by the column definitions of the prepare, which
// a capture that starts mid-session has not recorded.
func formatMySQLRow(payload []byte, columns int, binaryRow bool, opts Options) string {
	if binaryRow {
		return fmt.Sprintf("Row(%d cols, binary)", columns)
	}

	head := fmt.Sprintf("Row(%d cols)", columns)

	if !opts.ShowRows {
		return head
	}

	return head + " " + formatMySQLTextValues(payload, columns)
}

// formatMySQLTextValues decodes a text-protocol row: one length-encoded string
// per column, with 0xFB standing for NULL.
func formatMySQLTextValues(payload []byte, columns int) string {
	rendered := make([]string, 0, columns)
	rest := payload

	for range columns {
		value, isNull, n, ok := mysqlLenEncString(rest)
		if !ok {
			rendered = append(rendered, "?")

			break
		}

		if isNull {
			rendered = append(rendered, "NULL")
		} else {
			rendered = append(rendered, quote(truncate(string(value))))
		}

		rest = sliceFrom(rest, n)
	}

	return "[" + strings.Join(rendered, ", ") + "]"
}

// mysqlLenEncInt reads a length-encoded integer. ok is false when the buffer is
// too short or the byte is the NULL marker, which is not an integer.
func mysqlLenEncInt(b []byte) (uint64, int, bool) {
	if len(b) == 0 {
		return 0, 0, false
	}

	switch first := b[0]; {
	case first < 0xFB:
		return uint64(first), 1, true
	case first == 0xFC && len(b) >= 3:
		return uint64(binary.LittleEndian.Uint16(b[1:3])), 3, true
	case first == 0xFD && len(b) >= 4:
		return uint64(b[1]) | uint64(b[2])<<8 | uint64(b[3])<<16, 4, true
	case first == 0xFE && len(b) >= 9:
		return binary.LittleEndian.Uint64(b[1:9]), 9, true
	default:
		return 0, 0, false
	}
}

// mysqlLenEncString reads one length-encoded string, reporting the NULL marker
// separately from a decoding failure.
func mysqlLenEncString(b []byte) ([]byte, bool, int, bool) {
	if len(b) == 0 {
		return nil, false, 0, false
	}

	if b[0] == 0xFB {
		return nil, true, 1, true
	}

	length, n, ok := mysqlLenEncInt(b)
	if !ok {
		return nil, false, 0, false
	}

	end := n + int(length)
	if end > len(b) {
		return nil, false, 0, false
	}

	return b[n:end], false, end, true
}

// mysqlNullString reads a NUL-terminated string and how many bytes it consumed,
// terminator included.
func mysqlNullString(b []byte) (string, int, bool) {
	idx := bytes.IndexByte(b, 0)
	if idx < 0 {
		return "", 0, false
	}

	return string(b[:idx]), idx + 1, true
}

// mysqlUint32 renders a little-endian uint32 at the front of b, or "?" when it
// is not there.
func mysqlUint32(b []byte) string {
	if len(b) < 4 {
		return "?"
	}

	return fmt.Sprintf("%d", binary.LittleEndian.Uint32(b[:4]))
}

// sliceFrom returns b from offset n, or nothing when n runs past the end.
func sliceFrom(b []byte, n int) []byte {
	if n < 0 || n > len(b) {
		return nil
	}

	return b[n:]
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}

	return b
}
