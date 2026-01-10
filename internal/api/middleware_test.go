package api

import (
	"testing"
	"time"

	"github.com/fclairamb/dbbat/internal/store"
)

func TestAuthFailureTracker_CheckRateLimit(t *testing.T) {
	t.Parallel()

	t.Run("allows first attempts", func(t *testing.T) {
		tracker := newAuthFailureTracker()
		allowed, retryAfter := tracker.checkRateLimit("user1")
		if !allowed {
			t.Error("First attempt should be allowed")
		}
		if retryAfter != 0 {
			t.Errorf("retryAfter = %d, want 0", retryAfter)
		}
	})

	t.Run("allows attempts under threshold", func(t *testing.T) {
		tracker := newAuthFailureTracker()

		// Record 2 failures (threshold is 3)
		tracker.recordFailure("user2")
		tracker.recordFailure("user2")

		allowed, retryAfter := tracker.checkRateLimit("user2")
		if !allowed {
			t.Error("Should still be allowed with 2 failures")
		}
		if retryAfter != 0 {
			t.Errorf("retryAfter = %d, want 0", retryAfter)
		}
	})

	t.Run("blocks after 3 failures", func(t *testing.T) {
		tracker := newAuthFailureTracker()

		// Record 3 failures
		for i := 0; i < 3; i++ {
			tracker.recordFailure("user3")
		}

		allowed, retryAfter := tracker.checkRateLimit("user3")
		if allowed {
			t.Error("Should be blocked after 3 failures")
		}
		if retryAfter <= 0 || retryAfter > 5 {
			t.Errorf("retryAfter = %d, want 1-5", retryAfter)
		}
	})

	t.Run("blocks with longer delay after more failures", func(t *testing.T) {
		tracker := newAuthFailureTracker()

		// Record 5 failures (should get 30 second delay)
		for i := 0; i < 5; i++ {
			tracker.recordFailure("user4")
		}

		allowed, retryAfter := tracker.checkRateLimit("user4")
		if allowed {
			t.Error("Should be blocked after 5 failures")
		}
		// Should be around 30 seconds
		if retryAfter < 25 || retryAfter > 30 {
			t.Errorf("retryAfter = %d, want 25-30", retryAfter)
		}
	})

	t.Run("resets after successful login", func(t *testing.T) {
		tracker := newAuthFailureTracker()

		// Record 5 failures
		for i := 0; i < 5; i++ {
			tracker.recordFailure("user5")
		}

		// Should be blocked
		allowed, _ := tracker.checkRateLimit("user5")
		if allowed {
			t.Error("Should be blocked after 5 failures")
		}

		// Reset on successful login
		tracker.resetFailures("user5")

		// Should be allowed again
		allowed, retryAfter := tracker.checkRateLimit("user5")
		if !allowed {
			t.Error("Should be allowed after reset")
		}
		if retryAfter != 0 {
			t.Errorf("retryAfter = %d, want 0", retryAfter)
		}
	})

	t.Run("different users are independent", func(t *testing.T) {
		tracker := newAuthFailureTracker()

		// Block user6
		for i := 0; i < 5; i++ {
			tracker.recordFailure("user6")
		}

		// user6 should be blocked
		allowed, _ := tracker.checkRateLimit("user6")
		if allowed {
			t.Error("user6 should be blocked")
		}

		// user7 should still be allowed
		allowed, retryAfter := tracker.checkRateLimit("user7")
		if !allowed {
			t.Error("user7 should be allowed")
		}
		if retryAfter != 0 {
			t.Errorf("retryAfter = %d, want 0", retryAfter)
		}
	})
}

func TestUserHasChangedPassword(t *testing.T) {
	t.Parallel()

	t.Run("returns false when PasswordChangedAt is nil", func(t *testing.T) {
		user := &store.User{
			PasswordChangedAt: nil,
		}
		if user.HasChangedPassword() {
			t.Error("HasChangedPassword() should return false when PasswordChangedAt is nil")
		}
	})

	t.Run("returns true when PasswordChangedAt is set", func(t *testing.T) {
		now := time.Now()
		user := &store.User{
			PasswordChangedAt: &now,
		}
		if !user.HasChangedPassword() {
			t.Error("HasChangedPassword() should return true when PasswordChangedAt is set")
		}
	})
}
