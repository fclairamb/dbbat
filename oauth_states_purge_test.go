package main

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/store"
)

var errStoreUnavailable = errors.New("store unavailable")

// The purge runs against the real store in production.
var _ oauthStateCleaner = (*store.Store)(nil)

// recordingCleaner counts the purges and can fail or panic on demand.
type recordingCleaner struct {
	calls   atomic.Int64
	failure func(call int64) error
}

func (r *recordingCleaner) CleanupExpiredOAuthStates(context.Context) (int64, error) {
	call := r.calls.Add(1)
	if r.failure != nil {
		return 0, r.failure(call)
	}

	return 1, nil
}

// TestOAuthStatePurgeRunsPeriodically pins that the purge runs at startup and
// then on every tick, and keeps running after a failed or panicking turn:
// expired oauth_states rows used to stay forever because nothing called
// CleanupExpiredOAuthStates.
func TestOAuthStatePurgeRunsPeriodically(t *testing.T) {
	t.Parallel()

	cleaner := &recordingCleaner{failure: func(call int64) error {
		switch call {
		case 2:
			return errStoreUnavailable
		case 3:
			panic("boom")
		}

		return nil
	}}

	purger := newOAuthStatePurger(cleaner, slog.New(slog.DiscardHandler), 10*time.Millisecond)

	go purger.run(context.Background())

	t.Cleanup(func() { _ = purger.Shutdown(context.Background()) })

	require.Eventually(t, func() bool { return cleaner.calls.Load() >= 5 },
		5*time.Second, 5*time.Millisecond,
		"the purge must keep running on its interval, past an error and a panic")
}

// TestOAuthStatePurgeStopsOnShutdown pins that Shutdown ends the loop.
func TestOAuthStatePurgeStopsOnShutdown(t *testing.T) {
	t.Parallel()

	cleaner := &recordingCleaner{}
	purger := newOAuthStatePurger(cleaner, slog.New(slog.DiscardHandler), time.Millisecond)

	done := make(chan struct{})

	go func() {
		purger.run(context.Background())
		close(done)
	}()

	require.Eventually(t, func() bool { return cleaner.calls.Load() >= 1 }, 5*time.Second, time.Millisecond)
	require.NoError(t, purger.Shutdown(context.Background()))
	require.NoError(t, purger.Shutdown(context.Background()), "Shutdown is idempotent")

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the purge loop did not stop on Shutdown")
	}
}

// TestOAuthStatePurgeIsRegistered pins the wiring: the purge starts whatever
// the configuration (there is no switch to disable it), runs straight away,
// uses the ten-minute interval, and is part of the shutdown sequence.
func TestOAuthStatePurgeIsRegistered(t *testing.T) {
	t.Parallel()

	cleaner := &recordingCleaner{}
	purger := startOAuthStatePurge(context.Background(), cleaner, slog.New(slog.DiscardHandler))

	t.Cleanup(func() { _ = purger.Shutdown(context.Background()) })

	assert.Equal(t, 10*time.Minute, purger.interval)
	require.Eventually(t, func() bool { return cleaner.calls.Load() >= 1 }, 5*time.Second, time.Millisecond,
		"the purge must run at startup")

	servers := collectServers(approvalDrain{}, nil, nil, nil, nil, nil, nil, nil, nil, purger, nil, nil)
	assert.Contains(t, servers, shutdownable(purger), "the purge must be stopped on shutdown")
}
