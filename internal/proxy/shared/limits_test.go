package shared

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fclairamb/dbbat/internal/store"
)

func int64Ptr(v int64) *int64 { return &v }

func TestLimitGuard_Check_NilGuardAndNilGrant(t *testing.T) {
	t.Parallel()

	var nilGuard *LimitGuard
	if err := nilGuard.Check(); err != nil {
		t.Fatalf("nil guard Check() = %v, want nil", err)
	}

	// A guard built from a nil grant enforces nothing.
	g := NewLimitGuard(nil, &atomic.Int64{}, &atomic.Int64{})
	if err := g.Check(); err != nil {
		t.Fatalf("nil-grant guard Check() = %v, want nil", err)
	}
}

func TestLimitGuard_Check_ByteQuota(t *testing.T) {
	t.Parallel()

	var from, to atomic.Int64

	grant := &store.Grant{
		BytesTransferred:    100, // already consumed before this session
		MaxBytesTransferred: int64Ptr(1000),
		ExpiresAt:           time.Now().Add(time.Hour),
	}

	g := NewLimitGuard(grant, &from, &to)

	// Under the cap: base(100) + live(500) = 600 < 1000.
	from.Store(300)
	to.Store(200)

	if err := g.Check(); err != nil {
		t.Fatalf("Check() under cap = %v, want nil", err)
	}

	// Cross the cap: base(100) + live(900) = 1000 >= 1000.
	to.Store(600)

	if err := g.Check(); !errors.Is(err, ErrByteQuotaExceeded) {
		t.Fatalf("Check() at cap = %v, want ErrByteQuotaExceeded", err)
	}
}

func TestLimitGuard_Check_Expiry(t *testing.T) {
	t.Parallel()

	grant := &store.Grant{
		ExpiresAt: time.Now().Add(time.Hour),
	}

	g := NewLimitGuard(grant, &atomic.Int64{}, &atomic.Int64{})

	// Not yet expired.
	if err := g.Check(); err != nil {
		t.Fatalf("Check() before expiry = %v, want nil", err)
	}

	// Advance the clock past ExpiresAt.
	g.setNow(func() time.Time { return grant.ExpiresAt.Add(time.Second) })

	if err := g.Check(); !errors.Is(err, ErrGrantExpired) {
		t.Fatalf("Check() after expiry = %v, want ErrGrantExpired", err)
	}
}

func TestLimitGuard_Check_NoLimitsNeverTrips(t *testing.T) {
	t.Parallel()

	// Grant with no byte cap; expiry far in the future.
	grant := &store.Grant{
		ExpiresAt: time.Now().Add(24 * time.Hour),
	}

	var from, to atomic.Int64
	from.Store(1 << 40)
	to.Store(1 << 40)

	g := NewLimitGuard(grant, &from, &to)

	if err := g.Check(); err != nil {
		t.Fatalf("Check() with no byte cap = %v, want nil", err)
	}
}

func TestLimitGuard_Watch_FiresOnByteQuota(t *testing.T) {
	t.Parallel()

	var from, to atomic.Int64

	grant := &store.Grant{
		MaxBytesTransferred: int64Ptr(1000),
		ExpiresAt:           time.Now().Add(time.Hour),
	}

	g := NewLimitGuard(grant, &from, &to)

	got := make(chan error, 1)

	go g.Watch(context.Background(), 5*time.Millisecond, func(err error) {
		got <- err
	})

	// Cross the cap after the watch has started.
	time.Sleep(10 * time.Millisecond)
	to.Store(2000)

	select {
	case err := <-got:
		if !errors.Is(err, ErrByteQuotaExceeded) {
			t.Fatalf("Watch onViolation = %v, want ErrByteQuotaExceeded", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Watch did not fire onViolation for byte quota")
	}
}

func TestLimitGuard_Watch_FiresOnExpiry(t *testing.T) {
	t.Parallel()

	grant := &store.Grant{
		ExpiresAt: time.Now().Add(time.Hour),
	}

	g := NewLimitGuard(grant, &atomic.Int64{}, &atomic.Int64{})
	// Pretend the grant is already expired.
	g.setNow(func() time.Time { return grant.ExpiresAt.Add(time.Minute) })

	got := make(chan error, 1)

	go g.Watch(context.Background(), 5*time.Millisecond, func(err error) {
		got <- err
	})

	select {
	case err := <-got:
		if !errors.Is(err, ErrGrantExpired) {
			t.Fatalf("Watch onViolation = %v, want ErrGrantExpired", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Watch did not fire onViolation for expiry")
	}
}

func TestLimitGuard_Watch_StopsOnContextCancel(t *testing.T) {
	t.Parallel()

	grant := &store.Grant{
		MaxBytesTransferred: int64Ptr(1000),
		ExpiresAt:           time.Now().Add(time.Hour),
	}

	g := NewLimitGuard(grant, &atomic.Int64{}, &atomic.Int64{})

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	fired := make(chan error, 1)

	go func() {
		g.Watch(ctx, 5*time.Millisecond, func(err error) { fired <- err })
		close(done)
	}()

	cancel()

	select {
	case <-done:
		// Watch returned after cancel — good.
	case <-time.After(2 * time.Second):
		t.Fatal("Watch did not return after context cancel")
	}

	select {
	case err := <-fired:
		t.Fatalf("Watch fired onViolation unexpectedly: %v", err)
	default:
	}
}

func TestLimitGuard_Check_Revocation(t *testing.T) {
	t.Parallel()

	var revoked atomic.Bool

	grant := &store.Grant{
		ExpiresAt: time.Now().Add(time.Hour),
	}

	g := NewLimitGuard(grant, &atomic.Int64{}, &atomic.Int64{}).WithRevocation(&revoked)

	// Not revoked yet.
	if err := g.Check(); err != nil {
		t.Fatalf("Check() before revoke = %v, want nil", err)
	}

	// Flip the shared flag as the API's revoke path would.
	revoked.Store(true)

	if err := g.Check(); !errors.Is(err, ErrGrantRevoked) {
		t.Fatalf("Check() after revoke = %v, want ErrGrantRevoked", err)
	}
}

func TestLimitGuard_Check_RevocationTakesPrecedence(t *testing.T) {
	t.Parallel()

	var revoked atomic.Bool
	revoked.Store(true)

	// Grant is also already expired; revocation must win as it is the most
	// authoritative reason.
	grant := &store.Grant{
		ExpiresAt: time.Now().Add(-time.Hour),
	}

	g := NewLimitGuard(grant, &atomic.Int64{}, &atomic.Int64{}).WithRevocation(&revoked)

	if err := g.Check(); !errors.Is(err, ErrGrantRevoked) {
		t.Fatalf("Check() = %v, want ErrGrantRevoked to take precedence", err)
	}
}

func TestLimitGuard_Watch_FiresOnRevocation(t *testing.T) {
	t.Parallel()

	var revoked atomic.Bool

	grant := &store.Grant{
		ExpiresAt: time.Now().Add(time.Hour),
	}

	g := NewLimitGuard(grant, &atomic.Int64{}, &atomic.Int64{}).WithRevocation(&revoked)

	got := make(chan error, 1)

	go g.Watch(context.Background(), 5*time.Millisecond, func(err error) {
		got <- err
	})

	// Revoke after the watch has started.
	time.Sleep(10 * time.Millisecond)
	revoked.Store(true)

	select {
	case err := <-got:
		if !errors.Is(err, ErrGrantRevoked) {
			t.Fatalf("Watch onViolation = %v, want ErrGrantRevoked", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Watch did not fire onViolation for revocation")
	}
}

func TestLimitGuard_Watch_KeepsRunningForRevocationWithoutLimits(t *testing.T) {
	t.Parallel()

	// A grant with no byte cap and no expiry, but a revocation flag attached:
	// Watch must keep polling so the eventual revoke tears the session down.
	var revoked atomic.Bool

	g := NewLimitGuard(nil, &atomic.Int64{}, &atomic.Int64{}).WithRevocation(&revoked)

	got := make(chan error, 1)

	go g.Watch(context.Background(), 5*time.Millisecond, func(err error) {
		got <- err
	})

	time.Sleep(10 * time.Millisecond)
	revoked.Store(true)

	select {
	case err := <-got:
		if !errors.Is(err, ErrGrantRevoked) {
			t.Fatalf("Watch onViolation = %v, want ErrGrantRevoked", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Watch did not fire onViolation for revocation-only guard")
	}
}

func TestLimitGuard_Watch_NoLimitsReturnsImmediately(t *testing.T) {
	t.Parallel()

	// No byte cap and no expiry -> Watch must return without spinning.
	g := NewLimitGuard(nil, &atomic.Int64{}, &atomic.Int64{})

	done := make(chan struct{})

	go func() {
		g.Watch(context.Background(), time.Millisecond, func(error) {
			t.Error("onViolation must not fire for a no-limits guard")
		})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Watch with no limits did not return")
	}
}
