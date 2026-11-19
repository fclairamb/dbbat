package api

import (
	"fmt"
	"testing"
	"time"

	"github.com/fclairamb/dbbat/internal/store"
)

// testClientIP is the source address the single-source tracker tests use.
const testClientIP = "192.0.2.10"

func TestAuthFailureTracker_CheckRateLimit(t *testing.T) {
	t.Parallel()

	t.Run("allows first attempts", func(t *testing.T) {
		t.Parallel()
		tracker := newAuthFailureTracker()
		allowed, retryAfter := tracker.checkRateLimit("user1", testClientIP)
		if !allowed {
			t.Error("First attempt should be allowed")
		}
		if retryAfter != 0 {
			t.Errorf("retryAfter = %d, want 0", retryAfter)
		}
	})

	t.Run("allows attempts under threshold", func(t *testing.T) {
		t.Parallel()
		tracker := newAuthFailureTracker()

		// Record 2 failures (threshold is 3)
		tracker.recordFailure("user2", testClientIP)
		tracker.recordFailure("user2", testClientIP)

		allowed, retryAfter := tracker.checkRateLimit("user2", testClientIP)
		if !allowed {
			t.Error("Should still be allowed with 2 failures")
		}
		if retryAfter != 0 {
			t.Errorf("retryAfter = %d, want 0", retryAfter)
		}
	})

	t.Run("blocks after 3 failures", func(t *testing.T) {
		t.Parallel()
		tracker := newAuthFailureTracker()

		// Record 3 failures
		for i := 0; i < 3; i++ {
			tracker.recordFailure("user3", testClientIP)
		}

		allowed, retryAfter := tracker.checkRateLimit("user3", testClientIP)
		if allowed {
			t.Error("Should be blocked after 3 failures")
		}
		if retryAfter <= 0 || retryAfter > 5 {
			t.Errorf("retryAfter = %d, want 1-5", retryAfter)
		}
	})

	t.Run("blocks with longer delay after more failures", func(t *testing.T) {
		t.Parallel()
		tracker := newAuthFailureTracker()

		// Record 5 failures (should get 30 second delay)
		for i := 0; i < 5; i++ {
			tracker.recordFailure("user4", testClientIP)
		}

		allowed, retryAfter := tracker.checkRateLimit("user4", testClientIP)
		if allowed {
			t.Error("Should be blocked after 5 failures")
		}
		// Should be around 30 seconds
		if retryAfter < 25 || retryAfter > 30 {
			t.Errorf("retryAfter = %d, want 25-30", retryAfter)
		}
	})

	t.Run("resets after successful login", func(t *testing.T) {
		t.Parallel()
		tracker := newAuthFailureTracker()

		// Record 5 failures
		for i := 0; i < 5; i++ {
			tracker.recordFailure("user5", testClientIP)
		}

		// Should be blocked
		allowed, _ := tracker.checkRateLimit("user5", testClientIP)
		if allowed {
			t.Error("Should be blocked after 5 failures")
		}

		// Reset on successful login
		tracker.resetFailures("user5", testClientIP)

		// Should be allowed again
		allowed, retryAfter := tracker.checkRateLimit("user5", testClientIP)
		if !allowed {
			t.Error("Should be allowed after reset")
		}
		if retryAfter != 0 {
			t.Errorf("retryAfter = %d, want 0", retryAfter)
		}
	})

	t.Run("different users are independent", func(t *testing.T) {
		t.Parallel()
		tracker := newAuthFailureTracker()

		// Block user6
		for i := 0; i < 5; i++ {
			tracker.recordFailure("user6", testClientIP)
		}

		// user6 should be blocked
		allowed, _ := tracker.checkRateLimit("user6", testClientIP)
		if allowed {
			t.Error("user6 should be blocked")
		}

		// user7 should still be allowed
		allowed, retryAfter := tracker.checkRateLimit("user7", testClientIP)
		if !allowed {
			t.Error("user7 should be allowed")
		}
		if retryAfter != 0 {
			t.Errorf("retryAfter = %d, want 0", retryAfter)
		}
	})
}

func TestAuthFailureTracker_PerSource(t *testing.T) {
	t.Parallel()

	t.Run("failures from one address do not block another", func(t *testing.T) {
		t.Parallel()
		tracker := newAuthFailureTracker()

		for i := 0; i < 12; i++ {
			tracker.recordFailure("admin", "192.0.2.1")
		}

		allowed, retryAfter := tracker.checkRateLimit("admin", "192.0.2.1")
		if allowed || retryAfter <= 0 {
			t.Errorf("attacker source: allowed=%v retryAfter=%d, want blocked", allowed, retryAfter)
		}

		allowed, retryAfter = tracker.checkRateLimit("admin", "198.51.100.7")
		if !allowed || retryAfter != 0 {
			t.Errorf("other source: allowed=%v retryAfter=%d, want allowed", allowed, retryAfter)
		}
	})

	t.Run("a success clears only its own source", func(t *testing.T) {
		t.Parallel()
		tracker := newAuthFailureTracker()

		for i := 0; i < 5; i++ {
			tracker.recordFailure("alice", "192.0.2.1")
			tracker.recordFailure("alice", "192.0.2.2")
		}

		tracker.resetFailures("alice", "192.0.2.2")

		if allowed, _ := tracker.checkRateLimit("alice", "192.0.2.2"); !allowed {
			t.Error("the source that logged in should be cleared")
		}
		if allowed, _ := tracker.checkRateLimit("alice", "192.0.2.1"); allowed {
			t.Error("another source's backoff should survive the success")
		}
	})
}

func TestAuthFailureTracker_UsernameCeiling(t *testing.T) {
	t.Parallel()

	t.Run("distributed failures reach the ceiling once and start a burst for every source", func(t *testing.T) {
		t.Parallel()
		tracker := newAuthFailureTracker()

		crossings := 0
		for i := 0; i < authUsernameCeiling+5; i++ {
			// One failure per address: no single source is ever backed off.
			if tracker.recordFailure("admin", fmt.Sprintf("10.0.%d.%d", i/256, i%256)) {
				crossings++
				if i != authUsernameCeiling-1 {
					t.Errorf("ceiling reported at failure %d, want %d", i+1, authUsernameCeiling)
				}
			}
		}

		if crossings != 1 {
			t.Errorf("ceiling reported %d times, want exactly once", crossings)
		}

		// The ceiling never stops the password from being checked: a fresh
		// source passes the pre-check, and the burst shows in ceilingRetryAfter.
		if allowed, _ := tracker.checkRateLimit("admin", "203.0.113.99"); !allowed {
			t.Error("the ceiling must not refuse a password check before it happens")
		}
		retryAfter := tracker.ceilingRetryAfter("admin")
		if retryAfter <= int((authUsernameCeilingWindow - time.Minute).Seconds()) {
			t.Errorf("ceilingRetryAfter = %d, want close to the ceiling window", retryAfter)
		}
		if got := tracker.ceilingRetryAfter("bob"); got != 0 {
			t.Errorf("another username: ceilingRetryAfter = %d, want 0", got)
		}

		// A success lifts the ceiling and reports the burst it ended.
		if got := tracker.resetFailures("admin", "203.0.113.99"); got != authUsernameCeiling+5 {
			t.Errorf("resetFailures = %d, want %d", got, authUsernameCeiling+5)
		}
		if got := tracker.ceilingRetryAfter("admin"); got != 0 {
			t.Errorf("after a success: ceilingRetryAfter = %d, want 0", got)
		}
		if got := tracker.resetFailures("admin", "203.0.113.99"); got != 0 {
			t.Errorf("a second success reported a burst of %d, want 0", got)
		}
	})

	t.Run("a success below the ceiling clears it without reporting a burst", func(t *testing.T) {
		t.Parallel()
		tracker := newAuthFailureTracker()

		for i := 0; i < authUsernameCeiling-1; i++ {
			tracker.recordFailure("admin", fmt.Sprintf("10.3.%d.%d", i/256, i%256))
		}

		if got := tracker.resetFailures("admin", "203.0.113.99"); got != 0 {
			t.Errorf("resetFailures = %d, want 0 below the ceiling", got)
		}

		// The count restarted: one more failure does not reach the ceiling.
		if tracker.recordFailure("admin", "203.0.113.98") {
			t.Error("the ceiling count should have been cleared by the success")
		}
	})

	t.Run("under the ceiling a fresh source is allowed", func(t *testing.T) {
		t.Parallel()
		tracker := newAuthFailureTracker()

		for i := 0; i < authUsernameCeiling-1; i++ {
			if tracker.recordFailure("admin", fmt.Sprintf("10.1.%d.%d", i/256, i%256)) {
				t.Fatal("ceiling reported below the threshold")
			}
		}

		if allowed, _ := tracker.checkRateLimit("admin", "203.0.113.99"); !allowed {
			t.Error("a fresh source should be allowed below the ceiling")
		}
	})

	t.Run("sweep expires the ceiling after its window", func(t *testing.T) {
		t.Parallel()
		tracker := newAuthFailureTracker()

		for i := 0; i < authUsernameCeiling; i++ {
			tracker.recordFailure("admin", fmt.Sprintf("10.2.%d.%d", i/256, i%256))
		}

		tracker.sweep(time.Now().Add(authUsernameCeilingWindow + time.Minute))

		if got := tracker.ceilingRetryAfter("admin"); got != 0 {
			t.Errorf("the ceiling should be gone after its window, ceilingRetryAfter = %d", got)
		}
		tracker.mu.RLock()
		defer tracker.mu.RUnlock()
		if len(tracker.failures) != 0 || len(tracker.ceilings) != 0 {
			t.Errorf("sweep left %d backoff and %d ceiling entries", len(tracker.failures), len(tracker.ceilings))
		}
	})
}

func TestUserHasChangedPassword(t *testing.T) {
	t.Parallel()

	t.Run("returns false when PasswordChangedAt is nil", func(t *testing.T) {
		t.Parallel()
		user := &store.User{
			PasswordChangedAt: nil,
		}
		if user.HasChangedPassword() {
			t.Error("HasChangedPassword() should return false when PasswordChangedAt is nil")
		}
	})

	t.Run("returns true when PasswordChangedAt is set", func(t *testing.T) {
		t.Parallel()
		now := time.Now()
		user := &store.User{
			PasswordChangedAt: &now,
		}
		if !user.HasChangedPassword() {
			t.Error("HasChangedPassword() should return true when PasswordChangedAt is set")
		}
	})
}
