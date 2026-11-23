package api

import (
	"testing"
	"time"

	"github.com/fclairamb/dbbat/internal/store"
)

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
