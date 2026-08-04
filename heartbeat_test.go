package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/store"
)

// TestNextReclaimDelay pins the two properties the periodic reclaim relies on:
// the delay is spread, so replicas started together do not all sweep at the
// same instant, and it is bounded well inside the grace period, so a dead
// instance is never waited on for longer than the reclaim is worth.
func TestNextReclaimDelay(t *testing.T) {
	t.Parallel()

	const draws = 50

	seen := make(map[int64]struct{}, draws)

	for range draws {
		delay := nextReclaimDelay()

		assert.GreaterOrEqual(t, delay, store.InstanceReclaimInterval/2,
			"a reclaim must not fire more than twice as often as the interval")
		assert.Less(t, delay, store.InstanceStaleAfter,
			"a dead instance would otherwise wait more than a full grace period past going stale")

		seen[int64(delay)] = struct{}{}
	}

	require.Greater(t, len(seen), 1,
		"a fixed delay would put every replica's sweep in lockstep, which is what the jitter is for")
}

// TestClassifySharedIDCandidates covers how a live replica sharing our instance
// id is told apart from our own crashed predecessor.
//
// At startup the two are indistinguishable — both leave a fresh registry row
// carrying our id and someone else's run id — so the answer is whether the row
// keeps moving. Getting this wrong in the loud direction would fire the warning
// on every crash-restart of a stable instance id.
func TestClassifySharedIDCandidates(t *testing.T) {
	t.Parallel()

	startup := time.Now().Add(-time.Minute)

	candidates := map[string]time.Time{
		"live-peer":   startup,
		"dead-run":    startup,
		"pruned-run":  startup,
		"another-one": startup,
	}

	peers := []store.Instance{
		// Heartbeated since we sampled: a process, not a corpse.
		{InstanceID: "shared", RunID: "live-peer", LastSeenAt: startup.Add(30 * time.Second)},
		// Frozen at the instant it died, and still inside the grace period.
		{InstanceID: "shared", RunID: "dead-run", LastSeenAt: startup},
		// Registered after we sampled; it warns about us instead.
		{InstanceID: "shared", RunID: "newcomer", LastSeenAt: startup.Add(time.Minute)},
	}

	confirmed, remaining := classifySharedIDCandidates(candidates, peers)

	assert.Equal(t, []string{"live-peer"}, confirmed,
		"only a run whose heartbeat moved is another live process")

	assert.Equal(t, map[string]time.Time{"dead-run": startup}, remaining,
		"a frozen row stays under observation; rows that vanished are settled and dropped")
}
