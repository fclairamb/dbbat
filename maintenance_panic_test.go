package main

import (
	"context"
	"log/slog"
	"testing"
	"time"
)

// TestMaintenanceLoopPanicsDoNotEndTheProcess pins the two background loops
// package main owns.
//
// Both run on goroutines of their own, so no recover reaches them: before
// safe.RunMaintenance, a panic in CleanupOldQueryRows, HeartbeatInstance,
// reclaim or refreshOpenStamps ended the process — every live session, of every
// user, on every database — over a housekeeping tick. They are the same shape as
// the five proxies' dump-retention sweeps, and the accept-loop exemption
// documented in main.go does not reach them: nothing here is a listener, and
// none of it is worth the process over.
//
// A nil store is the injection: every one of these calls dereferences it, which
// is a real panic raised by real code rather than a synthetic one. The test
// binary staying alive is the assertion.
func TestMaintenanceLoopPanicsDoNotEndTheProcess(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	logger := slog.New(slog.DiscardHandler)

	t.Run("query retention sweep", func(t *testing.T) {
		t.Parallel()

		sweeper := &queryRetentionSweeper{logger: logger, stop: make(chan struct{})}

		sweeper.guardedSweep(ctx)
	})

	t.Run("heartbeat loop units", func(t *testing.T) {
		t.Parallel()

		beat := &instanceHeartbeat{logger: logger, stop: make(chan struct{})}

		// Every unit of work the loop dispatches, guarded independently: a
		// missed beat is what makes another replica's reconcile treat this run
		// as dead and close its live connections, so one blowing up must never
		// take the others with it.
		beat.guarded(ctx, goroutineNameHeartbeat, func() { beat.beat(ctx) })
		beat.guarded(ctx, goroutineNameReclaim, func() { beat.reclaim(ctx) })
		beat.guarded(ctx, goroutineNameOpenStamps, func() { beat.refreshOpenStamps(ctx) })

		// checkSharedInstanceID returns early on an empty candidate map, so it
		// needs one to reach the store at all.
		beat.sharedIDCandidates = map[string]time.Time{"run-a": {}}
		beat.guarded(ctx, goroutineNameSharedIDCheck, func() { beat.checkSharedInstanceID(ctx) })
	})
}
