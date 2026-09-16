package mysql

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/fclairamb/dbbat/internal/proxy/shared"
	"github.com/fclairamb/dbbat/internal/proxy/upstream"
	"github.com/fclairamb/dbbat/internal/version"
)

// maxProgramNameLen bounds the "program_name" connection attribute dbbat
// sends upstream. MySQL's CLIENT_CONNECT_ATTRS extension has no hard
// protocol-level length limit on an individual attribute value, but
// performance_schema_session_connect_attrs_size (the server setting that
// governs how much of the combined attribute blob MySQL retains for
// performance_schema.session_connect_attrs) historically defaults to 512
// bytes across all attributes combined. Keep well under that so the
// dbbat-branded name survives alongside any other attributes the driver
// sends.
const maxProgramNameLen = 256

// buildUpstreamProgramName constructs the "program_name" connection
// attribute sent to the upstream MySQL/MariaDB server: "dbbat/$version
// @$username", plus " for $appName" when the client declared its own
// program_name attribute. See shared.BuildUpstreamName for the truncation
// rules.
func buildUpstreamProgramName(username, clientProgramName string) string {
	return shared.BuildUpstreamName(version.Version, username, clientProgramName, maxProgramNameLen)
}

// connectUpstream opens an authenticated MySQL connection to the upstream
// database configured for the session's grant. The session's encrypted
// password is decrypted in-memory using the per-database AAD key.
//
// The connect itself is upstream.ConnectMySQL — the same call the connectivity
// check makes, so the two cannot drift on TLS policy, connection attributes or
// capability flags.
func (s *Session) connectUpstream() error {
	if err := s.database.DecryptPassword(s.server.encryptionKey); err != nil {
		return fmt.Errorf("decrypt upstream password: %w", err)
	}

	up, err := upstream.ConnectMySQL(s.ctx, s.dialUpstream, s.upstreamConfig())
	if err != nil {
		return fmt.Errorf("%w: %w", ErrUpstreamConnect, err)
	}

	s.upstreamConn = up.Conn
	s.upstreamTLS = up.TLS

	// Remember the backend's connection id before anything can nil the conn:
	// it is what a KILL QUERY from the watchdog has to name, and by then the
	// session is being torn down.
	s.upstreamConnID = up.Conn.GetConnectionID()
	s.upstreamVersion = up.Conn.GetServerVersion()

	if err := s.pinStatementTimeout(); err != nil {
		_ = up.Close()
		s.upstreamConn = nil

		return err
	}

	s.logger.DebugContext(s.ctx, "upstream MySQL connected",
		slog.String("addr", net.JoinHostPort(s.database.Host, strconv.Itoa(s.database.Port))),
		slog.String("user", s.database.Username),
		slog.String("database", s.database.DatabaseName))

	return nil
}

// upstreamConfig projects the session's server row onto the shared connector's
// config, which is also where the client's own program_name attribute — sent
// during the client's handshake with dbbat and captured on s.serverConn — is
// folded into the name dbbat advertises upstream.
func (s *Session) upstreamConfig() upstream.MySQLConfig {
	var clientProgramName string
	if s.serverConn != nil {
		clientProgramName = s.serverConn.Attributes()["program_name"]
	}

	return upstream.MySQLConfig{
		Host:        s.database.Host,
		Port:        s.database.Port,
		Username:    s.database.Username,
		Password:    s.database.Password,
		Database:    s.database.DatabaseName,
		ProgramName: buildUpstreamProgramName(s.user.Username, clientProgramName),
		SSLMode:     s.database.SSLMode,
	}
}

// dialUpstream opens the transport to the target: a direct TCP dial, or a
// tunnel through the SSH bastion chain when the server row's via_uid is set.
func (s *Session) dialUpstream(ctx context.Context) (net.Conn, error) {
	return shared.DialUpstream(ctx, s.server.store, s.server.encryptionKey, s.database)
}

// closeUpstream closes the upstream connection if open.
func (s *Session) closeUpstream() {
	if s.upstreamConn == nil {
		return
	}

	if err := s.upstreamConn.Close(); err != nil {
		s.logger.DebugContext(s.ctx, "upstream close error", slog.Any("error", err))
	}

	s.upstreamConn = nil
}

// mariaDBMarker is how MariaDB identifies itself in the version string it sends
// during the handshake ("10.11.6-MariaDB-1:10.11.6+maria~ubu2204", and on older
// builds "5.5.5-10.4.11-MariaDB"). There is no capability flag for "is
// MariaDB", so the string is the signal.
const mariaDBMarker = "mariadb"

// isMariaDB reports whether the upstream is MariaDB rather than MySQL. The two
// spell the per-statement limit differently and neither accepts the other's
// name, so guessing wrong means the session fails to start.
func isMariaDB(serverVersion string) bool {
	return strings.Contains(strings.ToLower(serverVersion), mariaDBMarker)
}

// statementTimeoutSetup is the SET the upstream session is pinned with, or ""
// when no limit applies.
//
//   - MySQL: max_execution_time, in milliseconds. It only covers read-only
//     SELECTs — the watchdog is what covers everything else, which is the
//     whole reason this is defense in depth rather than the enforcement.
//   - MariaDB: max_statement_time, in seconds (fractional allowed), and it
//     covers more than SELECT.
func statementTimeoutSetup(limit time.Duration, serverVersion string) string {
	if limit <= 0 {
		return ""
	}

	if isMariaDB(serverVersion) {
		return fmt.Sprintf("SET SESSION max_statement_time = %g", limit.Seconds())
	}

	return fmt.Sprintf("SET SESSION max_execution_time = %d", limit.Milliseconds())
}

// pinStatementTimeout applies the grant's per-statement limit to the upstream
// session, before the client is told it is connected.
//
// A session that cannot be pinned fails rather than running unbounded — the
// same rule the PostgreSQL read-only pin has always had. The failure is real:
// an ancient server that knows neither variable name would otherwise look
// bounded and not be.
func (s *Session) pinStatementTimeout() error {
	stmt := statementTimeoutSetup(s.statementLimit, s.upstreamVersion)
	if stmt == "" {
		return nil
	}

	if _, err := s.upstreamConn.Execute(stmt); err != nil {
		return fmt.Errorf("%w: %w", ErrUpstreamStatementTimeout, err)
	}

	return nil
}

// killUpstreamStatement issues KILL QUERY against this session's upstream
// backend, on a *fresh* connection: the one running the statement is, by
// definition, not reading its socket.
//
// Best effort, and deliberately ahead of the socket close in the teardown
// order. Closing the sockets alone leaves the server executing the statement
// until it next notices the client is gone, which on a long scan can be a very
// long time — and that load is exactly what the limit exists to stop.
func (s *Session) killUpstreamStatement() {
	if s.upstreamConnID == 0 || s.database == nil {
		return
	}

	ctx, cancel := context.WithTimeout(s.ctx, mysqlKillTimeout)
	defer cancel()

	up, err := upstream.ConnectMySQL(ctx, s.dialUpstream, s.upstreamConfig())
	if err != nil {
		s.logger.WarnContext(s.ctx, "failed to open a connection to kill the upstream statement",
			slog.Any("error", err))

		return
	}

	defer func() { _ = up.Close() }()

	if _, err := up.Conn.Execute(fmt.Sprintf("KILL QUERY %d", s.upstreamConnID)); err != nil {
		s.logger.WarnContext(s.ctx, "failed to kill the upstream statement",
			slog.Uint64("upstream_connection_id", uint64(s.upstreamConnID)),
			slog.Any("error", err))

		return
	}

	s.logger.InfoContext(s.ctx, "killed the upstream statement",
		slog.Uint64("upstream_connection_id", uint64(s.upstreamConnID)))
}

// mysqlKillTimeout bounds the whole kill exchange — dial, login, one statement.
// A kill is housekeeping on a session that is already going away, so it must
// never be what keeps the teardown waiting.
const mysqlKillTimeout = 5 * time.Second
