//go:build integration

package postgresql

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/cache"
	"github.com/fclairamb/dbbat/internal/store"
)

// remoteStore opens a *second* handle onto the fixture's storage database.
//
// This is the whole point of these two tests. A store mints its run id in
// memory at construction, so this handle is a different run — a different
// replica, as far as every scoping predicate in the store is concerned — and
// its cache.SessionRegistry is a separate, empty map. Nothing it does can reach
// the live session through the in-process fast path, so if the session drops,
// it dropped because the request crossed the store.
func remoteStore(ctx context.Context, t *testing.T, f *fixture) *store.Store {
	t.Helper()

	remote, err := store.New(ctx, f.storeDSN, store.Options{EncryptionKey: f.encKey})
	require.NoError(t, err)
	t.Cleanup(remote.Close)

	require.NotEqual(t, f.store.RunID(), remote.RunID(),
		"the second handle must be a different run, or this test proves nothing")

	require.False(t, remote.Sessions().Live(uuid.Nil),
		"the second handle's session registry must be its own")

	return remote
}

// pollTerminations is the heartbeat loop's termination poll, called directly.
//
// The loop itself lives in package main and ticks every
// store.TerminationPollInterval; running its body here keeps the test to the
// code under test rather than starting the whole server, and drives it faster
// than the 2s tick so the assertion below is about the mechanism rather than
// about the timer.
func pollTerminations(ctx context.Context, t *testing.T, s *store.Store) {
	t.Helper()

	pending, err := s.PendingTerminations(ctx)
	require.NoError(t, err)

	for _, p := range pending {
		s.Sessions().Terminate(p.ConnectionUID, cache.TerminationRequest{
			Reason: p.Reason,
			By:     p.RequestedBy,
			Detail: p.Detail,
		})
	}
}

// pollUntilSessionDrops runs the poll on the *serving* store until the client's
// statement returns, or the deadline passes.
//
// It stands in for the heartbeat loop that a running dbbat process has. The
// request itself was written by another handle entirely.
func pollUntilSessionDrops(ctx context.Context, t *testing.T, f *fixture, done <-chan error) error {
	t.Helper()

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	deadline := time.After(15 * time.Second)

	for {
		select {
		case err := <-done:
			return err
		case <-ticker.C:
			pollTerminations(ctx, t, f.store)
		case <-deadline:
			t.Fatal("the session did not drop within 15s of the termination being requested")

			return nil
		}
	}
}

// liveConnectionUID waits for the session's connection row to exist and returns
// its uid — the only identifier an admin has, and the key the whole mechanism
// is built on.
func liveConnectionUID(ctx context.Context, t *testing.T, f *fixture) uuid.UUID {
	t.Helper()

	var uid uuid.UUID

	require.Eventually(t, func() bool {
		conns, err := f.store.ListConnections(ctx, store.ConnectionFilter{ActiveOnly: true})
		if err != nil || len(conns) == 0 {
			return false
		}

		uid = conns[0].UID

		return true
	}, 10*time.Second, 100*time.Millisecond, "no live connection row appeared")

	return uid
}

// TestIntegration_TerminateConnection_CrossInstance is the feature: an admin
// ends one live session, from a replica that is not the one serving it.
//
// The termination is requested through a second store handle carrying a
// different run id, so the in-process registry on that handle is empty and
// cannot be what worked. Only the request row crosses, and the serving
// process's poll is what picks it up.
func TestIntegration_TerminateConnection_CrossInstance(t *testing.T) {
	ctx := context.Background()
	f := setupFixture(ctx, t)

	conn := f.mustConnect(ctx, fixturePass)

	var warmup int

	require.NoError(t, conn.QueryRow(ctx, "SELECT 1").Scan(&warmup))

	connectionUID := liveConnectionUID(ctx, t, f)

	// Park a long statement upstream, so the teardown has a real backend to
	// cancel rather than an idle session to close.
	done := make(chan error, 1)

	go func() {
		_, err := conn.Exec(context.Background(), "SELECT pg_sleep(30)")
		done <- err
	}()

	// Give the sleep time to reach the upstream.
	time.Sleep(500 * time.Millisecond)

	remote := remoteStore(ctx, t, f)

	admin, err := remote.CreateUser(ctx, "terminating-admin", "hash", []string{store.RoleAdmin})
	require.NoError(t, err)

	requested, err := remote.RequestConnectionTermination(ctx, connectionUID, admin.UID, "runaway report")
	require.NoError(t, err)
	require.Equal(t, connectionUID, requested.UID)

	// The remote handle owns no session: signaling its own registry finds
	// nothing, which is exactly the case this test exists for.
	require.False(t, remote.Sessions().Terminate(connectionUID, cache.TerminationRequest{
		Reason: store.TerminationAdminTerminated,
	}), "the requesting replica must not be the one that ends the session")

	started := time.Now()

	execErr := pollUntilSessionDrops(ctx, t, f, done)
	require.Error(t, execErr, "the session should have been torn down under the running statement")

	assert.Less(t, time.Since(started), 10*time.Second,
		"the termination took %s to land", time.Since(started))

	// The backend is gone upstream: the cancel landed, rather than the server
	// finishing the sleep for nobody.
	requireNoUpstreamSleep(ctx, t, f)

	// The paper trail, written by the process that actually tore the session
	// down — the same recording path a statement timeout uses.
	var terminated *store.Connection

	require.Eventually(t, func() bool {
		row, err := f.store.GetConnectionByUID(ctx, connectionUID)
		if err != nil {
			return false
		}

		if row.DisconnectedAt == nil || row.TerminationReason == nil {
			return false
		}

		terminated = row

		return true
	}, 15*time.Second, 250*time.Millisecond, "the terminated session was never closed")

	assert.Equal(t, store.TerminationAdminTerminated, *terminated.TerminationReason)
	require.NotNil(t, terminated.TerminateRequestedBy)
	assert.Equal(t, admin.UID, *terminated.TerminateRequestedBy)

	// And the chained audit entry, which survives even a DELETE of the
	// connection row.
	require.Eventually(t, func() bool {
		eventType := store.AuditEventConnectionTerminated

		events, err := f.store.ListAuditEvents(ctx, store.AuditFilter{EventType: &eventType})
		if err != nil {
			return false
		}

		for i := range events {
			details := string(events[i].Details)
			if strings.Contains(details, connectionUID.String()) &&
				strings.Contains(details, store.TerminationAdminTerminated) &&
				strings.Contains(details, "terminating-admin") {
				return true
			}
		}

		return false
	}, 15*time.Second, 250*time.Millisecond,
		"no connection.terminated audit entry names the admin who asked")
}

// TestIntegration_RevokeGrant_CrossInstance is the bug half of this spec: until
// the poller's second arm existed, revoking a grant only ever signaled the
// in-process registry, so a session on any other replica kept running until its
// grant expired.
//
// The revocation here goes through a second store handle and never touches the
// serving process's registry, so the drop can only come from the store.
func TestIntegration_RevokeGrant_CrossInstance(t *testing.T) {
	ctx := context.Background()
	f := setupFixture(ctx, t)

	conn := f.mustConnect(ctx, fixturePass)

	var warmup int

	require.NoError(t, conn.QueryRow(ctx, "SELECT 1").Scan(&warmup))

	connectionUID := liveConnectionUID(ctx, t, f)

	row, err := f.store.GetConnectionByUID(ctx, connectionUID)
	require.NoError(t, err)
	require.NotNil(t, row.GrantUID, "the session must be stamped with its grant")

	done := make(chan error, 1)

	go func() {
		_, execErr := conn.Exec(context.Background(), "SELECT pg_sleep(30)")
		done <- execErr
	}()

	time.Sleep(500 * time.Millisecond)

	remote := remoteStore(ctx, t, f)

	// Through the store only — deliberately *not* through the API handler,
	// which would also signal its own registry. This is what a revoke on
	// another replica looks like from here.
	require.NoError(t, remote.RevokeGrant(ctx, *row.GrantUID, f.user.UID))
	require.Zero(t, remote.Revocations().Revoke(*row.GrantUID),
		"the revoking replica owns no session under this grant")

	execErr := pollUntilSessionDrops(ctx, t, f, done)
	require.Error(t, execErr, "a revoked grant must end the session on every replica, not just the one that revoked")

	requireNoUpstreamSleep(ctx, t, f)

	require.Eventually(t, func() bool {
		closed, err := f.store.GetConnectionByUID(ctx, connectionUID)
		if err != nil {
			return false
		}

		return closed.DisconnectedAt != nil &&
			closed.TerminationReason != nil &&
			*closed.TerminationReason == store.TerminationGrantRevoked
	}, 15*time.Second, 250*time.Millisecond,
		"the session was not recorded as ended by the revocation")
}
