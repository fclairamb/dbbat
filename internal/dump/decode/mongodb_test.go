package decode

import (
	"encoding/binary"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/fclairamb/dbbat/internal/dump"
)

func mongoDoc(t *testing.T, doc bson.D) []byte {
	t.Helper()

	raw, err := bson.Marshal(doc)
	require.NoError(t, err)

	return raw
}

func mongoInt32(values ...int32) []byte {
	out := make([]byte, 0, 4*len(values))

	for _, value := range values {
		out = binary.LittleEndian.AppendUint32(out, uint32(value))
	}

	return out
}

// mongoSection0 builds the kind-0 section: the command document itself.
func mongoSection0(t *testing.T, doc bson.D) []byte {
	t.Helper()

	return mysqlConcat([]byte{0}, mongoDoc(t, doc))
}

// mongoSection1 builds a kind-1 section: an int32 size counting itself, the
// identifier, then the documents.
func mongoSection1(t *testing.T, identifier string, docs ...bson.D) []byte {
	t.Helper()

	parts := make([][]byte, 0, len(docs)+2)
	parts = append(parts, []byte(identifier), []byte{0})

	for _, doc := range docs {
		parts = append(parts, mongoDoc(t, doc))
	}

	payload := mysqlConcat(parts...)

	return mysqlConcat([]byte{1}, mongoInt32(int32(4+len(payload))), payload)
}

// mongoWireMessage wraps a body in the 16-byte header every wire message
// starts with.
func mongoWireMessage(requestID, responseTo, opCode int32, body []byte) []byte {
	header := mongoInt32(int32(mongoHeaderLen+len(body)), requestID, responseTo, opCode)

	return mysqlConcat(header, body)
}

// mongoOpMsg builds a complete OP_MSG message.
func mongoOpMsgMessage(requestID, responseTo int32, flags uint32, sections ...[]byte) []byte {
	body := mysqlConcat(append([][]byte{mongoInt32(int32(flags))}, sections...)...)

	return mongoWireMessage(requestID, responseTo, mongoOpMsg, body)
}

// TestMongoSplitter_Trace is the golden test for the whole shape of the
// output. It pins the two framing cases the splitter exists for — several
// messages in one packet, and one message split across two — and both section
// kinds.
func TestMongoSplitter_Trace(t *testing.T) {
	t.Parallel()

	split := newMongoSplitter(Options{})

	find := mongoOpMsgMessage(1, 0, 0, mongoSection0(t, bson.D{
		{Key: "find", Value: "orders"},
		{Key: "filter", Value: bson.D{{Key: "status", Value: "open"}, {Key: "total", Value: 500}}},
		{Key: "$db", Value: "app"},
	}))

	got := feedSplitter(t, split, 0, dump.DirClientToServer, find)

	reply := mongoOpMsgMessage(2, 1, 0, mongoSection0(t, bson.D{
		{Key: "cursor", Value: bson.D{
			{Key: "id", Value: int64(77)},
			{Key: "ns", Value: "app.orders"},
			{Key: "firstBatch", Value: bson.A{
				bson.D{{Key: "email", Value: "alice@example.com"}},
				bson.D{{Key: "email", Value: "bob@example.com"}},
			}},
		}},
		{Key: "ok", Value: 1.0},
	}))

	// The reply arrives split mid-message: the first feed must yield nothing.
	cut := len(reply) - 20
	got = append(got, feedSplitter(t, split, 12*ms, dump.DirServerToClient, reply[:cut])...)
	got = append(got, feedSplitter(t, split, 31*ms, dump.DirServerToClient, reply[cut:])...)

	// One client flush carrying two messages.
	insert := mongoOpMsgMessage(3, 0, 0,
		mongoSection0(t, bson.D{{Key: "insert", Value: "orders"}, {Key: "$db", Value: "app"}}),
		mongoSection1(t, "documents",
			bson.D{{Key: "email", Value: "carol@example.com"}},
			bson.D{{Key: "email", Value: "dave@example.com"}},
		),
	)

	getMore := mongoOpMsgMessage(4, 0, 0, mongoSection0(t, bson.D{
		{Key: "getMore", Value: int64(77)},
		{Key: "collection", Value: "orders"},
		{Key: "$db", Value: "app"},
	}))

	got = append(got, feedSplitter(t, split, 44*ms, dump.DirClientToServer,
		mysqlConcat(insert, getMore))...)

	assert.Equal(t, []string{
		`0s C> find orders db=app (filter: 2 keys)`,
		`31ms <S Reply ok=1 (cursor: 77, firstBatch: 2 docs)`,
		`44ms C> insert orders db=app (documents: 2 docs)`,
		`44ms C> getMore orders db=app`,
	}, got)

	joined := strings.Join(got, "\n")
	for _, secret := range []string{"alice@example.com", "carol@example.com", "open"} {
		assert.NotContains(t, joined, secret)
	}
}

// TestMongoSplitter_RedactsByDefault is the security-relevant default: on
// MongoDB the document *is* the data, so it is reported by shape and not by
// content unless --rows asks.
func TestMongoSplitter_RedactsByDefault(t *testing.T) {
	t.Parallel()

	command := mongoOpMsgMessage(1, 0, 0, mongoSection0(t, bson.D{
		{Key: "find", Value: "customers"},
		{Key: "filter", Value: bson.D{{Key: "iban", Value: "FR7630006000011234567890189"}}},
		{Key: "$db", Value: "app"},
	}))

	reply := mongoOpMsgMessage(2, 1, 0, mongoSection0(t, bson.D{
		{Key: "cursor", Value: bson.D{
			{Key: "id", Value: int64(0)},
			{Key: "firstBatch", Value: bson.A{bson.D{{Key: "email", Value: "alice@example.com"}}}},
		}},
		{Key: "ok", Value: 1.0},
	}))

	t.Run("default", func(t *testing.T) {
		t.Parallel()

		split := newMongoSplitter(Options{})
		lines := feedSplitter(t, split, 0, dump.DirClientToServer, command)
		lines = append(lines, feedSplitter(t, split, ms, dump.DirServerToClient, reply)...)

		assert.Equal(t, []string{
			`0s C> find customers db=app (filter: 1 keys)`,
			`1ms <S Reply ok=1 (cursor: 0, firstBatch: 1 docs)`,
		}, lines)

		joined := strings.Join(lines, "\n")
		assert.NotContains(t, joined, "FR7630006000011234567890189")
		assert.NotContains(t, joined, "alice@example.com")
	})

	t.Run("rows", func(t *testing.T) {
		t.Parallel()

		split := newMongoSplitter(Options{ShowRows: true})
		lines := feedSplitter(t, split, 0, dump.DirClientToServer, command)
		lines = append(lines, feedSplitter(t, split, ms, dump.DirServerToClient, reply)...)

		joined := strings.Join(lines, "\n")
		assert.Contains(t, joined, "FR7630006000011234567890189")
		assert.Contains(t, joined, "alice@example.com")
	})
}

// TestMongoSplitter_AuthenticationNeverDumped pins the one redaction --rows
// does not lift, in both directions: a reply is matched to its request, so the
// server's half of a SASL exchange stays out of the trace too.
func TestMongoSplitter_AuthenticationNeverDumped(t *testing.T) {
	t.Parallel()

	split := newMongoSplitter(Options{ShowRows: true})

	saslStart := mongoOpMsgMessage(10, 0, 0, mongoSection0(t, bson.D{
		{Key: "saslStart", Value: 1},
		{Key: "mechanism", Value: "SCRAM-SHA-256"},
		{Key: "payload", Value: []byte("n,,n=alice,r=client-nonce")},
		{Key: "$db", Value: "admin"},
	}))

	saslReply := mongoOpMsgMessage(11, 10, 0, mongoSection0(t, bson.D{
		{Key: "conversationId", Value: 1},
		{Key: "payload", Value: []byte("r=server-nonce,s=c2FsdA==,i=4096")},
		{Key: "ok", Value: 1.0},
	}))

	hello := mongoOpMsgMessage(12, 0, 0, mongoSection0(t, bson.D{
		{Key: "hello", Value: 1},
		{Key: "speculativeAuthenticate", Value: bson.D{{Key: "payload", Value: "speculative-proof"}}},
		{Key: "$db", Value: "admin"},
	}))

	lines := feedSplitter(t, split, 0, dump.DirClientToServer, saslStart)
	lines = append(lines, feedSplitter(t, split, ms, dump.DirServerToClient, saslReply)...)
	lines = append(lines, feedSplitter(t, split, 2*ms, dump.DirClientToServer, hello)...)

	assert.Equal(t, []string{
		"0s C> saslStart (redacted)",
		"1ms <S Reply (redacted)",
		"2ms C> hello (redacted)",
	}, lines)

	joined := strings.Join(lines, "\n")
	for _, secret := range []string{"client-nonce", "server-nonce", "c2FsdA==", "speculative-proof"} {
		assert.NotContains(t, joined, secret)
	}
}

// TestMongoSplitter_Error checks a refused command reads as one: the server's
// own message is printed, as an ErrorResponse is on PostgreSQL.
func TestMongoSplitter_Error(t *testing.T) {
	t.Parallel()

	split := newMongoSplitter(Options{})

	command := mongoOpMsgMessage(1, 0, 0, mongoSection0(t, bson.D{
		{Key: "drop", Value: "orders"},
		{Key: "$db", Value: "app"},
	}))

	reply := mongoOpMsgMessage(2, 1, 0, mongoSection0(t, bson.D{
		{Key: "ok", Value: 0.0},
		{Key: "errmsg", Value: "blocked by dbbat: grant is read-only"},
		{Key: "code", Value: 13},
	}))

	lines := feedSplitter(t, split, 0, dump.DirClientToServer, command)
	lines = append(lines, feedSplitter(t, split, ms, dump.DirServerToClient, reply)...)

	assert.Equal(t, []string{
		`0s C> drop orders db=app`,
		`1ms <S Reply ok=0 code=13 "blocked by dbbat: grant is read-only" (3 keys)`,
	}, lines)
}

// TestMongoSplitter_MoreToCome checks the exhaust flag is visible: a message
// that expects no answer is worth seeing as such in a trace.
func TestMongoSplitter_MoreToCome(t *testing.T) {
	t.Parallel()

	split := newMongoSplitter(Options{})

	command := mongoOpMsgMessage(1, 0, mongoFlagMoreToCome, mongoSection0(t, bson.D{
		{Key: "insert", Value: "events"},
		{Key: "$db", Value: "app"},
	}))

	assert.Equal(t,
		[]string{`0s C> insert events db=app [moreToCome]`},
		feedSplitter(t, split, 0, dump.DirClientToServer, command),
	)
}

// TestMongoSplitter_OutOfSync checks a truncated capture is reported rather
// than silently producing nonsense: DBB_DUMP_MAX_SIZE drops whole packets.
func TestMongoSplitter_OutOfSync(t *testing.T) {
	t.Parallel()

	split := newMongoSplitter(Options{})

	_, err := split.Feed(&dump.Packet{
		Direction: dump.DirClientToServer,
		Data:      mongoInt32(4, 1, 0, mongoOpMsg),
	})
	require.ErrorIs(t, err, ErrOutOfSync)
}

// TestMongoSplitter_PartialMessagesYieldNothing checks a packet that completes
// no message produces no line rather than an error.
func TestMongoSplitter_PartialMessagesYieldNothing(t *testing.T) {
	t.Parallel()

	split := newMongoSplitter(Options{})

	command := mongoOpMsgMessage(1, 0, 0, mongoSection0(t, bson.D{
		{Key: "ping", Value: 1},
		{Key: "$db", Value: "admin"},
	}))

	assert.Empty(t, feedSplitter(t, split, 0, dump.DirClientToServer, command[:3]))
	assert.Empty(t, feedSplitter(t, split, ms, dump.DirClientToServer, command[3:len(command)-1]))
	assert.Equal(t,
		[]string{`2ms C> ping db=admin`},
		feedSplitter(t, split, 2*ms, dump.DirClientToServer, command[len(command)-1:]),
	)
}
