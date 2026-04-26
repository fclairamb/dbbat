package mysql

import (
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"strconv"

	gomysqlclient "github.com/go-mysql-org/go-mysql/client"
	gomysql "github.com/go-mysql-org/go-mysql/mysql"

	"github.com/fclairamb/dbbat/internal/version"
)

// connectUpstream opens an authenticated MySQL connection to the upstream
// database configured for the session's grant. The session's encrypted
// password is decrypted in-memory using the per-database AAD key.
//
// The upstream connection's authentication plugin is whatever the upstream
// server prefers (typically caching_sha2_password for MySQL 8.x). go-mysql's
// client handles plugin negotiation transparently — this is the
// caching_sha2_password support we deliberately did NOT implement on the
// server-facing side.
func (s *Session) connectUpstream() error {
	if err := s.database.DecryptPassword(s.server.encryptionKey); err != nil {
		return fmt.Errorf("decrypt upstream password: %w", err)
	}

	addr := net.JoinHostPort(s.database.Host, strconv.Itoa(s.database.Port))

	conn, err := gomysqlclient.Connect(
		addr,
		s.database.Username,
		s.database.Password,
		s.database.DatabaseName,
		s.applyUpstreamOptions,
	)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrUpstreamConnect, err)
	}

	s.upstreamConn = conn

	s.logger.DebugContext(s.ctx, "upstream MySQL connected",
		slog.String("addr", addr),
		slog.String("user", s.database.Username),
		slog.String("database", s.database.DatabaseName))

	return nil
}

// applyUpstreamOptions configures the upstream client connection: TLS mode
// from the database's ssl_mode column, a connection attribute identifying
// dbbat as the application, and explicit refusal of CLIENT_LOCAL_FILES so a
// compromised upstream cannot ask the proxy to upload arbitrary files via
// `LOAD DATA LOCAL INFILE` mid-query.
func (s *Session) applyUpstreamOptions(c *gomysqlclient.Conn) error {
	switch s.database.SSLMode {
	case "require":
		c.UseSSL(true) // skip cert verification
	case "verify-ca", "verify-full":
		c.SetTLSConfig(&tls.Config{MinVersion: tls.VersionTLS12, ServerName: s.database.Host})
	case "", "disable", "prefer", "allow":
		// plaintext upstream — also the path for "prefer"/"allow" since the
		// client doesn't currently negotiate opportunistic TLS for MySQL
	}

	// Defense-in-depth: refuse the LOCAL INFILE capability on the upstream
	// connection. The shared SQL validator already blocks the keyword in
	// inbound client SQL, but a malicious upstream could still issue a
	// LOCAL_INFILE_REQUEST (0xFB) packet as part of any query response — and
	// the go-mysql client would happily read the file from the proxy host's
	// filesystem unless we opt out at handshake time.
	c.UnsetCapability(gomysql.CLIENT_LOCAL_FILES)

	c.SetAttributes(map[string]string{
		"program_name": "dbbat-" + version.Version,
	})

	return nil
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
