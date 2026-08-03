package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInstanceRegistry(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	store.SetInstanceID("registry-a")

	t.Run("register creates the row", func(t *testing.T) {
		require.NoError(t, store.RegisterInstance(ctx))

		inst, err := store.GetInstance(ctx, "registry-a")
		require.NoError(t, err)
		require.NotNil(t, inst)
		assert.WithinDuration(t, time.Now(), inst.LastSeenAt, time.Minute)
		assert.WithinDuration(t, time.Now(), inst.StartedAt, time.Minute)
	})

	t.Run("heartbeat moves last_seen_at without rewriting started_at", func(t *testing.T) {
		backdated := time.Now().Add(-2 * time.Hour).Truncate(time.Microsecond)
		_, err := store.db.ExecContext(ctx,
			"UPDATE instances SET started_at = ?, last_seen_at = ? WHERE instance_id = ?",
			backdated, backdated, "registry-a")
		require.NoError(t, err)

		require.NoError(t, store.HeartbeatInstance(ctx))

		inst, err := store.GetInstance(ctx, "registry-a")
		require.NoError(t, err)
		require.NotNil(t, inst)
		assert.WithinDuration(t, time.Now(), inst.LastSeenAt, time.Minute, "the heartbeat should refresh liveness")
		assert.WithinDuration(t, backdated, inst.StartedAt, time.Millisecond,
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
		before, err := store.GetInstance(ctx, "registry-a")
		require.NoError(t, err)
		require.NotNil(t, before)

		_, err = store.db.ExecContext(ctx,
			"UPDATE instances SET started_at = ? WHERE instance_id = ?",
			time.Now().Add(-72*time.Hour), "registry-a")
		require.NoError(t, err)

		require.NoError(t, store.RegisterInstance(ctx))

		after, err := store.GetInstance(ctx, "registry-a")
		require.NoError(t, err)
		require.NotNil(t, after)
		assert.WithinDuration(t, time.Now(), after.StartedAt, time.Minute)
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

	registerInstanceAs(t, ctx, store, "prune-live", time.Now())
	registerInstanceAs(t, ctx, store, "prune-stale", time.Now().Add(-InstanceStaleAfter-time.Minute))

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
