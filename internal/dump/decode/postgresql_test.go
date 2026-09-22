package decode

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/dump"
)

// encodeFrontend concatenates client-to-server messages into one payload, the
// way a client flushes a batch in a single write.
func encodeFrontend(t *testing.T, msgs ...pgproto3.FrontendMessage) []byte {
	t.Helper()

	var out []byte

	for _, msg := range msgs {
		encoded, err := msg.Encode(out)
		require.NoError(t, err)

		out = encoded
	}

	return out
}

// encodeBackend does the same for server-to-client messages.
func encodeBackend(t *testing.T, msgs ...pgproto3.BackendMessage) []byte {
	t.Helper()

	var out []byte

	for _, msg := range msgs {
		encoded, err := msg.Encode(out)
		require.NoError(t, err)

		out = encoded
	}

	return out
}

// feed pushes one packet through the splitter and returns the rendered lines.
func feed(t *testing.T, split *postgresSplitter, ns int64, direction byte, data []byte) []string {
	t.Helper()

	msgs, err := split.Feed(&dump.Packet{RelativeNs: ns, Direction: direction, Data: data})
	require.NoError(t, err)

	lines := make([]string, 0, len(msgs))
	for _, msg := range msgs {
		lines = append(lines, msg.String())
	}

	return lines
}

const (
	ms  = int64(1_000_000)
	sec = 1000 * ms
)

// TestPostgresSplitter_Trace is the golden test for the whole shape of the
// output: the offset, the direction marker and the per-message rendering, over
// a session that goes through startup, the extended protocol and a simple
// query. It also pins the two framing cases the splitter exists for — several
// messages in one packet, and one message split across two.
func TestPostgresSplitter_Trace(t *testing.T) {
	t.Parallel()

	split := newPostgresSplitter(Options{})

	got := make([]string, 0, 20)

	got = append(got, feed(t, split, 0, dump.DirClientToServer, encodeFrontend(t,
		&pgproto3.StartupMessage{
			ProtocolVersion: pgproto3.ProtocolVersion30,
			Parameters:      map[string]string{"user": "alice", "database": "app"},
		},
	))...)

	got = append(got, feed(t, split, 12*ms, dump.DirServerToClient, encodeBackend(t,
		&pgproto3.AuthenticationOk{},
		&pgproto3.ParameterStatus{Name: "server_version", Value: "16.1"},
		&pgproto3.BackendKeyData{ProcessID: 4242, SecretKey: []byte("cancel-token")},
		&pgproto3.ReadyForQuery{TxStatus: 'I'},
	))...)

	// One client flush carrying the whole extended-protocol batch.
	got = append(got, feed(t, split, 234*ms, dump.DirClientToServer, encodeFrontend(t,
		&pgproto3.Parse{Query: "SELECT id, name\nFROM users WHERE id = $1", ParameterOIDs: []uint32{23}},
		&pgproto3.Bind{Parameters: [][]byte{[]byte("42")}},
		&pgproto3.Describe{ObjectType: 'P'},
		&pgproto3.Execute{MaxRows: 501},
		&pgproto3.Sync{},
	))...)

	// The server's answer arrives split mid-DataRow: the first packet must
	// yield only the messages that are complete.
	answer := encodeBackend(t,
		&pgproto3.ParseComplete{},
		&pgproto3.BindComplete{},
		&pgproto3.RowDescription{Fields: []pgproto3.FieldDescription{
			{Name: []byte("id")}, {Name: []byte("name")},
		}},
		&pgproto3.DataRow{Values: [][]byte{[]byte("42"), []byte("alice")}},
		&pgproto3.PortalSuspended{},
		&pgproto3.ReadyForQuery{TxStatus: 'I'},
	)

	cut := len(answer) - 12
	got = append(got, feed(t, split, 251*ms, dump.DirServerToClient, answer[:cut])...)
	got = append(got, feed(t, split, 329*ms, dump.DirServerToClient, answer[cut:])...)

	got = append(got, feed(t, split, 1400*ms, dump.DirClientToServer, encodeFrontend(t,
		&pgproto3.Query{String: "SELECT 1"},
	))...)

	got = append(got, feed(t, split, 1450*ms, dump.DirServerToClient, encodeBackend(t,
		&pgproto3.CommandComplete{CommandTag: []byte("SELECT 1")},
		&pgproto3.ReadyForQuery{TxStatus: 'I'},
	))...)

	want := []string{
		`0s C> StartupMessage protocol=3.0 database="app" user="alice"`,
		`12ms <S AuthenticationOk`,
		`12ms <S ParameterStatus server_version="16.1"`,
		`12ms <S BackendKeyData pid=4242`,
		`12ms <S ReadyForQuery I`,
		`234ms C> Parse stmt="" "SELECT id, name FROM users WHERE id = $1" (1 params)`,
		`234ms C> Bind portal="" stmt="" (1 params)`,
		`234ms C> Describe portal=""`,
		`234ms C> Execute portal="" maxRows=501`,
		`234ms C> Sync`,
		`251ms <S ParseComplete`,
		`251ms <S BindComplete`,
		`251ms <S RowDescription(2 fields)`,
		`329ms <S DataRow(2 cols)`,
		`329ms <S PortalSuspended`,
		`329ms <S ReadyForQuery I`,
		`1.4s C> Query "SELECT 1"`,
		`1.45s <S CommandComplete "SELECT 1"`,
		`1.45s <S ReadyForQuery I`,
	}

	assert.Equal(t, want, got)

	// The secret cancellation key never reaches the trace.
	assert.NotContains(t, strings.Join(got, "\n"), "cancel-token")
}

// TestPostgresSplitter_RedactsByDefault is the security-relevant default: a
// capture holds customer rows, so a trace that can be pasted into a bug report
// must not.
func TestPostgresSplitter_RedactsByDefault(t *testing.T) {
	t.Parallel()

	rows := encodeBackend(t,
		&pgproto3.RowDescription{Fields: []pgproto3.FieldDescription{
			{Name: []byte("email")}, {Name: []byte("iban")},
		}},
		&pgproto3.DataRow{Values: [][]byte{[]byte("alice@example.com"), nil}},
	)

	bind := encodeFrontend(t,
		&pgproto3.StartupMessage{
			ProtocolVersion: pgproto3.ProtocolVersion30,
			Parameters:      map[string]string{"user": "alice"},
		},
		&pgproto3.Bind{Parameters: [][]byte{[]byte("secret-token"), nil}},
	)

	t.Run("default", func(t *testing.T) {
		t.Parallel()

		split := newPostgresSplitter(Options{})
		lines := feed(t, split, 0, dump.DirClientToServer, bind)
		lines = append(lines, feed(t, split, ms, dump.DirServerToClient, rows)...)

		joined := strings.Join(lines, "\n")
		assert.NotContains(t, joined, "secret-token")
		assert.NotContains(t, joined, "alice@example.com")
		assert.NotContains(t, joined, "iban")
		assert.Contains(t, joined, "DataRow(2 cols)")
		assert.Contains(t, joined, "RowDescription(2 fields)")
		assert.Contains(t, joined, "Bind portal=\"\" stmt=\"\" (2 params)")
	})

	t.Run("rows", func(t *testing.T) {
		t.Parallel()

		split := newPostgresSplitter(Options{ShowRows: true})
		lines := feed(t, split, 0, dump.DirClientToServer, bind)
		lines = append(lines, feed(t, split, ms, dump.DirServerToClient, rows)...)

		joined := strings.Join(lines, "\n")
		assert.Contains(t, joined, `DataRow(2 cols) ["alice@example.com", NULL]`)
		assert.Contains(t, joined, `RowDescription(2 fields) [email, iban]`)
		assert.Contains(t, joined, `Bind portal="" stmt="" (2 params) ["secret-token", NULL]`)
	})
}

// TestPostgresSplitter_AuthenticationNeverDumped pins the one redaction --rows
// does not lift.
func TestPostgresSplitter_AuthenticationNeverDumped(t *testing.T) {
	t.Parallel()

	split := newPostgresSplitter(Options{ShowRows: true})

	lines := feed(t, split, 0, dump.DirClientToServer, encodeFrontend(t,
		&pgproto3.StartupMessage{
			ProtocolVersion: pgproto3.ProtocolVersion30,
			Parameters:      map[string]string{"user": "alice"},
		},
	))

	lines = append(lines, feed(t, split, ms, dump.DirServerToClient, encodeBackend(t,
		&pgproto3.AuthenticationSASL{AuthMechanisms: []string{"SCRAM-SHA-256"}},
	))...)

	lines = append(lines, feed(t, split, 2*ms, dump.DirClientToServer, encodeFrontend(t,
		&pgproto3.SASLInitialResponse{AuthMechanism: "SCRAM-SHA-256", Data: []byte("n,,n=alice,r=client-nonce")},
	))...)

	lines = append(lines, feed(t, split, 3*ms, dump.DirServerToClient, encodeBackend(t,
		&pgproto3.AuthenticationSASLContinue{Data: []byte("r=server-nonce,s=c2FsdA==,i=4096")},
	))...)

	lines = append(lines, feed(t, split, 4*ms, dump.DirClientToServer, encodeFrontend(t,
		&pgproto3.SASLResponse{Data: []byte("c=biws,r=server-nonce,p=client-proof")},
	))...)

	joined := strings.Join(lines, "\n")

	assert.Contains(t, joined, "AuthenticationSASL")
	assert.Contains(t, joined, "SASLInitialResponse")
	assert.Contains(t, joined, "AuthenticationSASLContinue (redacted)")
	assert.Contains(t, joined, "SASLResponse (redacted)")

	for _, secret := range []string{"client-nonce", "server-nonce", "client-proof", "c2FsdA=="} {
		assert.NotContains(t, joined, secret)
	}
}

// TestPostgresSplitter_SSLNegotiation covers the untyped prologue: a capture
// taken below TLS starts with an SSLRequest and a one-byte answer, and a
// refused upgrade is followed by an ordinary startup packet.
func TestPostgresSplitter_SSLNegotiation(t *testing.T) {
	t.Parallel()

	t.Run("refused then startup", func(t *testing.T) {
		t.Parallel()

		split := newPostgresSplitter(Options{})

		lines := feed(t, split, 0, dump.DirClientToServer, encodeFrontend(t, &pgproto3.SSLRequest{}))
		lines = append(lines, feed(t, split, ms, dump.DirServerToClient, []byte{'N'})...)
		lines = append(lines, feed(t, split, 2*ms, dump.DirClientToServer, encodeFrontend(t,
			&pgproto3.StartupMessage{
				ProtocolVersion: pgproto3.ProtocolVersion30,
				Parameters:      map[string]string{"user": "bob"},
			},
		))...)
		lines = append(lines, feed(t, split, 3*ms, dump.DirServerToClient, encodeBackend(t,
			&pgproto3.AuthenticationOk{},
		))...)

		assert.Equal(t, []string{
			"0s C> SSLRequest",
			"1ms <S SSLResponse N (refused)",
			`2ms C> StartupMessage protocol=3.0 user="bob"`,
			"3ms <S AuthenticationOk",
		}, lines)
	})

	t.Run("accepted stops decoding", func(t *testing.T) {
		t.Parallel()

		split := newPostgresSplitter(Options{})

		lines := feed(t, split, 0, dump.DirClientToServer, encodeFrontend(t, &pgproto3.SSLRequest{}))
		lines = append(lines, feed(t, split, ms, dump.DirServerToClient, []byte{'S'})...)
		// Whatever follows is TLS records; the splitter stops rather than
		// reporting garbage as protocol messages.
		lines = append(lines, feed(t, split, 2*ms, dump.DirClientToServer,
			[]byte{0x16, 0x03, 0x01, 0x00, 0x42, 0x01})...)

		assert.Equal(t, []string{
			"0s C> SSLRequest",
			"1ms <S SSLResponse S (accepted, rest of the capture is TLS)",
		}, lines)
	})
}

// TestPostgresSplitter_OutOfSync checks a truncated capture is reported rather
// than silently producing nonsense: DBB_DUMP_MAX_SIZE drops whole packets.
func TestPostgresSplitter_OutOfSync(t *testing.T) {
	t.Parallel()

	split := newPostgresSplitter(Options{})

	_, err := split.Feed(&dump.Packet{
		Direction: dump.DirClientToServer,
		Data:      []byte{0x00, 0x00, 0x00, 0x02},
	})
	require.ErrorIs(t, err, ErrOutOfSync)

	// A typed stream whose length prefix is impossible is caught too.
	typed := newPostgresSplitter(Options{})
	_, err = typed.Feed(&dump.Packet{
		Direction: dump.DirServerToClient,
		Data:      []byte{'Z', 0x00, 0x00, 0x00, 0x01, 'I'},
	})
	require.ErrorIs(t, err, ErrOutOfSync)
}

// TestPostgresSplitter_PartialMessagesYieldNothing checks a packet that
// completes no message produces no line rather than an error.
func TestPostgresSplitter_PartialMessagesYieldNothing(t *testing.T) {
	t.Parallel()

	split := newPostgresSplitter(Options{})

	startup := encodeFrontend(t, &pgproto3.StartupMessage{
		ProtocolVersion: pgproto3.ProtocolVersion30,
		Parameters:      map[string]string{"user": "carol"},
	})

	assert.Empty(t, feed(t, split, 0, dump.DirClientToServer, startup[:3]))
	assert.Empty(t, feed(t, split, ms, dump.DirClientToServer, startup[3:len(startup)-1]))
	assert.Equal(t,
		[]string{`2ms C> StartupMessage protocol=3.0 user="carol"`},
		feed(t, split, 2*ms, dump.DirClientToServer, startup[len(startup)-1:]),
	)
}

func TestFormatOffset(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "0s", FormatOffset(0))
	assert.Equal(t, "234ms", FormatOffset(234*ms))
	assert.Equal(t, "1.234s", FormatOffset(1234*ms))
	assert.Equal(t, "2m3.456s", FormatOffset(123456*ms))
	assert.Equal(t, "1s", FormatOffset(sec))
}
