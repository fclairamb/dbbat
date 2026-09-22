package decode

import (
	"fmt"
	"strings"
	"testing"

	gomysql "github.com/go-mysql-org/go-mysql/mysql"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/dump"
)

// feedSplitter pushes one packet through any splitter and returns the rendered
// lines. The per-protocol tests all use it, so the trace shape they assert is
// the one File writes.
func feedSplitter(t *testing.T, split splitter, ns int64, direction byte, data []byte) []string {
	t.Helper()

	msgs, err := split.Feed(&dump.Packet{RelativeNs: ns, Direction: direction, Data: data})
	require.NoError(t, err)

	lines := make([]string, 0, len(msgs))
	for _, msg := range msgs {
		lines = append(lines, msg.String())
	}

	return lines
}

// mysqlPacket wraps a payload in the 3-byte little-endian length and the
// sequence id that frame every MySQL packet.
func mysqlPacket(seq byte, payload []byte) []byte {
	out := make([]byte, 0, mysqlHeaderLen+len(payload))
	out = append(out, byte(len(payload)), byte(len(payload)>>8), byte(len(payload)>>16), seq)

	return append(out, payload...)
}

// mysqlConcat joins byte slices into one payload, which is how these tests
// build a packet body without tripping over a half-filled make.
func mysqlConcat(parts ...[]byte) []byte {
	total := 0
	for _, part := range parts {
		total += len(part)
	}

	out := make([]byte, 0, total)
	for _, part := range parts {
		out = append(out, part...)
	}

	return out
}

// mysqlPackets concatenates several packets into one payload, the way a server
// flushes a whole result set in a single write.
func mysqlPackets(payloads ...[]byte) []byte {
	framed := make([][]byte, 0, len(payloads))
	for i, payload := range payloads {
		framed = append(framed, mysqlPacket(byte(i+1), payload))
	}

	return mysqlConcat(framed...)
}

func mysqlLenencStr(s string) []byte {
	return append([]byte{byte(len(s))}, s...)
}

func mysqlCommandPayload(cmd byte, arg string) []byte {
	return append([]byte{cmd}, arg...)
}

// mysqlColumnDefPayload builds a protocol-41 column definition: six
// length-encoded strings, then a fixed 12-byte tail.
func mysqlColumnDefPayload(name string) []byte {
	var out []byte

	for _, part := range []string{"def", "app", "customers", "customers", name, name} {
		out = append(out, mysqlLenencStr(part)...)
	}

	out = append(out, 0x0c)

	return append(out, make([]byte, 10)...)
}

// mysqlTextRowPayload builds a text-protocol row, with nil standing for NULL.
func mysqlTextRowPayload(values ...*string) []byte {
	var out []byte

	for _, value := range values {
		if value == nil {
			out = append(out, 0xFB)

			continue
		}

		out = append(out, mysqlLenencStr(*value)...)
	}

	return out
}

func mysqlEOFPayload() []byte {
	return []byte{gomysql.EOF_HEADER, 0x00, 0x00, 0x02, 0x00}
}

func mysqlOKPayload() []byte {
	return []byte{gomysql.OK_HEADER, 0x01, 0x00, 0x02, 0x00, 0x00, 0x00}
}

// mysqlResultEndOKPayload is the packet that ends a result set on a server
// which negotiated CLIENT_DEPRECATE_EOF: an OK body wearing the EOF header.
func mysqlResultEndOKPayload() []byte {
	return []byte{gomysql.EOF_HEADER, 0x01, 0x00, 0x02, 0x00, 0x00, 0x00}
}

func mysqlErrPayload(code uint16, state, message string) []byte {
	out := []byte{gomysql.ERR_HEADER, byte(code), byte(code >> 8), '#'}
	out = append(out, state...)

	return append(out, message...)
}

func strptr(s string) *string { return &s }

// TestMySQLSplitter_Trace is the golden test for the whole shape of the output
// over a session that runs a simple query and then quits. It pins the two
// framing cases the splitter exists for — several messages in one packet, and
// one message split across two.
func TestMySQLSplitter_Trace(t *testing.T) {
	t.Parallel()

	split := newMySQLSplitter(Options{})

	got := make([]string, 0, 12)

	got = append(got, feedSplitter(t, split, 0, dump.DirClientToServer,
		mysqlPacket(0, mysqlCommandPayload(gomysql.COM_QUERY, "SELECT id, email\nFROM customers")))...)

	// One server flush carrying the header and both column definitions.
	got = append(got, feedSplitter(t, split, 12*ms, dump.DirServerToClient, mysqlPackets(
		[]byte{0x02},
		mysqlColumnDefPayload("id"),
		mysqlColumnDefPayload("email"),
	))...)

	// The rows arrive split mid-packet: the first feed must yield only what is
	// complete.
	rows := mysqlPackets(
		mysqlEOFPayload(),
		mysqlTextRowPayload(strptr("42"), strptr("alice@example.com")),
		mysqlTextRowPayload(strptr("43"), nil),
		mysqlEOFPayload(),
	)

	cut := len(rows) - 12
	got = append(got, feedSplitter(t, split, 31*ms, dump.DirServerToClient, rows[:cut])...)
	got = append(got, feedSplitter(t, split, 44*ms, dump.DirServerToClient, rows[cut:])...)

	got = append(got, feedSplitter(t, split, 1400*ms, dump.DirClientToServer,
		mysqlPacket(0, []byte{gomysql.COM_QUIT}))...)

	assert.Equal(t, []string{
		`0s C> COM_QUERY "SELECT id, email FROM customers"`,
		`12ms <S ResultSet(2 cols)`,
		`12ms <S ColumnDefinition`,
		`12ms <S ColumnDefinition`,
		`31ms <S EOF warnings=0 status=0x0002`,
		`31ms <S Row(2 cols)`,
		`44ms <S Row(2 cols)`,
		`44ms <S EOF warnings=0 status=0x0002`,
		`1.4s C> COM_QUIT`,
	}, got)

	assert.NotContains(t, strings.Join(got, "\n"), "alice@example.com")
}

// TestMySQLSplitter_RedactsByDefault is the security-relevant default: a
// capture holds customer rows, so a trace that can be pasted into a bug report
// must not.
func TestMySQLSplitter_RedactsByDefault(t *testing.T) {
	t.Parallel()

	query := mysqlPacket(0, mysqlCommandPayload(gomysql.COM_QUERY, "SELECT * FROM customers"))
	answer := mysqlPackets(
		[]byte{0x02},
		mysqlColumnDefPayload("email"),
		mysqlColumnDefPayload("iban"),
		mysqlTextRowPayload(strptr("alice@example.com"), nil),
		mysqlResultEndOKPayload(),
	)

	t.Run("default", func(t *testing.T) {
		t.Parallel()

		split := newMySQLSplitter(Options{})
		lines := feedSplitter(t, split, 0, dump.DirClientToServer, query)
		lines = append(lines, feedSplitter(t, split, ms, dump.DirServerToClient, answer)...)

		joined := strings.Join(lines, "\n")
		assert.NotContains(t, joined, "alice@example.com")
		assert.NotContains(t, joined, "iban")
		assert.Contains(t, joined, "ResultSet(2 cols)")
		assert.Contains(t, joined, "Row(2 cols)")
	})

	t.Run("rows", func(t *testing.T) {
		t.Parallel()

		split := newMySQLSplitter(Options{ShowRows: true})
		lines := feedSplitter(t, split, 0, dump.DirClientToServer, query)
		lines = append(lines, feedSplitter(t, split, ms, dump.DirServerToClient, answer)...)

		joined := strings.Join(lines, "\n")
		assert.Contains(t, joined, `Row(2 cols) ["alice@example.com", NULL]`)
		assert.Contains(t, joined, "ColumnDefinition email")
		assert.Contains(t, joined, "ColumnDefinition iban")
	})
}

// TestMySQLSplitter_AuthenticationNeverDumped pins the one redaction --rows
// does not lift. dbbat's own tap starts after authentication, but a capture
// taken any other way still must not print a scramble.
func TestMySQLSplitter_AuthenticationNeverDumped(t *testing.T) {
	t.Parallel()

	split := newMySQLSplitter(Options{ShowRows: true})

	greeting := mysqlConcat(
		[]byte{0x0A},
		[]byte("8.4.0-dbbat\x00"),
		[]byte{0x2A, 0x00, 0x00, 0x00},
		[]byte("server-salt\x00"),
	)

	lines := feedSplitter(t, split, 0, dump.DirServerToClient, mysqlPacket(0, greeting))

	caps := gomysql.CLIENT_CONNECT_WITH_DB | gomysql.CLIENT_PLUGIN_AUTH_LENENC_CLIENT_DATA
	header := make([]byte, mysqlSSLRequestLen)
	header[0] = byte(caps)
	header[1] = byte(caps >> 8)
	header[2] = byte(caps >> 16)
	header[3] = byte(caps >> 24)

	response := mysqlConcat(
		header,
		[]byte("alice\x00"),
		mysqlLenencStr("client-scramble"),
		[]byte("app\x00"),
	)

	lines = append(lines, feedSplitter(t, split, ms, dump.DirClientToServer, mysqlPacket(1, response))...)

	authSwitch := mysqlConcat(
		[]byte{gomysql.EOF_HEADER},
		[]byte("caching_sha2_password\x00"),
		[]byte("fresh-salt\x00"),
	)

	lines = append(lines, feedSplitter(t, split, 2*ms, dump.DirServerToClient, mysqlPacket(2, authSwitch))...)
	lines = append(lines, feedSplitter(t, split, 3*ms, dump.DirClientToServer,
		mysqlPacket(3, []byte("client-proof")))...)
	lines = append(lines, feedSplitter(t, split, 4*ms, dump.DirServerToClient,
		mysqlPacket(4, mysqlConcat([]byte{gomysql.MORE_DATE_HEADER}, []byte("server-public-key"))))...)
	lines = append(lines, feedSplitter(t, split, 5*ms, dump.DirClientToServer,
		mysqlPacket(5, []byte("encrypted-password")))...)
	lines = append(lines, feedSplitter(t, split, 6*ms, dump.DirServerToClient,
		mysqlPacket(6, mysqlOKPayload()))...)

	joined := strings.Join(lines, "\n")

	assert.Contains(t, joined, `InitialHandshake server="8.4.0-dbbat" connection=42 (salt redacted)`)
	assert.Contains(t, joined, `HandshakeResponse user="alice" db="app" (auth redacted)`)
	assert.Contains(t, joined, `AuthSwitchRequest plugin="caching_sha2_password" (data redacted)`)
	assert.Contains(t, joined, "AuthResponse(12 bytes, redacted)")
	assert.Contains(t, joined, "AuthMoreData(17 bytes, redacted)")
	assert.Contains(t, joined, "AuthOK")

	for _, secret := range []string{
		"server-salt", "client-scramble", "fresh-salt",
		"client-proof", "server-public-key", "encrypted-password",
	} {
		assert.NotContains(t, joined, secret)
	}
}

// TestMySQLSplitter_Prepared covers the second framing shape: a prepare answer
// counts its definitions instead of ending on a column count, and the binary
// rows that follow an execute are never printed.
func TestMySQLSplitter_Prepared(t *testing.T) {
	t.Parallel()

	split := newMySQLSplitter(Options{ShowRows: true})

	got := feedSplitter(t, split, 0, dump.DirClientToServer,
		mysqlPacket(0, mysqlCommandPayload(gomysql.COM_STMT_PREPARE, "SELECT email FROM customers WHERE id = ?")))

	prepareOK := []byte{0x00, 0x07, 0x00, 0x00, 0x00, 0x01, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00}

	got = append(got, feedSplitter(t, split, 5*ms, dump.DirServerToClient, mysqlPackets(
		prepareOK,
		mysqlColumnDefPayload("id"),
		mysqlEOFPayload(),
		mysqlColumnDefPayload("email"),
		mysqlEOFPayload(),
	))...)

	execute := []byte{gomysql.COM_STMT_EXECUTE, 0x07, 0x00, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00}
	got = append(got, feedSplitter(t, split, 9*ms, dump.DirClientToServer, mysqlPacket(0, execute))...)

	binaryRow := mysqlConcat([]byte{0x00, 0x00, 0x11}, []byte("alice@example.com"))

	got = append(got, feedSplitter(t, split, 11*ms, dump.DirServerToClient, mysqlPackets(
		[]byte{0x01},
		mysqlColumnDefPayload("email"),
		binaryRow,
		mysqlResultEndOKPayload(),
	))...)

	assert.Equal(t, []string{
		`0s C> COM_STMT_PREPARE "SELECT email FROM customers WHERE id = ?"`,
		`5ms <S PrepareOK stmt=7 (1 params, 1 cols)`,
		`5ms <S ColumnDefinition id`,
		`5ms <S EOF warnings=0 status=0x0002`,
		`5ms <S ColumnDefinition email`,
		`5ms <S EOF warnings=0 status=0x0002`,
		`9ms C> COM_STMT_EXECUTE stmt=7`,
		`11ms <S ResultSet(1 cols)`,
		`11ms <S ColumnDefinition email`,
		`11ms <S Row(1 cols, binary)`,
		`11ms <S OK affected=1 insertId=0 status=0x0002 warnings=0`,
	}, got)

	// --rows is on and the row still does not reach the trace: a binary row is
	// typed by the prepare, which a capture starting mid-session has not seen.
	assert.NotContains(t, strings.Join(got, "\n"), "alice@example.com")
}

// TestMySQLSplitter_Error checks a failed statement reads as one.
func TestMySQLSplitter_Error(t *testing.T) {
	t.Parallel()

	split := newMySQLSplitter(Options{})

	lines := feedSplitter(t, split, 0, dump.DirClientToServer,
		mysqlPacket(0, mysqlCommandPayload(gomysql.COM_QUERY, "SELECT * FROM nope")))
	lines = append(lines, feedSplitter(t, split, ms, dump.DirServerToClient,
		mysqlPacket(1, mysqlErrPayload(1146, "42S02", "Table 'app.nope' doesn't exist")))...)

	assert.Equal(t, []string{
		`0s C> COM_QUERY "SELECT * FROM nope"`,
		`1ms <S ERR 1146 (42S02): Table 'app.nope' doesn't exist`,
	}, lines)
}

// TestMySQLSplitter_OutOfSync checks a truncated capture is reported rather
// than silently producing nonsense: DBB_DUMP_MAX_SIZE drops whole packets.
func TestMySQLSplitter_OutOfSync(t *testing.T) {
	t.Parallel()

	split := newMySQLSplitter(Options{})

	_, err := split.Feed(&dump.Packet{
		Direction: dump.DirClientToServer,
		Data:      mysqlPacket(0, nil),
	})
	require.ErrorIs(t, err, ErrOutOfSync)
}

// TestMySQLSplitter_PartialMessagesYieldNothing checks a packet that completes
// no message produces no line rather than an error.
func TestMySQLSplitter_PartialMessagesYieldNothing(t *testing.T) {
	t.Parallel()

	split := newMySQLSplitter(Options{})

	query := mysqlPacket(0, mysqlCommandPayload(gomysql.COM_QUERY, "SELECT 1"))

	assert.Empty(t, feedSplitter(t, split, 0, dump.DirClientToServer, query[:2]))
	assert.Empty(t, feedSplitter(t, split, ms, dump.DirClientToServer, query[2:len(query)-1]))
	assert.Equal(t,
		[]string{`2ms C> COM_QUERY "SELECT 1"`},
		feedSplitter(t, split, 2*ms, dump.DirClientToServer, query[len(query)-1:]),
	)
}

// TestMySQLSplitter_LargePayloadContinuation exercises the one framing rule
// that has no counterpart in the other four protocols: a payload of exactly
// 0xFFFFFF bytes is not a message, it is a chunk, and the next packet carries
// the rest. The rule fires only at that exact length, so nothing short of a
// real 16MB packet proves the branch is taken — a TCP-level split at an
// arbitrary offset exercises the buffering, not this.
//
// Two full chunks are used rather than one, because a splitter that handled
// the first continuation and then treated the second as a fresh message would
// pass a single-chunk test.
func TestMySQLSplitter_LargePayloadContinuation(t *testing.T) {
	t.Parallel()

	// COM_STMT_SEND_LONG_DATA is the command to build this out of: its
	// rendering reports the payload's byte count without echoing it, so the
	// assertion stays one line while still proving every byte arrived.
	head := []byte{gomysql.COM_STMT_SEND_LONG_DATA, 7, 0, 0, 0, 0, 0}

	const tailLen = 123

	split := newMySQLSplitter(Options{})

	assert.Empty(t, feedSplitter(t, split, 0, dump.DirClientToServer,
		mysqlPacket(0, mysqlConcat(head, make([]byte, mysqlMaxPayload-len(head))))))
	assert.Empty(t, feedSplitter(t, split, ms, dump.DirClientToServer,
		mysqlPacket(1, make([]byte, mysqlMaxPayload))))

	// The short packet is what ends the run, and the message is timed by it.
	lines := feedSplitter(t, split, 2*ms, dump.DirClientToServer,
		mysqlPacket(2, make([]byte, tailLen)))

	// Two full chunks plus the tail, less the command byte, the statement id
	// and the parameter id.
	const wantBytes = 2*mysqlMaxPayload + tailLen - 7

	assert.Equal(t,
		[]string{fmt.Sprintf("2ms C> COM_STMT_SEND_LONG_DATA stmt=7 (%d bytes)", wantBytes)},
		lines,
	)
}

// TestMySQLSplitter_TLSStopsDecoding mirrors the PostgreSQL case: a capture
// taken below TLS stops at the upgrade rather than reporting records as
// protocol messages.
func TestMySQLSplitter_TLSStopsDecoding(t *testing.T) {
	t.Parallel()

	split := newMySQLSplitter(Options{})

	greeting := mysqlConcat([]byte{0x0A}, []byte("8.4.0\x00"), []byte{0x01, 0x00, 0x00, 0x00})

	lines := feedSplitter(t, split, 0, dump.DirServerToClient, mysqlPacket(0, greeting))
	lines = append(lines, feedSplitter(t, split, ms, dump.DirClientToServer,
		mysqlPacket(1, make([]byte, mysqlSSLRequestLen)))...)
	lines = append(lines, feedSplitter(t, split, 2*ms, dump.DirClientToServer,
		[]byte{0x16, 0x03, 0x01, 0x00, 0x42, 0x01})...)

	assert.Equal(t, []string{
		`0s <S InitialHandshake server="8.4.0" connection=1 (salt redacted)`,
		`1ms C> SSLRequest (accepted, rest of the capture is TLS)`,
	}, lines)
}
