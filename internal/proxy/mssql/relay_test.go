package mssql

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/store"
)

// sqlBatchPayload encodes a SQLBatch body: the TDS 7.2+ ALL_HEADERS block
// followed by the statement as UCS-2LE. The relay does not look inside it in
// stage 2 — the point here is that whatever the client sends arrives byte for
// byte.
func sqlBatchPayload(sql string) []byte {
	// A single transaction-descriptor header: total length (4), header length
	// (4), type (2), transaction descriptor (8), outstanding requests (4).
	headers := []byte{
		0x16, 0x00, 0x00, 0x00,
		0x12, 0x00, 0x00, 0x00,
		0x02, 0x00,
		0, 0, 0, 0, 0, 0, 0, 0,
		0x01, 0x00, 0x00, 0x00,
	}

	return append(headers, stringToUCS2(sql)...)
}

func TestRelayForwardsRequestsAndResponses(t *testing.T) {
	t.Parallel()

	fixture := newAuthFixture(t, encryptNotSup, "disable")

	client, outcome := fixture.login(t, fixtureUser, fixturePassword, fixtureDBEntry)
	require.True(t, outcome.Acked)

	const sql = "SELECT 1 -- accentué"

	batch := sqlBatchPayload(sql)
	require.NoError(t, client.pkt.WriteMessage(packetTypeSQLBatch, batch))

	response := client.readReply(t)
	assert.Equal(t, buildDoneToken(0, 1), response, "the upstream's answer must arrive untouched")

	require.Eventually(t, func() bool { return len(fixture.fake.receivedRequests()) == 1 },
		10*time.Second, 20*time.Millisecond)

	got := fixture.fake.receivedRequests()[0]
	assert.Equal(t, packetTypeSQLBatch, got.msgType)
	assert.Equal(t, batch, got.payload, "the request must reach the upstream byte for byte")
}

// TestRelayStreamsAResponseLargerThanOnePacket is the reason responses are
// forwarded a packet at a time rather than reassembled: a result set is a
// single logical TDS message and can be arbitrarily large.
func TestRelayStreamsAResponseLargerThanOnePacket(t *testing.T) {
	t.Parallel()

	fixture := newAuthFixture(t, encryptNotSup, "disable")

	// Comfortably more than the negotiated 4096-byte packet size, so the
	// message spans dozens of packets and the packet-id restart matters.
	large := bytes.Repeat([]byte{0xAB, 0xCD}, 60*1024)

	fixture.fake.mu.Lock()
	fixture.fake.batchResponse = large
	fixture.fake.mu.Unlock()

	client, outcome := fixture.login(t, fixtureUser, fixturePassword, fixtureDBEntry)
	require.True(t, outcome.Acked)

	require.NoError(t, client.pkt.WriteMessage(packetTypeSQLBatch, sqlBatchPayload("SELECT * FROM big")))

	assert.Equal(t, large, client.readReply(t))
}

// TestRelayForwardsAnAttention proves the out-of-band cancel path survives the
// relay. It is the reason the two directions run as independent pumps rather
// than a request/response loop.
func TestRelayForwardsAnAttention(t *testing.T) {
	t.Parallel()

	fixture := newAuthFixture(t, encryptNotSup, "disable")

	client, outcome := fixture.login(t, fixtureUser, fixturePassword, fixtureDBEntry)
	require.True(t, outcome.Acked)

	// ATTENTION is a header-only message: no payload at all.
	require.NoError(t, client.pkt.WriteMessage(packetTypeAttention, nil))

	require.Eventually(t, func() bool { return len(fixture.fake.receivedRequests()) == 1 },
		10*time.Second, 20*time.Millisecond)

	assert.Equal(t, packetTypeAttention, fixture.fake.receivedRequests()[0].msgType)
}

func TestSessionClosesTheConnectionRowOnDisconnect(t *testing.T) {
	t.Parallel()

	fixture := newAuthFixture(t, encryptNotSup, "disable")

	client, outcome := fixture.login(t, fixtureUser, fixturePassword, fixtureDBEntry)
	require.True(t, outcome.Acked)

	require.NoError(t, client.conn.Close())

	require.Eventually(t, func() bool {
		connections, err := fixture.store.ListConnections(context.Background(), store.ConnectionFilter{Limit: 10})

		return err == nil && len(connections) == 1 && connections[0].DisconnectedAt != nil
	}, 10*time.Second, 50*time.Millisecond)
}

// TestSessionWritesACapture covers the dump plumbing: a pcapng file per
// session, holding the post-auth TDS packets in plaintext because the tap sits
// on the codec rather than on the socket.
func TestSessionWritesACapture(t *testing.T) {
	t.Parallel()

	dumpDir := t.TempDir()

	fixture := newAuthFixtureWithDump(t, encryptNotSup, "disable", dumpDir)

	client, outcome := fixture.login(t, fixtureUser, fixturePassword, fixtureDBEntry)
	require.True(t, outcome.Acked)

	require.NoError(t, client.pkt.WriteMessage(packetTypeSQLBatch, sqlBatchPayload("SELECT 1")))
	client.readReply(t)

	require.NoError(t, client.conn.Close())

	var captures []string

	require.Eventually(t, func() bool {
		found, err := filepath.Glob(filepath.Join(dumpDir, "*.pcapng"))
		if err != nil || len(found) != 1 {
			return false
		}

		info, err := os.Stat(found[0])
		if err != nil || info.Size() == 0 {
			return false
		}

		captures = found

		return true
	}, 10*time.Second, 50*time.Millisecond)

	assert.Len(t, captures, 1)
}
