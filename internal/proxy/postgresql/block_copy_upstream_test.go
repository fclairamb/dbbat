package postgresql

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/fclairamb/dbbat/internal/proxy/shared"
	"github.com/fclairamb/dbbat/internal/store"
)

// A copy the server starts on a block_copy grant ends the session, whatever
// the statement looked like to the text check.
func TestSession_ProxyUpstreamToClient_BlockCopyRefusesUpstreamCopy(t *testing.T) {
	t.Parallel()

	for name, msg := range map[string]pgproto3.BackendMessage{
		"copy out": &pgproto3.CopyOutResponse{},
		"copy in":  &pgproto3.CopyInResponse{},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			clientProxyEnd, clientTestEnd := net.Pipe()
			upstreamProxyEnd, upstreamTestEnd := net.Pipe()

			defer func() { _ = clientTestEnd.Close() }()
			defer func() { _ = upstreamTestEnd.Close() }()

			var fromClient, toClient atomic.Int64

			counted := shared.NewCountingConn(clientProxyEnd, &fromClient, &toClient)
			grant := &store.Grant{
				ExpiresAt:  time.Now().Add(time.Hour),
				Definition: &store.GrantDefinition{Controls: []string{store.ControlBlockCopy}},
			}

			s := &Session{
				clientConn:       counted,
				clientBackend:    pgproto3.NewBackend(counted, counted),
				upstreamFrontend: pgproto3.NewFrontend(upstreamProxyEnd, upstreamProxyEnd),
				grant:            grant,
				bytesFromClient:  &fromClient,
				bytesToClient:    &toClient,
				guard:            shared.NewLimitGuard(grant, &fromClient, &toClient),
				extendedState: &extendedQueryState{
					preparedStatements: make(map[string]*preparedStatement),
					portals:            make(map[string]*portalState),
				},
				logger: slog.New(slog.DiscardHandler),
				ctx:    context.Background(),
			}

			go func() {
				buf, err := msg.Encode(nil)
				if err == nil {
					_, _ = upstreamTestEnd.Write(buf)
				}
			}()

			if err := s.proxyUpstreamToClient(); !errors.Is(err, ErrCopyNotPermitted) {
				t.Fatalf("proxyUpstreamToClient() = %v, want ErrCopyNotPermitted", err)
			}
		})
	}
}
