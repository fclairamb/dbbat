package decode

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gomysql "github.com/go-mysql-org/go-mysql/mysql"
	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/fclairamb/dbbat/internal/dump"
)

// writeCapture builds a real pcapng capture with dump.Writer so File is
// exercised end to end, reader included, rather than against a stub.
func writeCapture(t *testing.T, protocol string, packets []dump.Packet) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "session"+dump.FileExt)

	writer, err := dump.NewWriter(path, dump.Header{
		SessionID: "11111111-2222-3333-4444-555555555555",
		Protocol:  protocol,
		StartTime: time.Now(),
	}, 0)
	require.NoError(t, err)

	for _, packet := range packets {
		require.NoError(t, writer.WritePacket(packet.Direction, packet.Data))
	}

	require.NoError(t, writer.Close())

	return path
}

// textOf strips the offset and direction marker, leaving the message rendering.
// dump.Writer timestamps packets with the wall clock, so the offsets of a
// capture written by a test are not reproducible; the splitter's own test pins
// them instead.
func textOf(t *testing.T, trace string) []string {
	t.Helper()

	var out []string

	for _, line := range strings.Split(strings.TrimSpace(trace), "\n") {
		if strings.HasPrefix(line, "#") {
			out = append(out, line)

			continue
		}

		fields := strings.SplitN(line, " ", 3)
		require.Len(t, fields, 3, "line %q", line)
		assert.Contains(t, []string{markerClientToServer, markerServerToClient}, fields[1])

		out = append(out, fields[1]+" "+fields[2])
	}

	return out
}

func TestFile_PostgreSQL(t *testing.T) {
	t.Parallel()

	startup, err := (&pgproto3.StartupMessage{
		ProtocolVersion: pgproto3.ProtocolVersion30,
		Parameters:      map[string]string{"user": "alice", "database": "app"},
	}).Encode(nil)
	require.NoError(t, err)

	var server []byte
	for _, msg := range []pgproto3.BackendMessage{
		&pgproto3.AuthenticationOk{},
		&pgproto3.ReadyForQuery{TxStatus: 'I'},
	} {
		server, err = msg.Encode(server)
		require.NoError(t, err)
	}

	var client []byte
	for _, msg := range []pgproto3.FrontendMessage{
		&pgproto3.Query{String: "SELECT email FROM customers"},
	} {
		client, err = msg.Encode(client)
		require.NoError(t, err)
	}

	var rows []byte
	for _, msg := range []pgproto3.BackendMessage{
		&pgproto3.RowDescription{Fields: []pgproto3.FieldDescription{{Name: []byte("email")}}},
		&pgproto3.DataRow{Values: [][]byte{[]byte("alice@example.com")}},
		&pgproto3.CommandComplete{CommandTag: []byte("SELECT 1")},
		&pgproto3.ReadyForQuery{TxStatus: 'I'},
	} {
		rows, err = msg.Encode(rows)
		require.NoError(t, err)
	}

	path := writeCapture(t, dump.ProtocolPostgreSQL, []dump.Packet{
		{Direction: dump.DirClientToServer, Data: startup},
		{Direction: dump.DirServerToClient, Data: server},
		{Direction: dump.DirClientToServer, Data: client},
		{Direction: dump.DirServerToClient, Data: rows},
	})

	t.Run("redacted by default", func(t *testing.T) {
		t.Parallel()

		var out bytes.Buffer
		require.NoError(t, File(path, Options{}, &out))

		assert.Equal(t, []string{
			"# postgresql session 11111111-2222-3333-4444-555555555555",
			`C> StartupMessage protocol=3.0 database="app" user="alice"`,
			"<S AuthenticationOk",
			"<S ReadyForQuery I",
			`C> Query "SELECT email FROM customers"`,
			"<S RowDescription(1 fields)",
			"<S DataRow(1 cols)",
			`<S CommandComplete "SELECT 1"`,
			"<S ReadyForQuery I",
		}, textOf(t, out.String()))

		assert.NotContains(t, out.String(), "alice@example.com")
	})

	t.Run("--rows opts into values", func(t *testing.T) {
		t.Parallel()

		var out bytes.Buffer
		require.NoError(t, File(path, Options{ShowRows: true}, &out))

		assert.Contains(t, out.String(), `DataRow(1 cols) ["alice@example.com"]`)
	})
}

// TestFile_UnsupportedProtocol pins the deliberate refusal: a capture whose
// header names a protocol with no decoder must say so rather than print
// nothing or misread the bytes as something it is not.
func TestFile_UnsupportedProtocol(t *testing.T) {
	t.Parallel()

	const unknown = "cassandra"

	path := writeCapture(t, unknown, []dump.Packet{
		{Direction: dump.DirClientToServer, Data: []byte("whatever")},
	})

	var out bytes.Buffer
	err := File(path, Options{}, &out)

	require.ErrorIs(t, err, ErrUnsupportedProtocol)
	assert.Contains(t, err.Error(), unknown)
	assert.Empty(t, out.String())
	assert.False(t, Supported(unknown))

	// Every protocol dbbat proxies is decoded.
	assert.True(t, Supported(dump.ProtocolPostgreSQL))
	assert.True(t, Supported(dump.ProtocolMySQL))
	assert.True(t, Supported(dump.ProtocolMongo))
	assert.True(t, Supported(dump.ProtocolMSSQL))
	assert.True(t, Supported(dump.ProtocolOracle))
}

// TestFile_MSSQL is the end-to-end pass over a real capture written by
// dump.Writer, reader included.
func TestFile_MSSQL(t *testing.T) {
	t.Parallel()

	batch := tdsPacket(tdsTypeSQLBatch, true,
		mysqlConcat(tdsAllHeaders(), tdsUCS2("SELECT email FROM customers")))

	response := tdsPacket(tdsTypeReply, true, mysqlConcat(
		tdsColMetadata(tdsNVarCharColumn("email")),
		tdsRow(tdsNVarCharValue("alice@example.com")),
		tdsDone(1),
	))

	path := writeCapture(t, dump.ProtocolMSSQL, []dump.Packet{
		{Direction: dump.DirClientToServer, Data: batch},
		{Direction: dump.DirServerToClient, Data: response},
	})

	t.Run("redacted by default", func(t *testing.T) {
		t.Parallel()

		var out bytes.Buffer
		require.NoError(t, File(path, Options{}, &out))

		assert.Equal(t, []string{
			"# mssql session 11111111-2222-3333-4444-555555555555",
			`C> SQLBatch "SELECT email FROM customers"`,
			"<S ColMetaData(1 cols)",
			"<S Row(1 cols)",
			"<S Done status=0x0010 rows=1",
		}, textOf(t, out.String()))

		assert.NotContains(t, out.String(), "alice@example.com")
	})

	t.Run("--rows opts into values", func(t *testing.T) {
		t.Parallel()

		var out bytes.Buffer
		require.NoError(t, File(path, Options{ShowRows: true}, &out))

		assert.Contains(t, out.String(), `Row(1 cols) ["alice@example.com"]`)
	})
}

// TestFile_MongoDB is the end-to-end pass over a real capture written by
// dump.Writer, reader included.
func TestFile_MongoDB(t *testing.T) {
	t.Parallel()

	command := mongoOpMsgMessage(1, 0, 0, mongoSection0(t, bson.D{
		{Key: "find", Value: "customers"},
		{Key: "filter", Value: bson.D{{Key: "email", Value: "alice@example.com"}}},
		{Key: "$db", Value: "app"},
	}))

	reply := mongoOpMsgMessage(2, 1, 0, mongoSection0(t, bson.D{
		{Key: "cursor", Value: bson.D{
			{Key: "id", Value: int64(0)},
			{Key: "firstBatch", Value: bson.A{bson.D{{Key: "email", Value: "alice@example.com"}}}},
		}},
		{Key: "ok", Value: 1.0},
	}))

	path := writeCapture(t, dump.ProtocolMongo, []dump.Packet{
		{Direction: dump.DirClientToServer, Data: command},
		{Direction: dump.DirServerToClient, Data: reply},
	})

	t.Run("redacted by default", func(t *testing.T) {
		t.Parallel()

		var out bytes.Buffer
		require.NoError(t, File(path, Options{}, &out))

		assert.Equal(t, []string{
			"# mongodb session 11111111-2222-3333-4444-555555555555",
			"C> find customers db=app (filter: 1 keys)",
			"<S Reply ok=1 (cursor: 0, firstBatch: 1 docs)",
		}, textOf(t, out.String()))

		assert.NotContains(t, out.String(), "alice@example.com")
	})

	t.Run("--rows opts into documents", func(t *testing.T) {
		t.Parallel()

		var out bytes.Buffer
		require.NoError(t, File(path, Options{ShowRows: true}, &out))

		assert.Contains(t, out.String(), "alice@example.com")
	})
}

// TestFile_MySQL is the end-to-end pass: a real capture written by dump.Writer,
// read back through File, reader included.
func TestFile_MySQL(t *testing.T) {
	t.Parallel()

	path := writeCapture(t, dump.ProtocolMySQL, []dump.Packet{
		{
			Direction: dump.DirClientToServer,
			Data:      mysqlPacket(0, mysqlCommandPayload(gomysql.COM_QUERY, "SELECT email FROM customers")),
		},
		{
			Direction: dump.DirServerToClient,
			Data: mysqlPackets(
				[]byte{0x01},
				mysqlColumnDefPayload("email"),
				mysqlTextRowPayload(strptr("alice@example.com")),
				mysqlResultEndOKPayload(),
			),
		},
	})

	t.Run("redacted by default", func(t *testing.T) {
		t.Parallel()

		var out bytes.Buffer
		require.NoError(t, File(path, Options{}, &out))

		assert.Equal(t, []string{
			"# mysql session 11111111-2222-3333-4444-555555555555",
			`C> COM_QUERY "SELECT email FROM customers"`,
			"<S ResultSet(1 cols)",
			"<S ColumnDefinition",
			"<S Row(1 cols)",
			"<S OK affected=1 insertId=0 status=0x0002 warnings=0",
		}, textOf(t, out.String()))

		assert.NotContains(t, out.String(), "alice@example.com")
	})

	t.Run("--rows opts into values", func(t *testing.T) {
		t.Parallel()

		var out bytes.Buffer
		require.NoError(t, File(path, Options{ShowRows: true}, &out))

		assert.Contains(t, out.String(), `Row(1 cols) ["alice@example.com"]`)
	})
}

func TestFile_MissingFile(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	require.Error(t, File(filepath.Join(t.TempDir(), "nope"+dump.FileExt), Options{}, &out))
}

// TestFile_TruncatedCapture checks the lines decoded before a break are still
// written out: a capture cut short by DBB_DUMP_MAX_SIZE is still evidence up to
// the cut.
func TestFile_TruncatedCapture(t *testing.T) {
	t.Parallel()

	startup, err := (&pgproto3.StartupMessage{
		ProtocolVersion: pgproto3.ProtocolVersion30,
		Parameters:      map[string]string{"user": "alice"},
	}).Encode(nil)
	require.NoError(t, err)

	path := writeCapture(t, dump.ProtocolPostgreSQL, []dump.Packet{
		{Direction: dump.DirClientToServer, Data: startup},
		// A typed message claiming an impossible length: what a dropped packet
		// leaves behind.
		{Direction: dump.DirClientToServer, Data: []byte{'Q', 0x00, 0x00, 0x00, 0x00}},
	})

	var out bytes.Buffer
	err = File(path, Options{}, &out)

	require.ErrorIs(t, err, ErrOutOfSync)
	assert.Contains(t, out.String(), "StartupMessage")
}
