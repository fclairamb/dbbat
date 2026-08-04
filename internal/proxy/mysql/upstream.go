package mysql

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strconv"

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
