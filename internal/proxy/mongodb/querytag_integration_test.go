//go:build integration

package mongodb

import (
	"context"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/fclairamb/dbbat/internal/store"
)

// tagMarker is what every tagged command's comment starts with. The values
// vary with the fixture (version, username, connection uid, grant slug), so
// the assertions check the marker plus the fields that must be present.
const tagMarker = "dbbat='"

// setupTaggedFixture is setupFixture with DBB_QUERY_TAGGING on. The flag is an
// atomic on the server, so turning it on after the listener started is safe —
// and no session exists yet at this point anyway.
func setupTaggedFixture(ctx context.Context, t *testing.T) *fixture {
	t.Helper()

	f := setupFixture(ctx, t)
	f.proxy.SetQueryTagging(true)

	return f
}

// dialUpstreamDirect connects to the mongod container as root, bypassing
// dbbat. Every assertion about what the *target* saw goes through this: the
// point of the tag is what the target's own instrumentation reports, so
// trusting the proxy's account of it would prove nothing.
func (f *fixture) dialUpstreamDirect(ctx context.Context) *mongo.Client {
	f.t.Helper()

	client, err := mongo.Connect(options.Client().
		SetHosts([]string{net.JoinHostPort(f.upstreamHost, strconv.Itoa(f.upstreamPort))}).
		SetDirect(true).
		SetServerSelectionTimeout(10 * time.Second).
		SetAuth(options.Credential{Username: rootUser, Password: rootPass, AuthSource: "admin"}))
	require.NoError(f.t, err)

	f.t.Cleanup(func() { _ = client.Disconnect(context.WithoutCancel(ctx)) })

	return client
}

// enableProfiler turns on `system.profile` for every operation on testdb —
// the MongoDB counterpart of MySQL's performance_schema and the very tool
// (Atlas exposes it as the Query Profiler) the tag exists to feed.
//
// Set directly on the container rather than through dbbat: profiling is an
// admin knob, not part of what this test is exercising.
func (f *fixture) enableProfiler(ctx context.Context, direct *mongo.Client) {
	f.t.Helper()

	require.NoError(f.t, direct.Database(testDBName).
		RunCommand(ctx, bson.D{{Key: "profile", Value: 2}}).Err())

	f.t.Cleanup(func() {
		_ = direct.Database(testDBName).
			RunCommand(context.WithoutCancel(ctx), bson.D{{Key: "profile", Value: 0}}).Err()
	})
}

// profiledCommand waits for the profiler entry of the command whose filter
// carries marker, and returns the command document the *server* recorded.
func (f *fixture) profiledCommand(ctx context.Context, direct *mongo.Client, marker string) bson.Raw {
	f.t.Helper()

	var recorded bson.Raw

	require.Eventually(f.t, func() bool {
		res := direct.Database(testDBName).Collection("system.profile").FindOne(ctx, bson.D{
			{Key: "command.filter.marker", Value: marker},
		})
		if res.Err() != nil {
			return false
		}

		var entry bson.Raw
		if err := res.Decode(&entry); err != nil {
			return false
		}

		cmd, ok := entry.Lookup("command").DocumentOK()
		if !ok {
			return false
		}

		recorded = cmd

		return true
	}, 15*time.Second, 200*time.Millisecond,
		"system.profile never showed a find carrying marker %q", marker)

	return recorded
}

// TestIntegration_QueryTagging_UpstreamSeesTagStoreDoesNot is the spec's
// central assertion, read from the target's own profiler: `system.profile`
// holds the command with dbbat's `comment`, and everything dbbat persists
// holds the command the client sent.
func TestIntegration_QueryTagging_UpstreamSeesTagStoreDoesNot(t *testing.T) {
	ctx := context.Background()

	f := setupTaggedFixture(ctx, t)
	direct := f.dialUpstreamDirect(ctx)
	f.enableProfiler(ctx, direct)

	client := f.dialThrough(fixtureUser, fixturePass)
	defer func() { _ = client.Disconnect(ctx) }()

	const marker = "tagme"

	coll := client.Database(testDBName).Collection("widgets")
	_, err := coll.InsertOne(ctx, bson.D{{Key: "marker", Value: marker}})
	require.NoError(t, err)

	cursor, err := coll.Find(ctx, bson.D{{Key: "marker", Value: marker}})
	require.NoError(t, err)

	var docs []bson.M
	require.NoError(t, cursor.All(ctx, &docs))
	require.Len(t, docs, 1)

	upstreamSaw := f.profiledCommand(ctx, direct, marker)

	comment, ok := upstreamSaw.Lookup("comment").StringValueOK()
	require.True(t, ok, "the upstream recorded no string comment: %v", upstreamSaw)
	assert.True(t, strings.HasPrefix(comment, tagMarker),
		"the upstream must have received the tag, got %q", comment)
	assert.Contains(t, comment, "user='"+fixtureUser+"'")

	// The conn= value must be this session's own connection uid suffix.
	conns, err := f.store.ListConnections(ctx, store.ConnectionFilter{Limit: 10})
	require.NoError(t, err)
	require.NotEmpty(t, conns)

	hex := strings.ReplaceAll(conns[0].UID.String(), "-", "")
	assert.Contains(t, comment, "conn='"+hex[len(hex)-12:]+"'")

	// Nothing dbbat stored may carry it.
	require.Eventually(t, func() bool {
		queries, err := f.store.ListQueries(ctx, store.QueryFilter{Limit: 200})
		if err != nil {
			return false
		}

		for i := range queries {
			if strings.HasPrefix(queries[i].SQLText, "find ") && strings.Contains(queries[i].SQLText, marker) {
				return true
			}
		}

		return false
	}, 15*time.Second, 200*time.Millisecond,
		"the find was never recorded with the client's own command")

	queries, err := f.store.ListQueries(ctx, store.QueryFilter{Limit: 200})
	require.NoError(t, err)

	for i := range queries {
		assert.NotContains(t, queries[i].SQLText, tagMarker,
			"a stored command carries the tag — history is now proxy-rewritten")
		assert.NotContains(t, queries[i].SQLText, `"comment"`,
			"a stored command carries a comment dbbat added")
	}
}

// TestIntegration_QueryTagging_ClientCommentWins: a client that sets its own
// `comment` keeps it byte-for-byte. It is single-valued and someone else's
// tracing depends on it, which is why dbbat skips the command rather than
// appending to or overwriting the value.
func TestIntegration_QueryTagging_ClientCommentWins(t *testing.T) {
	ctx := context.Background()

	f := setupTaggedFixture(ctx, t)
	direct := f.dialUpstreamDirect(ctx)
	f.enableProfiler(ctx, direct)

	client := f.dialThrough(fixtureUser, fixturePass)
	defer func() { _ = client.Disconnect(ctx) }()

	const (
		marker        = "clientcomment"
		clientComment = "trace-id=abc123"
	)

	// RunCommand rather than a typed helper: the comment has to be on the wire
	// exactly as a client's own tracing would put it there.
	err := client.Database(testDBName).RunCommand(ctx, bson.D{
		{Key: "find", Value: "widgets"},
		{Key: "filter", Value: bson.D{{Key: "marker", Value: marker}}},
		{Key: "comment", Value: clientComment},
	}).Err()
	require.NoError(t, err)

	upstreamSaw := f.profiledCommand(ctx, direct, marker)

	comment, ok := upstreamSaw.Lookup("comment").StringValueOK()
	require.True(t, ok, "the client's comment is gone: %v", upstreamSaw)
	assert.Equal(t, clientComment, comment,
		"dbbat overwrote or appended to the client's own comment")
}

// TestIntegration_QueryTagging_DisabledLeavesBytesUnchanged — with the feature
// off the upstream receives exactly what it received before it existed.
func TestIntegration_QueryTagging_DisabledLeavesBytesUnchanged(t *testing.T) {
	ctx := context.Background()

	f := setupFixture(ctx, t)
	direct := f.dialUpstreamDirect(ctx)
	f.enableProfiler(ctx, direct)

	client := f.dialThrough(fixtureUser, fixturePass)
	defer func() { _ = client.Disconnect(ctx) }()

	const marker = "untagged"

	cursor, err := client.Database(testDBName).Collection("widgets").
		Find(ctx, bson.D{{Key: "marker", Value: marker}})
	require.NoError(t, err)
	require.NoError(t, cursor.Close(ctx))

	upstreamSaw := f.profiledCommand(ctx, direct, marker)

	assert.Equal(t, bson.Type(0), upstreamSaw.Lookup("comment").Type,
		"a comment was added with tagging off: %v", upstreamSaw)
}

// TestIntegration_QueryTagging_ControlsUnchanged — a read_only refusal is the
// same refusal with tagging on as with it off, because every control matched
// the client's command either way.
func TestIntegration_QueryTagging_ControlsUnchanged(t *testing.T) {
	ctx := context.Background()

	refusal := func(tagging bool) string {
		f := setupFixture(ctx, t)
		f.proxy.SetQueryTagging(tagging)
		f.replaceGrant(ctx, []string{"read_only"})

		client := f.dialThrough(fixtureUser, fixturePass)
		defer func() { _ = client.Disconnect(ctx) }()

		_, err := client.Database(testDBName).Collection("widgets").
			InsertOne(ctx, bson.D{{Key: "x", Value: 1}})
		require.Error(t, err, "a read_only grant must refuse a write, tagging or not")

		return err.Error()
	}

	assert.Equal(t, refusal(false), refusal(true),
		"the refusal text must not depend on whether tagging is on")
}

// TestIntegration_QueryTagging_AuditChainVerifies should be a non-event — the
// chain never saw a tagged command — but a MAC computed over rewritten text
// would still verify against itself, so what this catches is the chain being
// built over bytes the client never sent.
func TestIntegration_QueryTagging_AuditChainVerifies(t *testing.T) {
	ctx := context.Background()

	f := setupTaggedFixture(ctx, t)
	client := f.dialThrough(fixtureUser, fixturePass)

	coll := client.Database(testDBName).Collection("widgets")

	for i := range 5 {
		_, err := coll.InsertOne(ctx, bson.D{{Key: "n", Value: i}})
		require.NoError(t, err)

		cursor, err := coll.Find(ctx, bson.D{{Key: "n", Value: i}})
		require.NoError(t, err)
		require.NoError(t, cursor.Close(ctx))
	}

	require.NoError(t, client.Disconnect(ctx))

	require.Eventually(t, func() bool {
		res, err := f.store.VerifyQueryChains(ctx, nil)

		return err == nil && res.Connections > 0 && res.Verified > 0
	}, 20*time.Second, 200*time.Millisecond, "no chained command was ever written")

	res, err := f.store.VerifyQueryChains(ctx, nil)
	require.NoError(t, err)
	assert.True(t, res.OK(), "query chain break with tagging on: %+v", res.Break)

	auditRes, err := f.store.VerifyAuditChain(ctx)
	require.NoError(t, err)
	assert.True(t, auditRes.OK(), "audit chain break with tagging on: %+v", auditRes.Break)
}
