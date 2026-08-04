package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// looksStale asks the database the same question the reclaim asks: is this
// registry row past the grace period? Evaluated entirely on the database clock,
// so the answer does not depend on the test process's own time.
func looksStale(t *testing.T, ctx context.Context, store *Store, instanceID string) bool {
	t.Helper()

	var stale bool
	err := store.db.QueryRowContext(ctx,
		"SELECT last_seen_at < now() - make_interval(secs => ?) FROM instances WHERE instance_id = ?",
		InstanceStaleAfter.Seconds(), instanceID).Scan(&stale)
	require.NoError(t, err)

	return stale
}

func TestInstanceRegistry(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	store.SetInstanceID("registry-a")

	t.Run("register creates a row that is alive", func(t *testing.T) {
		require.NoError(t, store.RegisterInstance(ctx))

		inst, err := store.GetInstance(ctx, "registry-a")
		require.NoError(t, err)
		require.NotNil(t, inst)
		assert.False(t, looksStale(t, ctx, store, "registry-a"),
			"a just-registered instance must never look dead")
		assert.WithinDuration(t, inst.StartedAt, inst.LastSeenAt, time.Second)
	})

	t.Run("heartbeat revives a stale row without rewriting started_at", func(t *testing.T) {
		// Age the row past the grace period on the database clock, so it is
		// stale by exactly the predicate the reclaim uses.
		_, err := store.db.ExecContext(ctx,
			`UPDATE instances
			 SET started_at = now() - make_interval(secs => ?), last_seen_at = now() - make_interval(secs => ?)
			 WHERE instance_id = ?`,
			(InstanceStaleAfter + time.Hour).Seconds(), (InstanceStaleAfter + time.Hour).Seconds(), "registry-a")
		require.NoError(t, err)
		require.True(t, looksStale(t, ctx, store, "registry-a"), "the row should be stale before the heartbeat")

		before, err := store.GetInstance(ctx, "registry-a")
		require.NoError(t, err)
		require.NotNil(t, before)

		require.NoError(t, store.HeartbeatInstance(ctx))

		inst, err := store.GetInstance(ctx, "registry-a")
		require.NoError(t, err)
		require.NotNil(t, inst)
		assert.False(t, looksStale(t, ctx, store, "registry-a"), "the heartbeat should refresh liveness")
		assert.WithinDuration(t, before.StartedAt, inst.StartedAt, time.Millisecond,
			"the heartbeat must keep describing the run that started")
	})

	t.Run("heartbeat re-creates a row that disappeared", func(t *testing.T) {
		// A missing row means "dead", so losing ours must not be permanent —
		// otherwise a live process's connections stay reclaimable forever.
		require.NoError(t, store.DeregisterInstance(ctx))

		gone, err := store.GetInstance(ctx, "registry-a")
		require.NoError(t, err)
		require.Nil(t, gone)

		require.NoError(t, store.HeartbeatInstance(ctx))

		back, err := store.GetInstance(ctx, "registry-a")
		require.NoError(t, err)
		assert.NotNil(t, back)
	})

	t.Run("register resets started_at on a restart", func(t *testing.T) {
		_, err := store.db.ExecContext(ctx,
			"UPDATE instances SET started_at = now() - make_interval(secs => ?) WHERE instance_id = ?",
			(72 * time.Hour).Seconds(), "registry-a")
		require.NoError(t, err)

		before, err := store.GetInstance(ctx, "registry-a")
		require.NoError(t, err)
		require.NotNil(t, before)

		require.NoError(t, store.RegisterInstance(ctx))

		after, err := store.GetInstance(ctx, "registry-a")
		require.NoError(t, err)
		require.NotNil(t, after)
		assert.True(t, after.StartedAt.After(before.StartedAt.Add(time.Hour)),
			"a restart on the same id must reset started_at, not keep the previous run's")
		assert.WithinDuration(t, after.StartedAt, after.LastSeenAt, time.Second)
	})

	t.Run("an unset instance id registers nothing", func(t *testing.T) {
		// '' is not an identity: registering it would make the legacy
		// ownerless connection rows look alive forever.
		store.SetInstanceID("")
		require.NoError(t, store.RegisterInstance(ctx))
		require.NoError(t, store.HeartbeatInstance(ctx))

		empty, err := store.GetInstance(ctx, "")
		require.NoError(t, err)
		assert.Nil(t, empty)
	})
}

func TestPruneStaleInstances(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	registerInstanceAs(t, ctx, store, "prune-live", 0)
	registerInstanceAs(t, ctx, store, "prune-stale", InstanceStaleAfter+time.Minute)

	pruned, err := store.PruneStaleInstances(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(1), pruned)

	live, err := store.GetInstance(ctx, "prune-live")
	require.NoError(t, err)
	assert.NotNil(t, live, "a heartbeating instance is never pruned")

	stale, err := store.GetInstance(ctx, "prune-stale")
	require.NoError(t, err)
	assert.Nil(t, stale)
}

// TestInstanceStaleAfterIsGenerous pins the safety margin: the grace period is
// what stands between the reclaim and closing a live replica's connections, so
// it must stay a large multiple of the heartbeat rather than a couple of ticks.
func TestInstanceStaleAfterIsGenerous(t *testing.T) {
	assert.GreaterOrEqual(t, InstanceStaleAfter, 20*InstanceHeartbeatInterval,
		"the stale-instance grace period must tolerate a long run of missed heartbeats")
	assert.GreaterOrEqual(t, InstanceStaleAfter, 10*time.Minute,
		"the grace period must also cover a rolling upgrade window")
}
