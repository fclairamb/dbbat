package main

import (
	"context"
	"log/slog"
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

	for {
		select {
		case <-ticker.C:
			h.beat(ctx)
		case <-h.stop:
			return
		}
	}
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
