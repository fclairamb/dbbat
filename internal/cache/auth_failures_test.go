package cache

import (
	"fmt"
	"testing"
	"time"
)

// testClientIP is the source address the single-source tracker tests use.
const testClientIP = "192.0.2.10"

func TestAuthFailureTracker_CheckRateLimit(t *testing.T) {
	t.Parallel()

	t.Run("allows first attempts", func(t *testing.T) {
		t.Parallel()
		tracker := NewAuthFailureTracker()
		allowed, retryAfter := tracker.CheckRateLimit("user1", testClientIP)
		if !allowed {
			t.Error("First attempt should be allowed")
		}
		if retryAfter != 0 {
			t.Errorf("retryAfter = %d, want 0", retryAfter)
		}
	})

	t.Run("allows attempts under threshold", func(t *testing.T) {
		t.Parallel()
		tracker := NewAuthFailureTracker()

		// Record 2 failures (threshold is 3)
		tracker.RecordFailure("user2", testClientIP)
		tracker.RecordFailure("user2", testClientIP)

		allowed, retryAfter := tracker.CheckRateLimit("user2", testClientIP)
		if !allowed {
			t.Error("Should still be allowed with 2 failures")
		}
		if retryAfter != 0 {
			t.Errorf("retryAfter = %d, want 0", retryAfter)
		}
	})

	t.Run("blocks after 3 failures", func(t *testing.T) {
		t.Parallel()
		tracker := NewAuthFailureTracker()

		// Record 3 failures
		for i := 0; i < 3; i++ {
			tracker.RecordFailure("user3", testClientIP)
		}

		allowed, retryAfter := tracker.CheckRateLimit("user3", testClientIP)
		if allowed {
			t.Error("Should be blocked after 3 failures")
		}
		if retryAfter <= 0 || retryAfter > 5 {
			t.Errorf("retryAfter = %d, want 1-5", retryAfter)
		}
	})

	t.Run("blocks with longer delay after more failures", func(t *testing.T) {
		t.Parallel()
		tracker := NewAuthFailureTracker()

		// Record 5 failures (should get 30 second delay)
		for i := 0; i < 5; i++ {
			tracker.RecordFailure("user4", testClientIP)
		}

		allowed, retryAfter := tracker.CheckRateLimit("user4", testClientIP)
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
		tracker := NewAuthFailureTracker()

		// Record 5 failures
		for i := 0; i < 5; i++ {
			tracker.RecordFailure("user5", testClientIP)
		}

		// Should be blocked
		allowed, _ := tracker.CheckRateLimit("user5", testClientIP)
		if allowed {
			t.Error("Should be blocked after 5 failures")
		}

		// Reset on successful login
		tracker.ResetFailures("user5", testClientIP)

		// Should be allowed again
		allowed, retryAfter := tracker.CheckRateLimit("user5", testClientIP)
		if !allowed {
			t.Error("Should be allowed after reset")
		}
		if retryAfter != 0 {
			t.Errorf("retryAfter = %d, want 0", retryAfter)
		}
	})

	t.Run("different users are independent", func(t *testing.T) {
		t.Parallel()
		tracker := NewAuthFailureTracker()

		// Block user6
		for i := 0; i < 5; i++ {
			tracker.RecordFailure("user6", testClientIP)
		}

		// user6 should be blocked
		allowed, _ := tracker.CheckRateLimit("user6", testClientIP)
		if allowed {
			t.Error("user6 should be blocked")
		}

		// user7 should still be allowed
		allowed, retryAfter := tracker.CheckRateLimit("user7", testClientIP)
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
		tracker := NewAuthFailureTracker()

		for i := 0; i < 12; i++ {
			tracker.RecordFailure("admin", "192.0.2.1")
		}

		allowed, retryAfter := tracker.CheckRateLimit("admin", "192.0.2.1")
		if allowed || retryAfter <= 0 {
			t.Errorf("attacker source: allowed=%v retryAfter=%d, want blocked", allowed, retryAfter)
		}

		allowed, retryAfter = tracker.CheckRateLimit("admin", "198.51.100.7")
		if !allowed || retryAfter != 0 {
			t.Errorf("other source: allowed=%v retryAfter=%d, want allowed", allowed, retryAfter)
		}
	})

	t.Run("a success clears only its own source", func(t *testing.T) {
		t.Parallel()
		tracker := NewAuthFailureTracker()

		for i := 0; i < 5; i++ {
			tracker.RecordFailure("alice", "192.0.2.1")
			tracker.RecordFailure("alice", "192.0.2.2")
		}

		tracker.ResetFailures("alice", "192.0.2.2")

		if allowed, _ := tracker.CheckRateLimit("alice", "192.0.2.2"); !allowed {
			t.Error("the source that logged in should be cleared")
		}
		if allowed, _ := tracker.CheckRateLimit("alice", "192.0.2.1"); allowed {
			t.Error("another source's backoff should survive the success")
		}
	})
}

func TestAuthFailureTracker_UsernameCeiling(t *testing.T) {
	t.Parallel()

	t.Run("distributed failures reach the ceiling once and start a burst for every source", func(t *testing.T) {
		t.Parallel()
		tracker := NewAuthFailureTracker()

		crossings := 0
		for i := 0; i < AuthUsernameCeiling+5; i++ {
			// One failure per address: no single source is ever backed off.
			if tracker.RecordFailure("admin", fmt.Sprintf("10.0.%d.%d", i/256, i%256)) {
				crossings++
				if i != AuthUsernameCeiling-1 {
					t.Errorf("ceiling reported at failure %d, want %d", i+1, AuthUsernameCeiling)
				}
			}
		}

		if crossings != 1 {
			t.Errorf("ceiling reported %d times, want exactly once", crossings)
		}

		// The ceiling never stops the password from being checked: a fresh
		// source passes the pre-check, and the burst shows in CeilingRetryAfter.
		if allowed, _ := tracker.CheckRateLimit("admin", "203.0.113.99"); !allowed {
			t.Error("the ceiling must not refuse a password check before it happens")
		}
		retryAfter := tracker.CeilingRetryAfter("admin")
		if retryAfter <= int((AuthUsernameCeilingWindow - time.Minute).Seconds()) {
			t.Errorf("CeilingRetryAfter = %d, want close to the ceiling window", retryAfter)
		}
		if got := tracker.CeilingRetryAfter("bob"); got != 0 {
			t.Errorf("another username: CeilingRetryAfter = %d, want 0", got)
		}

		// A success lifts the ceiling and reports the burst it ended.
		if got := tracker.ResetFailures("admin", "203.0.113.99"); got != AuthUsernameCeiling+5 {
			t.Errorf("ResetFailures = %d, want %d", got, AuthUsernameCeiling+5)
		}
		if got := tracker.CeilingRetryAfter("admin"); got != 0 {
			t.Errorf("after a success: CeilingRetryAfter = %d, want 0", got)
		}
		if got := tracker.ResetFailures("admin", "203.0.113.99"); got != 0 {
			t.Errorf("a second success reported a burst of %d, want 0", got)
		}
	})

	t.Run("a success below the ceiling clears it without reporting a burst", func(t *testing.T) {
		t.Parallel()
		tracker := NewAuthFailureTracker()

		for i := 0; i < AuthUsernameCeiling-1; i++ {
			tracker.RecordFailure("admin", fmt.Sprintf("10.3.%d.%d", i/256, i%256))
		}

		if got := tracker.ResetFailures("admin", "203.0.113.99"); got != 0 {
			t.Errorf("ResetFailures = %d, want 0 below the ceiling", got)
		}

		// The count restarted: one more failure does not reach the ceiling.
		if tracker.RecordFailure("admin", "203.0.113.98") {
			t.Error("the ceiling count should have been cleared by the success")
		}
	})

	t.Run("under the ceiling a fresh source is allowed", func(t *testing.T) {
		t.Parallel()
		tracker := NewAuthFailureTracker()

		for i := 0; i < AuthUsernameCeiling-1; i++ {
			if tracker.RecordFailure("admin", fmt.Sprintf("10.1.%d.%d", i/256, i%256)) {
				t.Fatal("ceiling reported below the threshold")
			}
		}

		if allowed, _ := tracker.CheckRateLimit("admin", "203.0.113.99"); !allowed {
			t.Error("a fresh source should be allowed below the ceiling")
		}
	})

	t.Run("sweep expires the ceiling after its window", func(t *testing.T) {
		t.Parallel()
		tracker := NewAuthFailureTracker()

		for i := 0; i < AuthUsernameCeiling; i++ {
			tracker.RecordFailure("admin", fmt.Sprintf("10.2.%d.%d", i/256, i%256))
		}

		tracker.sweep(time.Now().Add(AuthUsernameCeilingWindow + time.Minute))

		if got := tracker.CeilingRetryAfter("admin"); got != 0 {
			t.Errorf("the ceiling should be gone after its window, CeilingRetryAfter = %d", got)
		}
		tracker.mu.RLock()
		defer tracker.mu.RUnlock()
		if len(tracker.failures) != 0 || len(tracker.ceilings) != 0 {
			t.Errorf("sweep left %d backoff and %d ceiling entries", len(tracker.failures), len(tracker.ceilings))
		}
	})
}
