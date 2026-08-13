package main

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/fclairamb/dbbat/internal/proxy/shared"
	"github.com/fclairamb/dbbat/internal/store"
)

// What a panic in one turn of the heartbeat loop is logged under. Each unit of
// work is named separately because that is the only thing the log line can
// usefully say: which of the registry's four jobs stopped happening.
const (
	goroutineNameHeartbeat     = "instance heartbeat"
	goroutineNameSharedIDCheck = "shared instance id check"
	goroutineNameReclaim       = "orphaned connection reclaim"
	goroutineNameOpenStamps    = "open query chain stamp refresh"
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
// some unrelated process happens to restart. The same tick re-seals the query
// chain head of the sessions this run still has open (see refreshOpenStamps).
type instanceHeartbeat struct {
	store    *store.Store
	logger   *slog.Logger
	stop     chan struct{}
	stopOnce sync.Once

	// sharedIDCandidates maps the run id of every other registry row that
	// carried our instance id and looked alive when we started, to the
	// last_seen_at we saw then. Nil once the question is settled. See
	// checkSharedInstanceID.
	sharedIDCandidates map[string]time.Time
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
			slog.String("instance_id", dataStore.InstanceID()),
			slog.String("run_id", dataStore.RunID()))
	}

	beat := &instanceHeartbeat{
		store:              dataStore,
		logger:             logger,
		stop:               make(chan struct{}),
		sharedIDCandidates: sharedInstanceIDCandidates(ctx, dataStore, logger),
	}

	go beat.run(ctx)

	logger.InfoContext(ctx, "Instance registered",
		slog.String("instance_id", dataStore.InstanceID()),
		slog.String("run_id", dataStore.RunID()),
		slog.Duration("heartbeat_interval", store.InstanceHeartbeatInterval),
		slog.Duration("stale_after", store.InstanceStaleAfter))

	return beat
}

// sharedInstanceIDCandidates samples the runs that are registered under our
// instance id, are not us, and still look alive.
//
// It is only a suspicion, which is why nothing is logged here: at startup a
// fresh row carrying our id and someone else's run id is either a replica that
// shares our DBB_INSTANCE_ID or our own predecessor, which crashed moments ago
// and left a row that will keep its last heartbeat until it goes stale. Warning
// straight away would cry wolf on every crash-restart of a stable instance id.
// checkSharedInstanceID tells the two apart later.
//
// Call it after RegisterInstance, so our own row is already excluded. A failed
// lookup yields no candidates: this is an advisory, and nothing downstream
// depends on it.
func sharedInstanceIDCandidates(ctx context.Context, dataStore *store.Store, logger *slog.Logger) map[string]time.Time {
	peers, err := dataStore.LiveRunsSharingInstanceID(ctx)
	if err != nil {
		logger.WarnContext(ctx, "failed to check whether this instance id is shared",
			slog.Any("error", err))

		return nil
	}

	if len(peers) == 0 {
		return nil
	}

	candidates := make(map[string]time.Time, len(peers))
	for _, peer := range peers {
		candidates[peer.RunID] = peer.LastSeenAt
	}

	return candidates
}

// checkSharedInstanceID settles the suspicion raised at startup: is another
// process really running under our instance id?
//
// A live peer's last_seen_at moves; a dead run's row keeps the timestamp it
// died with until it goes stale. So a candidate whose heartbeat has advanced
// since we started is a live peer, and one whose heartbeat has not is our own
// crashed predecessor — the reason this is not simply logged at registration.
//
// Sharing an id is no longer dangerous: both halves of the reconcile key on the
// run id, so neither process can close the other's sessions. It is still worth
// saying once. The operator almost certainly meant the ids to be unique — the
// silent path here is config.FallbackInstanceID, which every replica that
// cannot read its hostname lands on — and a shared id makes the reconcile's
// "left open by a previous run" count read as this process's when it belongs to
// the whole id, and collapses several replicas into one identity wherever that
// id is shown.
//
// Runs once: after it warns, or after every candidate has gone, the map is
// dropped and this becomes a no-op for the life of the process.
func (h *instanceHeartbeat) checkSharedInstanceID(ctx context.Context) {
	if len(h.sharedIDCandidates) == 0 {
		return
	}

	peers, err := h.store.LiveRunsSharingInstanceID(ctx)
	if err != nil {
		// Keep the candidates: the next tick tries again.
		h.logger.WarnContext(ctx, "failed to check whether this instance id is shared",
			slog.Any("error", err))

		return
	}

	confirmed, remaining := classifySharedIDCandidates(h.sharedIDCandidates, peers)
	if len(confirmed) == 0 {
		h.sharedIDCandidates = remaining

		return
	}

	h.sharedIDCandidates = nil

	h.logger.WarnContext(ctx, "Another live process is running under this instance id",
		slog.String("instance_id", h.store.InstanceID()),
		slog.String("run_id", h.store.RunID()),
		slog.Any("other_run_ids", confirmed),
		slog.String("hint", "DBB_INSTANCE_ID must be unique per running process; "+
			"the default (the hostname) already is"))
}

// classifySharedIDCandidates splits the runs registered under our instance id
// into those that have proven they are alive since we started — their heartbeat
// moved — and those still under observation.
//
// A candidate that has disappeared from the registry is dropped: it was pruned
// or deregistered, which settles it as dead. A run that registered after we
// sampled is not judged here at all; it runs this same check against our row,
// so no warning is lost.
func classifySharedIDCandidates(
	candidates map[string]time.Time, peers []store.Instance,
) ([]string, map[string]time.Time) {
	var confirmed []string

	remaining := make(map[string]time.Time, len(candidates))

	for _, peer := range peers {
		seenAtStartup, isCandidate := candidates[peer.RunID]
		switch {
		case !isCandidate:
		case peer.LastSeenAt.After(seenAtStartup):
			confirmed = append(confirmed, peer.RunID)
		default:
			remaining[peer.RunID] = seenAtStartup
		}
	}

	return confirmed, remaining
}

func (h *instanceHeartbeat) run(ctx context.Context) {
	ticker := time.NewTicker(store.InstanceHeartbeatInterval)
	defer ticker.Stop()

	// The reclaim rides along on this goroutine rather than getting one of its
	// own: it is the same "keep the registry honest" job seen from the other
	// side, and it must stop for exactly the same reasons.
	reclaim := time.NewTimer(nextReclaimDelay())
	defer reclaim.Stop()

	// Every unit of work runs under shared.RunMaintenance, per turn and
	// individually. This is a goroutine of its own, so no recover reaches it and
	// an unguarded panic in any of these store calls would end the process with
	// every live session on it.
	//
	// Individually rather than a guard per tick, because each of the four is
	// load-bearing for a different reason and none should be skipped because
	// another blew up: a missed beat makes this run look dead to another
	// replica's reconcile, which is license to close our live connections, and a
	// missed refreshOpenStamps is what bounds how long an open session's query
	// chain goes unsealed. reclaim.Reset stays outside the guards so the timer is
	// rearmed whatever happened inside them.
	for {
		select {
		case <-ticker.C:
			h.guarded(ctx, goroutineNameHeartbeat, func() { h.beat(ctx) })
			h.guarded(ctx, goroutineNameSharedIDCheck, func() { h.checkSharedInstanceID(ctx) })
		case <-reclaim.C:
			h.guarded(ctx, goroutineNameReclaim, func() { h.reclaim(ctx) })
			h.guarded(ctx, goroutineNameOpenStamps, func() { h.refreshOpenStamps(ctx) })
			reclaim.Reset(nextReclaimDelay())
		case <-h.stop:
			return
		}
	}
}

// guarded runs one unit of the heartbeat loop's work with a panic recover
// around it.
func (h *instanceHeartbeat) guarded(ctx context.Context, name string, fn func()) {
	shared.RunMaintenance(ctx, h.logger, name, fn)
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
// Only the reclaim half runs here, and it is enough: it excludes this run, not
// this instance id, so a previous run of our own id is reclaimed on this pass
// like anyone else's once its registry row goes stale. The own half stays at
// startup because that is the only moment its count means "the run I replaced"
// — see store.CloseOrphanedConnections.
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

// refreshOpenStamps re-seals the query chain head of the sessions this run
// still has open.
//
// It rides on the reclaim tick rather than a timer of its own because it closes
// the same class of gap from the other end. The reclaim bounds how long a
// *crashed* session's tail goes unstamped; this bounds how long a session that
// simply never ends goes unstamped — a psql window left open all day, a pooled
// application connection, an approval hold waiting on a human. Before it, the
// stamp was only ever written by a close, so such a session had no protection
// against a trailing deletion for its entire life.
//
// After it, the protection is a prefix: whatever the last sweep sealed. See
// store.RefreshOpenChainStamps.
//
// A failure is logged at warn, not error: the next tick tries again, and
// nothing about serving traffic depends on it.
func (h *instanceHeartbeat) refreshOpenStamps(ctx context.Context) {
	stamped, err := h.store.RefreshOpenChainStamps(ctx)
	if err != nil {
		h.logger.WarnContext(ctx, "failed to refresh the query chain stamps of open sessions",
			slog.Any("error", err))

		return
	}

	if stamped == 0 {
		return
	}

	// Debug: on a busy replica this is every open session, every pass, and it
	// is routine housekeeping rather than an event.
	h.logger.DebugContext(ctx, "Refreshed the query chain stamp of open sessions",
		slog.Int64("connections", stamped),
		slog.String("run_id", h.store.RunID()))
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
