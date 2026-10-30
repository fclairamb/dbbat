package postgresql

import (
	"fmt"
	"net"
	"testing"

	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/store"
)

// TestClientHangupIsNotASessionError closes the client side the way a pooled
// driver does — no Terminate, just the socket — and asserts the relay reports
// errClientHungUp rather than a "failed to receive from client" error.
// pgproto3 turns that clean EOF into io.ErrUnexpectedEOF, so a check for io.EOF
// alone never matched.
func TestClientHangupIsNotASessionError(t *testing.T) {
	t.Parallel()

	proxySide, clientSide := net.Pipe()
	require.NoError(t, clientSide.Close())

	s := &Session{clientBackend: pgproto3.NewBackend(proxySide, proxySide)}

	err := s.proxyClientToUpstream()
	require.Error(t, err)
	assert.ErrorIs(t, err, errClientHungUp)
}

func TestIsClientRefusal(t *testing.T) {
	t.Parallel()

	assert.True(t, isClientRefusal(fmt.Errorf("authentication failed: %w", store.ErrNoActiveGrant)))
	assert.True(t, isClientRefusal(fmt.Errorf("authentication failed: %w", ErrQueryLimitExceeded)))
	assert.False(t, isClientRefusal(fmt.Errorf("authentication failed: %w", store.ErrTargetMatchesStorage)))
}
