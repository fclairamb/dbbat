package mysql

import (
	"errors"
	"testing"
	"time"

	"github.com/fclairamb/dbbat/internal/proxy/shared"
	"github.com/fclairamb/dbbat/internal/store"
)

// TestCheckQuotas_Expiry asserts the between-commands expiry gap is closed: a
// command issued after the grant's ExpiresAt is rejected with ErrGrantExpired,
// while a live grant is accepted.
func TestCheckQuotas_Expiry(t *testing.T) {
	t.Parallel()

	if err := checkQuotas(&store.Grant{ExpiresAt: time.Now().Add(-time.Minute)}); !errors.Is(err, shared.ErrGrantExpired) {
		t.Fatalf("checkQuotas() expired grant = %v, want ErrGrantExpired", err)
	}

	if err := checkQuotas(&store.Grant{ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatalf("checkQuotas() live grant = %v, want nil", err)
	}
}
