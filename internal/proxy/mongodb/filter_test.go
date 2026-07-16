package mongodb

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/fclairamb/dbbat/internal/store"
)

// TestFilterListDatabasesReply verifies a listDatabases reply is filtered to the
// grant's database, recomputing totalSize and preserving the ok field (item 5).
func TestFilterListDatabasesReply(t *testing.T) {
	t.Parallel()

	s := &Session{database: &store.Server{DatabaseName: "app"}}

	replyDoc := bson.D{
		{Key: "databases", Value: bson.A{
			bson.D{{Key: "name", Value: "app"}, {Key: "sizeOnDisk", Value: int64(2048)}, {Key: "empty", Value: false}},
			bson.D{{Key: "name", Value: "admin"}, {Key: "sizeOnDisk", Value: int64(4096)}, {Key: "empty", Value: false}},
			bson.D{{Key: "name", Value: "other"}, {Key: "sizeOnDisk", Value: int64(8192)}, {Key: "empty", Value: false}},
		}},
		{Key: "totalSize", Value: float64(14336)},
		{Key: "totalSizeMb", Value: float64(0)},
		{Key: "ok", Value: 1.0},
	}

	raw, err := buildOpMsgReply(5, 6, replyDoc)
	require.NoError(t, err)

	m, err := readMessage(bytes.NewReader(raw))
	require.NoError(t, err)

	rewritten, ok := s.filterListDatabasesReply(m)
	require.True(t, ok)

	rm, err := readMessage(bytes.NewReader(rewritten))
	require.NoError(t, err)

	parsed, err := parseOpMsg(rm.body)
	require.NoError(t, err)

	body, ok := parsed.commandBody()
	require.True(t, ok)

	arr, ok := body.Lookup("databases").ArrayOK()
	require.True(t, ok)

	values, err := arr.Values()
	require.NoError(t, err)
	require.Len(t, values, 1, "only the grant database should remain")

	kept, ok := values[0].DocumentOK()
	require.True(t, ok)
	assert.Equal(t, "app", lookupString(kept, "name"))

	total, ok := lookupInt64(body, "totalSize")
	require.True(t, ok)
	assert.Equal(t, int64(2048), total, "totalSize recomputed from kept entries")

	assert.True(t, replyOK(body), "ok field preserved")
}

// TestFilterListDatabasesReplyErrorReply verifies an error reply (no databases
// array) is left untouched so it relays verbatim.
func TestFilterListDatabasesReplyErrorReply(t *testing.T) {
	t.Parallel()

	s := &Session{database: &store.Server{DatabaseName: "app"}}

	raw, err := buildOpMsgReply(1, 2, bson.D{{Key: "ok", Value: 0.0}, {Key: "errmsg", Value: "nope"}})
	require.NoError(t, err)

	m, err := readMessage(bytes.NewReader(raw))
	require.NoError(t, err)

	_, ok := s.filterListDatabasesReply(m)
	assert.False(t, ok, "error replies are not rewritten")
}
