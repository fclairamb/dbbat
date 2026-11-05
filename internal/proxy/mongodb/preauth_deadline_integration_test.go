//go:build integration

package mongodb

import (
	"context"
	"crypto/tls"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/event"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// TestIntegration_PreAuthDeadlineSparesAuthenticatedIdleSession is the
// positive control for the pre-auth window: an authenticated pooled connection
// that sits idle for longer than the window is not closed, because the
// deadline is lifted once saslStart succeeds. The driver's monitoring
// connection, which never authenticates, survives too as long as it polls
// hello inside the window — hence the 500ms heartbeat (the driver's minimum).
func TestIntegration_PreAuthDeadlineSparesAuthenticatedIdleSession(t *testing.T) {
	ctx := context.Background()
	f := setupFixture(ctx, t)

	const window = 1500 * time.Millisecond

	f.proxy.preAuth.Set(window)

	// A closed pooled connection would be redialled and hide the failure, so
	// the assertion is on the pool: nothing closed, reads not retried.
	var closed atomic.Int32

	monitor := &event.PoolMonitor{Event: func(e *event.PoolEvent) {
		if e.Type == event.ConnectionClosed {
			closed.Add(1)
		}
	}}

	opts := options.Client().
		SetHosts([]string{f.proxyAddr}).
		SetAuth(options.Credential{
			AuthMechanism: "PLAIN",
			AuthSource:    testDBName,
			Username:      fixtureUser,
			Password:      fixturePass,
		}).
		SetTLSConfig(&tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}). //nolint:gosec // self-signed proxy cert
		SetDirect(true).
		SetHeartbeatInterval(500 * time.Millisecond).
		SetMaxPoolSize(1).
		SetPoolMonitor(monitor).
		SetRetryReads(false).
		SetRetryWrites(false).
		SetServerSelectionTimeout(8 * time.Second).
		SetTimeout(10 * time.Second)

	client, err := mongo.Connect(opts)
	require.NoError(t, err)

	defer func() { _ = client.Disconnect(ctx) }()

	coll := client.Database(testDBName).Collection("preauth_idle")

	_, err = coll.InsertOne(ctx, bson.D{{Key: "n", Value: 1}})
	require.NoError(t, err)

	time.Sleep(3 * window)

	n, err := coll.CountDocuments(ctx, bson.D{})
	require.NoError(t, err)
	require.Equal(t, int64(1), n)
	require.Zero(t, closed.Load(), "the authenticated connection must survive the idle pause")
}
