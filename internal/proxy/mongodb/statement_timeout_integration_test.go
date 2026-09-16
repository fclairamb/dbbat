//go:build integration

package mongodb

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/fclairamb/dbbat/internal/proxy/testsupport"
	"github.com/fclairamb/dbbat/internal/store"
)

// replaceGrantWithStatementTimeout revokes the fixture's grants and installs
// one whose definition carries a per-statement limit of the given seconds.
func (f *fixture) replaceGrantWithStatementTimeout(ctx context.Context, seconds int64) {
	f.t.Helper()

	grants, err := f.store.ListGrants(ctx, store.GrantFilter{ActiveOnly: true})
	require.NoError(f.t, err)

	for _, g := range grants {
		require.NoError(f.t, f.store.RevokeGrant(ctx, g.UID, f.user.UID))
	}

	dbUID, err := uuid.Parse(f.dbUID)
	require.NoError(f.t, err)

	_, err = testsupport.CreateGrantWithControls(ctx, f.t, f.store, f.user.UID, dbUID,
		nil, testsupport.WithStatementTimeout(seconds))
	require.NoError(f.t, err)
}

// TestIntegration_StatementTimeout_MaxTimeMSInjected is layer 1 on MongoDB:
// there is no session knob, so dbbat injects maxTimeMS into the forwarded
// command and the *server* cancels the operation — MaxTimeMSExpired, with the
// session intact.
func TestIntegration_StatementTimeout_MaxTimeMSInjected(t *testing.T) {
	ctx := context.Background()
	f := setupFixture(ctx, t)
	f.replaceGrantWithStatementTimeout(ctx, 1)

	client := f.dialThrough(fixtureUser, fixturePass)
	defer func() { _ = client.Disconnect(ctx) }()

	coll := client.Database(testDBName).Collection("widgets")

	_, err := coll.InsertOne(ctx, bson.D{{Key: "name", Value: "slow"}})
	require.NoError(t, err)

	// $where runs server-side JavaScript, which is the only portable way to
	// make a find take measurable wall-clock time on the server.
	cursor, err := coll.Find(ctx, bson.D{{Key: "$where", Value: "sleep(5000) || true"}})
	if err == nil {
		err = cursor.Err()

		if err == nil {
			var docs []bson.M
			err = cursor.All(ctx, &docs)
		}

		_ = cursor.Close(ctx)
	}

	require.Error(t, err, "a 5s $where must not survive a 1s statement limit")
	assert.Contains(t, strings.ToLower(err.Error()), "maxtimems",
		"expected MaxTimeMSExpired, got: %v", err)

	// The session survives: only the operation was canceled.
	require.NoError(t, client.Ping(ctx, nil),
		"the session should survive a server-side maxTimeMS expiry")
}

// TestIntegration_StatementTimeout_ClientMaxTimeMSClamped proves the clamping
// rule in both directions: a client asking for *more* than the grant's limit is
// cut at the limit, while a client asking for less keeps its own tighter value.
func TestIntegration_StatementTimeout_ClientMaxTimeMSClamped(t *testing.T) {
	ctx := context.Background()
	f := setupFixture(ctx, t)
	f.replaceGrantWithStatementTimeout(ctx, 2)

	client := f.dialThrough(fixtureUser, fixturePass)
	defer func() { _ = client.Disconnect(ctx) }()

	coll := client.Database(testDBName).Collection("widgets")

	_, err := coll.InsertOne(ctx, bson.D{{Key: "name", Value: "clamped"}})
	require.NoError(t, err)

	// RunCommand rather than a typed helper: the v2 driver no longer exposes
	// maxTimeMS on the option builders, and this is the value being clamped, so
	// the test has to put it on the wire itself.
	started := time.Now()

	err = client.Database(testDBName).RunCommand(ctx, bson.D{
		{Key: "count", Value: "widgets"},
		{Key: "query", Value: bson.D{{Key: "$where", Value: "sleep(30000) || true"}}},
		{Key: "maxTimeMS", Value: int64(60000)},
	}).Err()

	require.Error(t, err, "a client maxTimeMS above the grant's limit must be clamped")
	assert.Less(t, time.Since(started), 20*time.Second,
		"the clamp did not take: the operation ran for %s", time.Since(started))
	assert.Contains(t, strings.ToLower(err.Error()), "maxtimems",
		"expected MaxTimeMSExpired, got: %v", err)

	require.NoError(t, client.Ping(ctx, nil))

	// And the other direction: a client asking for *less* than the limit keeps
	// its own value, so this fails on the client's 500ms rather than dbbat's 2s.
	tight := time.Now()

	err = client.Database(testDBName).RunCommand(ctx, bson.D{
		{Key: "count", Value: "widgets"},
		{Key: "query", Value: bson.D{{Key: "$where", Value: "sleep(30000) || true"}}},
		{Key: "maxTimeMS", Value: int64(500)},
	}).Err()

	require.Error(t, err)
	assert.Less(t, time.Since(tight), 2*time.Second,
		"a client value tighter than the limit was overwritten: it took %s", time.Since(tight))
}
