package main

import (
	"testing"

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
