package decode

import (
	"encoding/binary"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/dump"
)

// tdsPacket wraps a body in the 8-byte TDS header, setting the EOM bit on the
// packet that ends the message.
func tdsPacket(msgType byte, eom bool, body []byte) []byte {
	status := byte(0)
	if eom {
		status = tdsStatusEOM
	}

	total := tdsHeaderLen + len(body)
	header := []byte{msgType, status, byte(total >> 8), byte(total), 0, 0, 1, 0}

	return mysqlConcat(header, body)
}

// tdsUCS2 encodes text the way TDS spells every string on the wire.
func tdsUCS2(s string) []byte {
	units := utf16.Encode([]rune(s))
	out := make([]byte, 0, len(units)*2)

	for _, unit := range units {
		out = binary.LittleEndian.AppendUint16(out, unit)
	}

	return out
}

func tdsUint16(values ...uint16) []byte {
	out := make([]byte, 0, 2*len(values))
	for _, value := range values {
		out = binary.LittleEndian.AppendUint16(out, value)
	}

	return out
}

func tdsUint32(value uint32) []byte {
	return binary.LittleEndian.AppendUint32(make([]byte, 0, 4), value)
}

func tdsUint64(value uint64) []byte {
	return binary.LittleEndian.AppendUint64(make([]byte, 0, 8), value)
}

func tdsBVarchar(s string) []byte {
	return mysqlConcat([]byte{byte(len([]rune(s)))}, tdsUCS2(s))
}

func tdsUSVarchar(s string) []byte {
	return mysqlConcat(tdsUint16(uint16(len([]rune(s)))), tdsUCS2(s))
}

// tdsAllHeaders builds the ALL_HEADERS block a request carries: a total length
// counting itself, then one transaction-descriptor header.
func tdsAllHeaders() []byte {
	header := mysqlConcat(tdsUint32(18), tdsUint16(0x0002), tdsUint64(0), tdsUint32(1))

	return mysqlConcat(tdsUint32(uint32(4+len(header))), header)
}

// tdsNVarCharColumn / tdsIntColumn build the two COLMETADATA entries the tests
// use: a USHORT-framed text column and a BYTE-framed nullable int.
func tdsNVarCharColumn(name string) []byte {
	return mysqlConcat(
		tdsUint32(0), tdsUint16(0), // user type, flags
		[]byte{tdsTypeNVarChar}, tdsUint16(8000), make([]byte, tdsCollationLen),
		tdsBVarchar(name),
	)
}

func tdsIntColumn(name string) []byte {
	return mysqlConcat(
		tdsUint32(0), tdsUint16(0),
		[]byte{tdsTypeIntN, 4},
		tdsBVarchar(name),
	)
}

func tdsColMetadata(columns ...[]byte) []byte {
	return mysqlConcat(append([][]byte{{tdsTokenColMetadata}, tdsUint16(uint16(len(columns)))}, columns...)...)
}

func tdsNVarCharValue(s string) []byte {
	encoded := tdsUCS2(s)

	return mysqlConcat(tdsUint16(uint16(len(encoded))), encoded)
}

func tdsNullNVarCharValue() []byte {
	return tdsUint16(0xFFFF)
}

func tdsIntValue(value int32) []byte {
	return mysqlConcat([]byte{4}, tdsUint32(uint32(value)))
}

func tdsRow(values ...[]byte) []byte {
	return mysqlConcat(append([][]byte{{tdsTokenRow}}, values...)...)
}

func tdsDone(rows uint64) []byte {
	return mysqlConcat([]byte{tdsTokenDone}, tdsUint16(tdsDoneCount, 0), tdsUint64(rows))
}

func tdsErrorToken(number uint32, class byte, message string) []byte {
	body := mysqlConcat(
		tdsUint32(number), []byte{1, class},
		tdsUSVarchar(message), tdsBVarchar("sql01"), tdsBVarchar(""), tdsUint32(1),
	)

	return mysqlConcat([]byte{tdsTokenError}, tdsUint16(uint16(len(body))), body)
}

func tdsEnvChangeToken() []byte {
	body := []byte{1, 0, 0}

	return mysqlConcat([]byte{tdsTokenEnvChange}, tdsUint16(uint16(len(body))), body)
}

// tdsPreloginPayload builds the option list both peers open with: a table of
// token/offset/length triples closed by 0xFF, then the option data.
func tdsPreloginPayload(encryption byte) []byte {
	const tableLen = 11 // two entries plus the terminator

	table := mysqlConcat(
		[]byte{0x00}, tdsBigUint16(tableLen), tdsBigUint16(6),
		[]byte{0x01}, tdsBigUint16(tableLen+6), tdsBigUint16(1),
		[]byte{0xFF},
	)

	return mysqlConcat(table, make([]byte, 6), []byte{encryption})
}

func tdsBigUint16(value uint16) []byte {
	return binary.BigEndian.AppendUint16(make([]byte, 0, 2), value)
}

// tdsLogin7Payload builds a LOGIN7 message with the four printable identity
// fields and a password that must never reach the trace.
func tdsLogin7Payload(host, user, password, app, database string) []byte {
	payload := make([]byte, tdsLogin7FixedLen)

	pairs := []struct {
		index int
		value string
	}{
		{tdsPairHostName, host},
		{tdsPairUserName, user},
		{2, password},
		{tdsPairAppName, app},
		{tdsPairDatabase, database},
	}

	blobs := make([][]byte, 0, len(pairs))
	offset := tdsLogin7FixedLen

	for _, pair := range pairs {
		encoded := tdsUCS2(pair.value)
		pos := tdsLogin7OffsetTable + pair.index*4

		binary.LittleEndian.PutUint16(payload[pos:pos+2], uint16(offset))
		binary.LittleEndian.PutUint16(payload[pos+2:pos+4], uint16(len([]rune(pair.value))))

		blobs = append(blobs, encoded)
		offset += len(encoded)
	}

	binary.LittleEndian.PutUint32(payload[:4], uint32(offset))

	return mysqlConcat(append([][]byte{payload}, blobs...)...)
}

// TestMSSQLSplitter_Trace is the golden test for the whole shape of the
// output. It pins the framing cases the splitter exists for: a request split
// across two TDS packets by the EOM bit, and a response split across two TCP
// payloads mid-packet.
func TestMSSQLSplitter_Trace(t *testing.T) {
	t.Parallel()

	split := newMSSQLSplitter(Options{})

	batch := mysqlConcat(tdsAllHeaders(), tdsUCS2("SELECT id, email\nFROM customers"))
	cut := len(batch) / 2

	got := feedSplitter(t, split, 0, dump.DirClientToServer, tdsPacket(tdsTypeSQLBatch, false, batch[:cut]))
	got = append(got, feedSplitter(t, split, 12*ms, dump.DirClientToServer,
		tdsPacket(tdsTypeSQLBatch, true, batch[cut:]))...)

	response := tdsPacket(tdsTypeReply, true, mysqlConcat(
		tdsColMetadata(tdsIntColumn("id"), tdsNVarCharColumn("email")),
		tdsRow(tdsIntValue(42), tdsNVarCharValue("alice@example.com")),
		tdsRow(tdsIntValue(43), tdsNullNVarCharValue()),
		tdsDone(2),
	))

	// Split mid-packet: the first feed completes nothing.
	tcpCut := len(response) - 20
	got = append(got, feedSplitter(t, split, 31*ms, dump.DirServerToClient, response[:tcpCut])...)
	got = append(got, feedSplitter(t, split, 44*ms, dump.DirServerToClient, response[tcpCut:])...)

	assert.Equal(t, []string{
		`12ms C> SQLBatch "SELECT id, email FROM customers"`,
		`44ms <S ColMetaData(2 cols)`,
		`44ms <S Row(2 cols)`,
		`44ms <S Row(2 cols)`,
		`44ms <S Done status=0x0010 rows=2`,
	}, got)

	assert.NotContains(t, strings.Join(got, "\n"), "alice@example.com")
}

// TestMSSQLSplitter_RedactsByDefault is the security-relevant default: a
// capture holds customer rows, so a trace that can be pasted into a bug report
// must not.
func TestMSSQLSplitter_RedactsByDefault(t *testing.T) {
	t.Parallel()

	batch := tdsPacket(tdsTypeSQLBatch, true, mysqlConcat(tdsAllHeaders(), tdsUCS2("SELECT * FROM customers")))
	response := tdsPacket(tdsTypeReply, true, mysqlConcat(
		tdsEnvChangeToken(),
		tdsColMetadata(tdsNVarCharColumn("email"), tdsNVarCharColumn("iban")),
		tdsRow(tdsNVarCharValue("alice@example.com"), tdsNullNVarCharValue()),
		tdsDone(1),
	))

	t.Run("default", func(t *testing.T) {
		t.Parallel()

		split := newMSSQLSplitter(Options{})
		lines := feedSplitter(t, split, 0, dump.DirClientToServer, batch)
		lines = append(lines, feedSplitter(t, split, ms, dump.DirServerToClient, response)...)

		assert.Equal(t, []string{
			`0s C> SQLBatch "SELECT * FROM customers"`,
			`1ms <S EnvChange(type=1)`,
			`1ms <S ColMetaData(2 cols)`,
			`1ms <S Row(2 cols)`,
			`1ms <S Done status=0x0010 rows=1`,
		}, lines)

		joined := strings.Join(lines, "\n")
		assert.NotContains(t, joined, "alice@example.com")
		assert.NotContains(t, joined, "iban")
	})

	t.Run("rows", func(t *testing.T) {
		t.Parallel()

		split := newMSSQLSplitter(Options{ShowRows: true})
		lines := feedSplitter(t, split, 0, dump.DirClientToServer, batch)
		lines = append(lines, feedSplitter(t, split, ms, dump.DirServerToClient, response)...)

		joined := strings.Join(lines, "\n")
		assert.Contains(t, joined, `ColMetaData(2 cols) [email nvarchar, iban nvarchar]`)
		assert.Contains(t, joined, `Row(2 cols) ["alice@example.com", NULL]`)
	})
}

// TestMSSQLSplitter_RPC covers the second request shape: the statement and its
// parameter declaration are printed, and the argument values that follow them
// are counted.
func TestMSSQLSplitter_RPC(t *testing.T) {
	t.Parallel()

	nvarcharType := mysqlConcat([]byte{tdsTypeNVarChar}, tdsUint16(8000), make([]byte, tdsCollationLen))

	body := mysqlConcat(
		tdsAllHeaders(),
		tdsUint16(0xFFFF, 10), // sp_executesql by id
		tdsUint16(0),          // option flags
		tdsBVarchar(""), []byte{0}, nvarcharType,
		tdsNVarCharValue("SELECT email FROM customers WHERE id = @p0"),
		tdsBVarchar(""), []byte{0}, nvarcharType, tdsNVarCharValue("@p0 int"),
		tdsBVarchar("@p0"), []byte{0}, []byte{tdsTypeIntN, 4}, tdsIntValue(42),
	)

	t.Run("default", func(t *testing.T) {
		t.Parallel()

		split := newMSSQLSplitter(Options{})

		assert.Equal(t, []string{
			`0s C> RPC sp_executesql "SELECT email FROM customers WHERE id = @p0" "@p0 int" (3 params)`,
		}, feedSplitter(t, split, 0, dump.DirClientToServer, tdsPacket(tdsTypeRPC, true, body)))
	})

	t.Run("rows", func(t *testing.T) {
		t.Parallel()

		split := newMSSQLSplitter(Options{ShowRows: true})

		assert.Equal(t, []string{
			`0s C> RPC sp_executesql "SELECT email FROM customers WHERE id = @p0" "@p0 int" @p0=42 (3 params)`,
		}, feedSplitter(t, split, 0, dump.DirClientToServer, tdsPacket(tdsTypeRPC, true, body)))
	})
}

// TestMSSQLSplitter_AuthenticationNeverDumped pins the one redaction --rows
// does not lift. LOGIN7's password obfuscation is a nibble swap and a fixed
// XOR, so a trace that printed it would be a trace that leaks it.
func TestMSSQLSplitter_AuthenticationNeverDumped(t *testing.T) {
	t.Parallel()

	split := newMSSQLSplitter(Options{ShowRows: true})

	lines := feedSplitter(t, split, 0, dump.DirClientToServer,
		tdsPacket(tdsTypePrelogin, true, tdsPreloginPayload(0x00)))
	lines = append(lines, feedSplitter(t, split, ms, dump.DirServerToClient,
		tdsPacket(tdsTypeReply, true, tdsPreloginPayload(0x02)))...)
	lines = append(lines, feedSplitter(t, split, 2*ms, dump.DirClientToServer,
		tdsPacket(tdsTypeLogin7, true,
			tdsLogin7Payload("laptop", "alice", "hunter2-secret", "dbeaver", "app")))...)
	lines = append(lines, feedSplitter(t, split, 3*ms, dump.DirClientToServer,
		tdsPacket(tdsTypeSSPI, true, []byte("kerberos-ticket")))...)

	assert.Equal(t, []string{
		"0s C> PRELOGIN(2 options: version, encryption) encryption=off",
		"1ms <S PRELOGIN(2 options: version, encryption) encryption=not_supported",
		`2ms C> LOGIN7 host="laptop" user="alice" app="dbeaver" database="app" (credentials redacted)`,
		"3ms C> SSPI(15 bytes, redacted)",
	}, lines)

	joined := strings.Join(lines, "\n")
	for _, secret := range []string{"hunter2-secret", "kerberos-ticket"} {
		assert.NotContains(t, joined, secret)
	}
}

// TestMSSQLSplitter_Error checks a failed statement reads as one: the server's
// own message is printed, as an ErrorResponse is on PostgreSQL.
func TestMSSQLSplitter_Error(t *testing.T) {
	t.Parallel()

	split := newMSSQLSplitter(Options{})

	lines := feedSplitter(t, split, 0, dump.DirClientToServer,
		tdsPacket(tdsTypeSQLBatch, true, mysqlConcat(tdsAllHeaders(), tdsUCS2("SELECT * FROM nope"))))
	lines = append(lines, feedSplitter(t, split, ms, dump.DirServerToClient,
		tdsPacket(tdsTypeReply, true, mysqlConcat(
			tdsErrorToken(208, 16, "Invalid object name 'nope'."),
			mysqlConcat([]byte{tdsTokenDone}, tdsUint16(0, 0), tdsUint64(0)),
		)))...)

	assert.Equal(t, []string{
		`0s C> SQLBatch "SELECT * FROM nope"`,
		`1ms <S Error 208 (16): Invalid object name 'nope'.`,
		`1ms <S Done status=0x0000`,
	}, lines)
}

// TestMSSQLSplitter_UnwalkableTokenStops checks the walk gives up loudly. A
// token whose length this decoder does not model makes everything after it not
// a token, and a guess would be worse than saying so.
func TestMSSQLSplitter_UnwalkableTokenStops(t *testing.T) {
	t.Parallel()

	split := newMSSQLSplitter(Options{})

	lines := feedSplitter(t, split, 0, dump.DirServerToClient, tdsPacket(tdsTypeReply, true, mysqlConcat(
		tdsEnvChangeToken(),
		[]byte{0x88, 0x01, 0x02, 0x03}, // ALTMETADATA: unmodelled on purpose
	)))

	assert.Equal(t, []string{
		"0s <S EnvChange(type=1)",
		"0s <S ... 4 bytes not decoded (token 0x88)",
	}, lines)
}

// TestMSSQLSplitter_OutOfSync checks a truncated capture is reported rather
// than silently producing nonsense: DBB_DUMP_MAX_SIZE drops whole packets.
func TestMSSQLSplitter_OutOfSync(t *testing.T) {
	t.Parallel()

	split := newMSSQLSplitter(Options{})

	_, err := split.Feed(&dump.Packet{
		Direction: dump.DirServerToClient,
		Data:      []byte{tdsTypeReply, tdsStatusEOM, 0x00, 0x04, 0, 0, 1, 0},
	})
	require.ErrorIs(t, err, ErrOutOfSync)
}

// TestMSSQLSplitter_PartialMessagesYieldNothing checks a packet that completes
// no message produces no line rather than an error.
func TestMSSQLSplitter_PartialMessagesYieldNothing(t *testing.T) {
	t.Parallel()

	split := newMSSQLSplitter(Options{})

	packet := tdsPacket(tdsTypeSQLBatch, true, mysqlConcat(tdsAllHeaders(), tdsUCS2("SELECT 1")))

	assert.Empty(t, feedSplitter(t, split, 0, dump.DirClientToServer, packet[:3]))
	assert.Empty(t, feedSplitter(t, split, ms, dump.DirClientToServer, packet[3:len(packet)-1]))
	assert.Equal(t,
		[]string{`2ms C> SQLBatch "SELECT 1"`},
		feedSplitter(t, split, 2*ms, dump.DirClientToServer, packet[len(packet)-1:]),
	)
}
