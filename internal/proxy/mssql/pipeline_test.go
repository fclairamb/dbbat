package mssql

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/store"
)

// countingResponse answers the index-th request with a DONE whose row count is
// index+1, so each query row can be matched to the response it was given.
func countingResponse(index int, _ fakeRequest) []byte {
	return buildDoneToken(doneCount, uint64(index+1))
}

// waitForQueries waits for n query rows that all carry a row count, and
// returns them in execution order.
func waitForQueries(t *testing.T, fixture *authFixture, n int) []store.Query {
	t.Helper()

	var queries []store.Query

	require.Eventually(t, func() bool {
		got, err := fixture.store.ListQueries(context.Background(), store.QueryFilter{Limit: 10})
		if err != nil || len(got) != n {
			return false
		}

		for _, query := range got {
			if query.RowsAffected == nil {
				return false
			}
		}

		queries = got

		return true
	}, 15*time.Second, 50*time.Millisecond)

	sort.Slice(queries, func(i, j int) bool { return queries[i].ExecutedAt.Before(queries[j].ExecutedAt) })

	return queries
}

// pipelineFixture logs a client in against a fake upstream that holds its
// answer to the first request until release is closed.
func pipelineFixture(t *testing.T) (*authFixture, *testClient, chan struct{}) {
	t.Helper()

	fixture := newAuthFixture(t, encryptNotSup, "disable")

	release := make(chan struct{})

	fixture.fake.mu.Lock()
	fixture.fake.respond = func(index int, req fakeRequest) []byte {
		if index == 0 {
			<-release
		}

		return countingResponse(index, req)
	}
	fixture.fake.mu.Unlock()

	client, outcome := fixture.login(t, fixtureUser, fixturePassword, fixtureDBEntry)
	require.True(t, outcome.Acked)

	require.NoError(t, client.conn.SetDeadline(time.Now().Add(60*time.Second)))

	return fixture, client, release
}

// assertPipelinedPair sends first and second back to back, before the first
// response, and checks each lands in the history with its own text and its own
// response.
func assertPipelinedPair(
	t *testing.T, firstType byte, first []byte, firstText string, second []byte, secondText string,
) {
	t.Helper()

	fixture, client, release := pipelineFixture(t)

	require.NoError(t, client.pkt.WriteMessage(firstType, first))
	require.NoError(t, client.pkt.WriteMessage(packetTypeSQLBatch, second))

	// The second request must wait at the proxy for the first one's answer.
	time.Sleep(300 * time.Millisecond)
	assert.Len(t, fixture.fake.receivedRequests(), 1,
		"a request must not reach the upstream while the previous one is unanswered")

	close(release)

	assert.Equal(t, buildDoneToken(doneCount, 1), client.readReply(t))
	assert.Equal(t, buildDoneToken(doneCount, 2), client.readReply(t))

	queries := waitForQueries(t, fixture, 2)

	assert.Contains(t, queries[0].SQLText, firstText)
	assert.Equal(t, int64(1), *queries[0].RowsAffected, "the first statement is answered by the first response")
	assert.Contains(t, queries[1].SQLText, secondText)
	assert.Equal(t, int64(2), *queries[1].RowsAffected, "the second statement is answered by the second response")
}

// TestPipelinedBatchesAreEachLogged is the regression test for a client that
// sends a second SQLBatch before the first one's response: without the proxy
// serializing them, the first response was logged under the second statement's
// text and the second statement was not logged at all.
func TestPipelinedBatchesAreEachLogged(t *testing.T) {
	t.Parallel()

	assertPipelinedPair(t,
		packetTypeSQLBatch, sqlBatchPayload("DELETE FROM employés WHERE id = 1"), "DELETE FROM employés",
		sqlBatchPayload("SELECT 2"), "SELECT 2")
}

// TestPipelinedRPCThenBatchAreEachLogged is the same with an RPC first.
func TestPipelinedRPCThenBatchAreEachLogged(t *testing.T) {
	t.Parallel()

	assertPipelinedPair(t,
		packetTypeRPC, rpcByID(spExecuteSQL, nvarcharMaxParam("", "UPDATE t SET x = 1")), "UPDATE t SET x = 1",
		sqlBatchPayload("SELECT 2"), "SELECT 2")
}

// TestAttentionOvertakesAPipelinedRequest proves the cancel path survives the
// serialization: an ATTENTION sent while a statement runs reaches the upstream
// at once, ahead of a request the client queued behind that statement.
func TestAttentionOvertakesAPipelinedRequest(t *testing.T) {
	t.Parallel()

	fixture := newAuthFixture(t, encryptNotSup, "disable")

	fixture.fake.mu.Lock()
	fixture.fake.silent = true
	fixture.fake.mu.Unlock()

	client, outcome := fixture.login(t, fixtureUser, fixturePassword, fixtureDBEntry)
	require.True(t, outcome.Acked)

	require.NoError(t, client.pkt.WriteMessage(packetTypeSQLBatch, sqlBatchPayload("WAITFOR DELAY '01:00:00'")))
	require.NoError(t, client.pkt.WriteMessage(packetTypeSQLBatch, sqlBatchPayload("SELECT 2")))
	require.NoError(t, client.pkt.WriteMessage(packetTypeAttention, nil))

	require.Eventually(t, func() bool { return len(fixture.fake.receivedRequests()) == 2 },
		10*time.Second, 20*time.Millisecond)

	time.Sleep(300 * time.Millisecond)

	got := fixture.fake.receivedRequests()
	require.Len(t, got, 2, "the queued request must still wait for the response")
	assert.Equal(t, packetTypeSQLBatch, got[0].msgType)
	assert.Equal(t, packetTypeAttention, got[1].msgType, "the cancel must reach the upstream at once")
}

// TestPipelinedRequestWaitsForTheAttentionAck covers a cancel whose
// acknowledgement arrives as a message of its own: the request after it must
// not be sent ahead of the DONE_ATTN, or the acknowledgement would be taken for
// that request's response.
func TestPipelinedRequestWaitsForTheAttentionAck(t *testing.T) {
	t.Parallel()

	fixture := newAuthFixture(t, encryptNotSup, "disable")

	fixture.fake.mu.Lock()
	fixture.fake.respond = countingResponse
	fixture.fake.mu.Unlock()

	client, outcome := fixture.login(t, fixtureUser, fixturePassword, fixtureDBEntry)
	require.True(t, outcome.Acked)

	require.NoError(t, client.conn.SetDeadline(time.Now().Add(60*time.Second)))

	// The fake answers the ATTENTION with its own DONE_ATTN message.
	require.NoError(t, client.pkt.WriteMessage(packetTypeAttention, nil))
	require.NoError(t, client.pkt.WriteMessage(packetTypeSQLBatch, sqlBatchPayload("SELECT 2")))

	assert.Equal(t, buildAttentionAck(), client.readReply(t))
	assert.Equal(t, buildDoneToken(doneCount, 1), client.readReply(t))

	queries := waitForQueries(t, fixture, 1)
	assert.Equal(t, "SELECT 2", queries[0].SQLText)
	assert.Equal(t, int64(1), *queries[0].RowsAffected,
		"the statement is answered by its own response, not by the cancel acknowledgement")
}

// TestAlternatingRequestsAreLoggedAsBefore is the positive control: a client
// that waits for each response is recorded exactly as it always was.
func TestAlternatingRequestsAreLoggedAsBefore(t *testing.T) {
	t.Parallel()

	fixture := newAuthFixture(t, encryptNotSup, "disable")

	fixture.fake.mu.Lock()
	fixture.fake.respond = countingResponse
	fixture.fake.mu.Unlock()

	client, outcome := fixture.login(t, fixtureUser, fixturePassword, fixtureDBEntry)
	require.True(t, outcome.Acked)

	assert.Equal(t, buildDoneToken(doneCount, 1),
		fixture.sendAndRead(t, client, packetTypeSQLBatch, sqlBatchPayload("SELECT 1")))
	assert.Equal(t, buildDoneToken(doneCount, 2),
		fixture.sendAndRead(t, client, packetTypeRPC, rpcByID(spExecuteSQL, nvarcharMaxParam("", "SELECT 2"))))

	queries := waitForQueries(t, fixture, 2)

	assert.Equal(t, "SELECT 1", queries[0].SQLText)
	assert.Equal(t, int64(1), *queries[0].RowsAffected)
	assert.Contains(t, queries[1].SQLText, "SELECT 2")
	assert.Equal(t, int64(2), *queries[1].RowsAffected)
}

// TestClientGoneWhileARequestWaitsItsTurnEndsTheSession is the regression test
// for a pump left reading a nil queue: a client that pipelines a second request
// and hangs up before the first one's answer had the pump forward the second
// once the first was answered, then park for good, and the session never ended.
func TestClientGoneWhileARequestWaitsItsTurnEndsTheSession(t *testing.T) {
	t.Parallel()

	fixture, client, release := pipelineFixture(t)

	require.NoError(t, client.pkt.WriteMessage(packetTypeSQLBatch, sqlBatchPayload("SELECT 1")))
	require.NoError(t, client.pkt.WriteMessage(packetTypeSQLBatch, sqlBatchPayload("SELECT 2")))

	require.Eventually(t, func() bool { return len(fixture.fake.receivedRequests()) == 1 },
		15*time.Second, 20*time.Millisecond, "the first request must have been forwarded upstream")

	require.NoError(t, client.conn.Close())

	// Let the reader see the close before the first answer frees the turn.
	time.Sleep(200 * time.Millisecond)
	close(release)

	fixture.waitForClosedConnection(t)
}

// TestUpstreamGoneWhileARequestWaitsItsTurnEndsTheSession checks the other
// side: an upstream that hangs up instead of answering ends the session, even
// with a request parked behind the unanswered one.
func TestUpstreamGoneWhileARequestWaitsItsTurnEndsTheSession(t *testing.T) {
	t.Parallel()

	fixture := newAuthFixture(t, encryptNotSup, "disable")

	release := make(chan struct{})

	fixture.fake.mu.Lock()
	fixture.fake.respond = func(index int, req fakeRequest) []byte {
		if index == 0 {
			<-release

			return nil // hang up
		}

		return countingResponse(index, req)
	}
	fixture.fake.mu.Unlock()

	client, outcome := fixture.login(t, fixtureUser, fixturePassword, fixtureDBEntry)
	require.True(t, outcome.Acked)

	require.NoError(t, client.conn.SetDeadline(time.Now().Add(60*time.Second)))

	require.NoError(t, client.pkt.WriteMessage(packetTypeSQLBatch, sqlBatchPayload("SELECT 1")))
	require.NoError(t, client.pkt.WriteMessage(packetTypeSQLBatch, sqlBatchPayload("SELECT 2")))

	require.Eventually(t, func() bool { return len(fixture.fake.receivedRequests()) == 1 },
		15*time.Second, 20*time.Millisecond, "the first request must have been forwarded upstream")

	time.Sleep(200 * time.Millisecond)
	close(release)

	fixture.waitForClosedConnection(t)

	assert.Len(t, fixture.fake.receivedRequests(), 1,
		"the parked request must not be forwarded once the upstream is gone")
}
