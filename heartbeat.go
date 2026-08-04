package main

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/fclairamb/dbbat/internal/store"
)

// instanceHeartbeat keeps this process's row in the instance registry fresh.
//
// The registry is what lets a starting process tell a dead instance's leftover
// connections from a live one's sessions (see store.CloseOrphanedConnections).
// This side of it is the promise the other side relies on: while we are
// running, our row must keep moving. A row that stops moving for
// store.InstanceStaleAfter is an invitation for another instance to close our
// connections, so the heartbeat runs for the entire life of the process and
// only stops once the servers have drained.
//
// The same loop also carries the other side of the bargain, at a much lower
// frequency: every store.InstanceReclaimInterval it reclaims the connections of
// instances the registry proves are gone (see reclaim). Doing it here rather
// than only at startup is what stops a crashed pod's rows sitting open until
// some unrelated process happens to restart.
type instanceHeartbeat struct {
	store    *store.Store
	logger   *slog.Logger
	stop     chan struct{}
	stopOnce sync.Once
}

// startInstanceHeartbeat registers this process and starts refreshing its row.
//
// Registration is synchronous and happens before the caller reconciles or
// accepts any traffic: until the row exists we look dead to every other
// replica, so that window must not overlap with any connection we own. A
// failed registration is logged, not fatal — the proxy serving traffic matters
// more than the bookkeeping, and the first heartbeat tick upserts the row
// anyway.
//
// Returns nil when there is no instance id to register, which also disables the
// reconcile; there is then nothing to keep alive.
func startInstanceHeartbeat(ctx context.Context, dataStore *store.Store, logger *slog.Logger) *instanceHeartbeat {
	if dataStore.InstanceID() == "" {
		return nil
	}

	if err := dataStore.RegisterInstance(ctx); err != nil {
		logger.ErrorContext(ctx, "failed to register this instance",
			slog.Any("error", err),
			slog.String("instance_id", dataStore.InstanceID()))
	}

	beat := &instanceHeartbeat{
		store:  dataStore,
		logger: logger,
		stop:   make(chan struct{}),
	}

	go beat.run(ctx)

	logger.InfoContext(ctx, "Instance registered",
		slog.String("instance_id", dataStore.InstanceID()),
		slog.Duration("heartbeat_interval", store.InstanceHeartbeatInterval),
		slog.Duration("stale_after", store.InstanceStaleAfter))

	return beat
}

func (h *instanceHeartbeat) run(ctx context.Context) {
	ticker := time.NewTicker(store.InstanceHeartbeatInterval)
	defer ticker.Stop()

	// The reclaim rides along on this goroutine rather than getting one of its
	// own: it is the same "keep the registry honest" job seen from the other
	// side, and it must stop for exactly the same reasons.
	reclaim := time.NewTimer(nextReclaimDelay())
	defer reclaim.Stop()

	for {
		select {
		case <-ticker.C:
			h.beat(ctx)
		case <-reclaim.C:
			h.reclaim(ctx)
			reclaim.Reset(nextReclaimDelay())
		case <-h.stop:
			return
		}
	}
}

// nextReclaimDelay spreads the reclaim over [half, one and a half] times
// store.InstanceReclaimInterval, so it averages out at the interval.
//
// The randomness is the whole point. Replicas of a Deployment start within
// seconds of each other, so a fixed interval would have all of them run the
// same UPDATE at the same moment, forever — the work is idempotent (the losers
// match nothing) but it is pointless contention on the connections table. A
// fresh delay after every tick, rather than a one-off startup offset, also
// keeps them from drifting back into phase. Bounded above by 1.5 intervals,
// which is still well inside store.InstanceStaleAfter, so a dead instance is
// never waited on for more than one grace period past going stale.
func nextReclaimDelay() time.Duration {
	half := store.InstanceReclaimInterval / 2

	// math/rand, not crypto/rand: this spreads a housekeeping tick, nothing
	// depends on it being unpredictable.
	return half + time.Duration(rand.Int64N(int64(store.InstanceReclaimInterval)))
}

// reclaim closes the connections of instances the registry now proves are gone,
// and prunes their registry rows.
//
// This is the periodic counterpart of the startup reconcile, and it exists
// because the startup pass alone misses the commonest crash shape: a SIGKILLed
// pod's registry row is only seconds old when its replacement starts, so the
// replacement reclaims nothing, and by the time the row goes stale 15 minutes
// later there is no restart left to notice. On a stable deployment the rows
// would then stay open until some process happens to restart, possibly days
// later.
//
// Only the liveness-checked half runs here. The own-instance half of the
// reconcile would close this run's own live sessions, which is why it is
// startup-only — see store.CloseOrphanedConnections.
//
// A failure is logged at warn, not error: the next tick tries again, and
// nothing about serving traffic depends on it.
func (h *instanceHeartbeat) reclaim(ctx context.Context) {
	reclaimed, err := h.store.ReclaimDeadInstanceConnections(ctx)
	if err != nil {
		h.logger.WarnContext(ctx, "failed to reclaim connections from dead instances",
			slog.Any("error", err))

		return
	}

	// Same wording and level as the startup reconcile, deliberately: away from
	// startup a non-zero count means exactly the same thing — a process died
	// without shutting down.
	logReclaimedConnections(ctx, h.logger, reclaimed)

	// After the reclaim, so it still saw the rows it judged.
	pruneStaleInstances(ctx, h.store, h.logger)
}

// beat refreshes the row. A failure is logged at warn rather than error: one
// missed tick is harmless, and it takes InstanceStaleAfter of consecutive
// failures before anything acts on it.
func (h *instanceHeartbeat) beat(ctx context.Context) {
	if err := h.store.HeartbeatInstance(ctx); err != nil {
		h.logger.WarnContext(ctx, "instance heartbeat failed",
			slog.Any("error", err),
			slog.String("instance_id", h.store.InstanceID()),
			slog.Duration("stale_after", store.InstanceStaleAfter))
	}
}

// Shutdown stops the heartbeat and removes this process from the registry.
//
// Deregistering is what makes a clean stop reclaimable immediately instead of
// after the grace period: the next process to start sees no row for us and can
// close whatever we left open. It must therefore run last in the shutdown
// sequence, once the proxies have drained — while we are still draining, we are
// alive and our sessions must stay off limits.
func (h *instanceHeartbeat) Shutdown(ctx context.Context) error {
	h.stopOnce.Do(func() {
		close(h.stop)
	})

	if err := h.store.DeregisterInstance(ctx); err != nil {
		// Logged rather than returned: a leftover row is not a failed shutdown,
		// it only delays reclaiming until the row goes stale.
		h.logger.WarnContext(ctx, "failed to deregister this instance",
			slog.Any("error", err),
			slog.String("instance_id", h.store.InstanceID()))
	}

	return nil
}
