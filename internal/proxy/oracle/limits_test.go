package oracle

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/fclairamb/dbbat/internal/proxy/shared"
	"github.com/fclairamb/dbbat/internal/store"
)

// TestCheckQuotas_Expiry asserts the between-commands expiry gap is closed: a
// command issued after the grant's ExpiresAt is rejected with ErrGrantExpired.
func TestCheckQuotas_Expiry(t *testing.T) {
	t.Parallel()

	expired := newTestSession(&store.Grant{ExpiresAt: time.Now().Add(-time.Minute)})
	assert.ErrorIs(t, expired.checkQuotas(), shared.ErrGrantExpired)

	live := newTestSession(&store.Grant{ExpiresAt: time.Now().Add(time.Hour)})
	assert.NoError(t, live.checkQuotas())
}
