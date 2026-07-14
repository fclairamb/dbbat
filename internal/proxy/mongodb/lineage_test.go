package mongodb

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func mustRaw(t *testing.T, d bson.D) bson.Raw {
	t.Helper()

	raw, err := bson.Marshal(d)
	require.NoError(t, err)

	return raw
}

// TestCursorLineage walks a find → getMore → drain sequence and asserts the
// shared cursor_id + origin annotations and the map cleanup (item 6).
func TestCursorLineage(t *testing.T) {
	t.Parallel()

	s := &Session{cursorOrigins: make(map[int64]cursorOrigin)}

	// A find reply opens cursor 42.
	findPQ := &pendingQuery{command: "find"}
	findReply := mustRaw(t, bson.D{
		{Key: "cursor", Value: bson.D{
			{Key: "id", Value: int64(42)},
			{Key: "ns", Value: "app.widgets"},
			{Key: "firstBatch", Value: bson.A{}},
		}},
		{Key: "ok", Value: 1.0},
	})
	s.trackCursorFromReply(findPQ, findReply)

	require.NotNil(t, findPQ.params)
	assert.Contains(t, findPQ.params.Values, "cursor_id=42", "origin find carries the cursor id")

	origin, ok := s.getCursorOrigin(42)
	require.True(t, ok)
	assert.Equal(t, "find", origin.command)
	assert.Equal(t, "app.widgets", origin.namespace)

	// A getMore on cursor 42 is linked back to the origin.
	getMorePQ := &pendingQuery{command: "getMore"}
	getMoreCmd := mustRaw(t, bson.D{
		{Key: "getMore", Value: int64(42)},
		{Key: "collection", Value: "widgets"},
		{Key: "$db", Value: "app"},
	})
	s.annotateGetMore(getMorePQ, getMoreCmd)

	assert.Equal(t, int64(42), getMorePQ.cursorID)
	require.NotNil(t, getMorePQ.params)
	assert.Contains(t, getMorePQ.params.Values, "cursor_id=42")
	assert.Contains(t, getMorePQ.params.Values, "cursor_origin=find app.widgets")

	// The getMore reply drains the cursor (id 0): the origin link is dropped.
	drainReply := mustRaw(t, bson.D{
		{Key: "cursor", Value: bson.D{
			{Key: "id", Value: int64(0)},
			{Key: "nextBatch", Value: bson.A{}},
		}},
		{Key: "ok", Value: 1.0},
	})
	s.trackCursorFromReply(getMorePQ, drainReply)

	_, ok = s.getCursorOrigin(42)
	assert.False(t, ok, "drained cursor origin should be forgotten")
}

// TestCursorLineageUnknownCursor verifies a getMore for an untracked cursor
// still records cursor_id but no origin reference.
func TestCursorLineageUnknownCursor(t *testing.T) {
	t.Parallel()

	s := &Session{cursorOrigins: make(map[int64]cursorOrigin)}

	pq := &pendingQuery{command: "getMore"}
	s.annotateGetMore(pq, mustRaw(t, bson.D{{Key: "getMore", Value: int64(99)}, {Key: "collection", Value: "x"}}))

	require.NotNil(t, pq.params)
	assert.Contains(t, pq.params.Values, "cursor_id=99")
	for _, v := range pq.params.Values {
		assert.NotContains(t, v, "cursor_origin=")
	}
}
