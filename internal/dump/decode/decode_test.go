package decode

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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

// TestFile_UnsupportedProtocol pins the deliberate refusal: the four other
// protocols have no decoder yet, and a capture of one must say so rather than
// print nothing or misread the bytes as PostgreSQL.
func TestFile_UnsupportedProtocol(t *testing.T) {
	t.Parallel()

	for _, protocol := range []string{
		dump.ProtocolOracle,
		dump.ProtocolMySQL,
		dump.ProtocolMongo,
		dump.ProtocolMSSQL,
	} {
		t.Run(protocol, func(t *testing.T) {
			t.Parallel()

			path := writeCapture(t, protocol, []dump.Packet{
				{Direction: dump.DirClientToServer, Data: []byte("whatever")},
			})

			var out bytes.Buffer
			err := File(path, Options{}, &out)

			require.ErrorIs(t, err, ErrUnsupportedProtocol)
			assert.Contains(t, err.Error(), protocol)
			assert.Empty(t, out.String())
			assert.False(t, Supported(protocol))
		})
	}

	assert.True(t, Supported(dump.ProtocolPostgreSQL))
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
