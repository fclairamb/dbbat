//go:build integration

package oracle

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/proxy/conncheck"
	"github.com/fclairamb/dbbat/internal/store"
)

// nullResolver satisfies shared.ServerResolver for checks that never tunnel:
// a direct dial resolves nothing.
type nullResolver struct{}

func (nullResolver) GetServerByUID(context.Context, uuid.UUID) (*store.Server, error) {
	return nil, assert.AnError
}

func (nullResolver) SetKnownHostKey(context.Context, uuid.UUID, string) error { return nil }

// TestIntegration_ConnCheckOracleLogin is the live counterpart to the fake-TNS
// unit tests: against a real Oracle it proves the connectivity check both
// accepts good credentials and *rejects* bad ones, which is the whole point of
// the probe — before it, a wrong password looked exactly like success.
func TestIntegration_ConnCheckOracleLogin(t *testing.T) {
	container, host, port := startOracleContainer(t)
	defer func() { _ = container.Terminate(context.Background()) }()

	service := "XEPDB1"

	newServer := func(password string) *store.Server {
		return &store.Server{
			UID:               uuid.New(),
			Host:              host,
			Port:              port,
			Protocol:          store.ProtocolOracle,
			Username:          "system",
			Password:          password,
			DatabaseName:      service,
			OracleServiceName: &service,
		}
	}

	// 32 bytes; unused because no row here carries encrypted material.
	checker := conncheck.New(nullResolver{}, []byte("0123456789012345678901234567890X"))

	t.Run("valid credentials", func(t *testing.T) {
		res := checker.Check(context.Background(), newServer("oracle"))

		require.True(t, res.OK, "stage=%s code=%s msg=%s", res.Stage, res.Code, res.Message)
		assert.Equal(t, conncheck.StageTargetAuth, res.Stage)
		assert.Equal(t, conncheck.CodeOK, res.Code)
	})

	t.Run("wrong password is an auth failure", func(t *testing.T) {
		res := checker.Check(context.Background(), newServer("definitely-not-the-password"))

		require.False(t, res.OK, "a wrong password must not pass the check")
		assert.Equal(t, conncheck.StageTargetAuth, res.Stage)
		assert.Equal(t, conncheck.CodeDBAuthFailed, res.Code,
			"wrong password must classify as db_auth_failed (msg=%s)", res.Message)
	})

	t.Run("unknown service is a handshake failure", func(t *testing.T) {
		srv := newServer("oracle")
		unknown := "NO_SUCH_SERVICE"
		srv.OracleServiceName = &unknown

		res := checker.Check(context.Background(), srv)

		require.False(t, res.OK)
		assert.Equal(t, conncheck.CodeDBHandshakeFailed, res.Code,
			"an unknown service points at the service name, not the credentials (msg=%s)", res.Message)
	})
}
