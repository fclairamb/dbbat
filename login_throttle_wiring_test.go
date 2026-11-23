package main

import (
	"context"
	"testing"

	"github.com/fclairamb/dbbat/internal/cache"
	"github.com/fclairamb/dbbat/internal/store"
)

// The listeners must count into the API's own tracker: one secret, one
// guessing budget, whichever port it is guessed on.
func TestLoginThrottleFactorySharesTheTracker(t *testing.T) {
	t.Parallel()

	tracker := cache.NewAuthFailureTracker()
	throttles := loginThrottleFactory(tracker, nil, nil, true)

	pg := throttles(store.ProtocolPostgreSQL)
	my := throttles(store.ProtocolMySQL)

	ctx := context.Background()
	pg.Failure(ctx, "admin", "192.0.2.1")
	pg.Failure(ctx, "admin", "192.0.2.1")
	my.Failure(ctx, "admin", "192.0.2.1")

	if allowed, _ := tracker.CheckRateLimit("admin", "192.0.2.1"); allowed {
		t.Fatal("failures on two listeners should add up in the shared tracker")
	}

	if my.Allow(ctx, "admin", "192.0.2.1") {
		t.Error("the MySQL listener should see the backoff the PostgreSQL failures caused")
	}

	testMode := loginThrottleFactory(tracker, nil, nil, false)(store.ProtocolMSSQL)
	if !testMode.Allow(ctx, "admin", "192.0.2.1") {
		t.Error("test mode never refuses")
	}
}
