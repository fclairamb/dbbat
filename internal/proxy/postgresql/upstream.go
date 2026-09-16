package postgresql

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync/atomic"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/fclairamb/dbbat/internal/proxy/shared"
	"github.com/fclairamb/dbbat/internal/proxy/upstream"
	"github.com/fclairamb/dbbat/internal/version"
)

// skipStatementTimeoutSetup suppresses the server-side `SET SESSION
// statement_timeout`, leaving only dbbat's own watchdog.
//
// A test seam, and one that earns its place in product code: the two layers are
// designed so the polite one normally wins, which means the watchdog — the layer
// the whole feature actually rests on — is never exercised end to end unless the
// polite one can be turned off. Nothing outside the integration suite writes it,
// and with it unset the behavior is exactly as if it did not exist.
var skipStatementTimeoutSetup atomic.Bool

// ErrUpstreamReadOnlyMode is returned when the upstream fails to set read-only
// mode. Kept as the historical name; ErrUpstreamSessionSetup is what the
// generalized setup path actually returns, and it wraps to this so callers and
// tests that matched on read-only still do.
var ErrUpstreamReadOnlyMode = errors.New("upstream error setting read-only mode")

// ErrUpstreamSessionSetup is returned when the upstream refuses one of the
// session-setup statements (the read-only pin, the statement limit). A session
// that cannot be pinned is failed rather than served unbounded.
var ErrUpstreamSessionSetup = fmt.Errorf("%w (session setup)", ErrUpstreamReadOnlyMode)

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
		ApplicationName: buildApplicationName(s.user.Username, s.connUID, s.clientApplicationName),
		SSLMode:         s.database.SSLMode,
	}, s.logger)
	if err != nil {
		return s.reportUpstreamConnectFailure(err)
	}

	s.upstreamConn = up.Conn
	s.upstreamFrontend = up.Frontend
	s.upstreamTLS = up.TLS

	// Remember the server's cancellation key for *dbbat's* use: the watchdog
	// cancels a runaway statement with it, on a fresh connection. It is the
	// same key that is forwarded to the client a few lines down, but the two
	// uses are independent and only one of them survives a client that never
	// asked for one.
	s.upstreamKey = up.BackendKeyData

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
	if err := s.runUpstreamSetup(s.upstreamSetupStatements()...); err != nil {
		return err
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
// connections: "dbbat/$version @$username c=$uidSuffix", plus " for
// $appName" when the client declared an application_name of its own. See
// shared.BuildUpstreamName for the truncation rules.
func buildApplicationName(username string, connUID uuid.UUID, clientAppName string) string {
	return shared.BuildUpstreamName(version.Version, username, connUID, clientAppName, maxAppNameLen)
}

// upstreamSetupStatements is the session state dbbat pins on the upstream
// before the client is told it is connected: the grant's controls expressed in
// the server's own terms.
//
// Both entries are defense in depth rather than the enforcement itself — dbbat
// refuses a write and kills an over-time statement on its own — but both change
// what the *client* sees when it crosses the line: a real PostgreSQL error with
// a real SQLSTATE (25006 for a write, 57014 for a canceled statement) instead
// of a dropped socket.
func (s *Session) upstreamSetupStatements() []string {
	var stmts []string

	if s.grant.IsReadOnly() {
		stmts = append(stmts, "SET SESSION default_transaction_read_only = on;")
	}

	if s.statementLimit > 0 && !skipStatementTimeoutSetup.Load() {
		// Milliseconds: statement_timeout's bare-integer unit, and the one
		// every version accepts without a unit suffix being parsed.
		stmts = append(stmts, fmt.Sprintf("SET SESSION statement_timeout = %d;",
			s.statementLimit.Milliseconds()))
	}

	return stmts
}

// runUpstreamSetup issues the session-setup statements on the upstream leg,
// before the client has been told it is connected.
//
// A session that cannot be pinned fails rather than serving one unbounded (or
// one writable) query: the same rule the read-only pin has always had, now
// covering the statement limit too. The statements are sent as one batch and
// answered by one ReadyForQuery, since PostgreSQL's simple-query protocol
// allows several statements in a single Query message.
func (s *Session) runUpstreamSetup(stmts ...string) error {
	if len(stmts) == 0 {
		return nil
	}

	s.upstreamFrontend.Send(&pgproto3.Query{String: strings.Join(stmts, " ")})

	if err := s.upstreamFrontend.Flush(); err != nil {
		return fmt.Errorf("send session setup: %w", err)
	}

	for {
		msg, err := s.upstreamFrontend.Receive()
		if err != nil {
			return fmt.Errorf("receive session setup response: %w", err)
		}

		switch msg.(type) {
		case *pgproto3.CommandComplete:
			// One per statement in the batch; keep reading until the server
			// says it is ready again.
			continue
		case *pgproto3.ReadyForQuery:
			return nil
		case *pgproto3.ErrorResponse:
			return fmt.Errorf("%w: %v", ErrUpstreamSessionSetup, msg)
		default:
			continue
		}
	}
}
