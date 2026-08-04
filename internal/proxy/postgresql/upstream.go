package postgresql

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"

	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/fclairamb/dbbat/internal/proxy/shared"
	"github.com/fclairamb/dbbat/internal/proxy/upstream"
	"github.com/fclairamb/dbbat/internal/version"
)

// ErrUpstreamReadOnlyMode is returned when the upstream fails to set read-only mode.
var ErrUpstreamReadOnlyMode = errors.New("upstream error setting read-only mode")

// connectUpstream connects to the upstream PostgreSQL server and replays the
// result to the client.
//
// The connect half — dial, TLS negotiation, login — is upstream.ConnectPostgres,
// the same code the connectivity check runs, so a green check really does prove
// the proxy can get in. What stays here is the half that only makes sense with
// a downstream client attached: forwarding the server's startup state, enforcing
// the grant's read-only control, and remembering the cancellation key.
func (s *Session) connectUpstream() error {
	// Decrypt database password
	if err := s.database.DecryptPassword(s.encryptionKey); err != nil {
		return fmt.Errorf("failed to decrypt database password: %w", err)
	}

	up, err := upstream.ConnectPostgres(s.ctx, s.dialUpstream, upstream.PostgresConfig{
		Host:            s.database.Host,
		Username:        s.database.Username,
		Password:        s.database.Password,
		Database:        s.database.DatabaseName,
		ApplicationName: buildApplicationName(s.user.Username, s.clientApplicationName),
		SSLMode:         s.database.SSLMode,
	}, s.logger)
	if err != nil {
		return s.reportUpstreamConnectFailure(err)
	}

	s.upstreamConn = up.Conn
	s.upstreamFrontend = up.Frontend
	s.upstreamTLS = up.TLS

	return s.replayUpstreamStartup(up)
}

// dialUpstream opens the transport to the target: a direct TCP dial, or a
// tunnel through the SSH bastion chain when the server row's via_uid is set.
func (s *Session) dialUpstream(ctx context.Context) (net.Conn, error) {
	conn, err := shared.DialUpstream(ctx, s.store, s.encryptionKey, s.database)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to upstream: %w", err)
	}

	return conn, nil
}

// reportUpstreamConnectFailure forwards an upstream ErrorResponse to the client
// before giving up, so a psql user sees the server's own wording ("password
// authentication failed for user ...") rather than a dbbat paraphrase of it.
func (s *Session) reportUpstreamConnectFailure(err error) error {
	var authErr *upstream.PostgresAuthError
	if errors.As(err, &authErr) {
		if sendErr := s.sendToClient(authErr.Response); sendErr != nil {
			s.logger.ErrorContext(s.ctx, "failed to forward error to client", slog.Any("error", sendErr))
		}
	}

	return err
}

// replayUpstreamStartup hands the client everything the upstream login
// produced, in the order the protocol requires: AuthenticationOk, the buffered
// ParameterStatus messages, BackendKeyData, then ReadyForQuery.
//
// The read-only SET runs first, before the client is told it is connected: a
// session that cannot be pinned read-only must fail rather than serve one
// writable query.
func (s *Session) replayUpstreamStartup(up *upstream.PostgresUpstream) error {
	if s.grant.IsReadOnly() {
		if err := s.setSessionReadOnly(); err != nil {
			return fmt.Errorf("failed to set read-only mode: %w", err)
		}
	}

	if err := s.sendToClient(&pgproto3.AuthenticationOk{}); err != nil {
		return fmt.Errorf("failed to send auth ok: %w", err)
	}

	s.logger.DebugContext(s.ctx, "forwarding ParameterStatus messages to client",
		slog.Int("count", len(up.ParameterStatuses)))

	for _, ps := range up.ParameterStatuses {
		if err := s.sendToClient(ps); err != nil {
			return fmt.Errorf("failed to forward parameter status: %w", err)
		}
	}

	// Forward BackendKeyData (required by JDBC and other clients).
	if up.BackendKeyData != nil {
		if err := s.sendToClient(up.BackendKeyData); err != nil {
			return fmt.Errorf("failed to forward backend key data: %w", err)
		}

		// Remember the key we just handed the client: a CancelRequest carrying
		// it arrives on a *different* connection and has to be routed back to
		// this session.
		s.noteCancelKey(up.BackendKeyData)
	}

	if err := s.sendToClient(up.ReadyForQuery); err != nil {
		return fmt.Errorf("failed to forward ready message: %w", err)
	}

	return nil
}

// sendToClient sends a message to client and flushes.
func (s *Session) sendToClient(msg pgproto3.BackendMessage) error {
	s.clientBackend.Send(msg)

	return s.clientBackend.Flush()
}

// maxAppNameLen is the maximum length for PostgreSQL application_name (NAMEDATALEN - 1).
const maxAppNameLen = 63

// buildApplicationName constructs the application_name for upstream
// connections: "dbbat/$version @$username", plus " for $appName" when the
// client declared an application_name of its own. See
// shared.BuildUpstreamName for the truncation rules.
func buildApplicationName(username, clientAppName string) string {
	return shared.BuildUpstreamName(version.Version, username, clientAppName, maxAppNameLen)
}

// setSessionReadOnly sets the upstream session to read-only mode.
// This enforces read-only access at the PostgreSQL level for defense-in-depth.
func (s *Session) setSessionReadOnly() error {
	// Send SET SESSION command to upstream database
	query := &pgproto3.Query{
		String: "SET SESSION default_transaction_read_only = on;",
	}

	s.upstreamFrontend.Send(query)

	if err := s.upstreamFrontend.Flush(); err != nil {
		return fmt.Errorf("send SET SESSION: %w", err)
	}

	// Read response from upstream
	for {
		msg, err := s.upstreamFrontend.Receive()
		if err != nil {
			return fmt.Errorf("receive response: %w", err)
		}

		switch msg.(type) {
		case *pgproto3.CommandComplete:
			// Success - read-only mode is now enforced
			continue
		case *pgproto3.ReadyForQuery:
			// Session is ready
			return nil
		case *pgproto3.ErrorResponse:
			return fmt.Errorf("%w: %v", ErrUpstreamReadOnlyMode, msg)
		default:
			continue
		}
	}
}
