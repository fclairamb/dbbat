package mysql

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"time"

	gomysqlclient "github.com/go-mysql-org/go-mysql/client"
	gomysqlserver "github.com/go-mysql-org/go-mysql/server"

	"github.com/fclairamb/dbbat/internal/dump"
	"github.com/fclairamb/dbbat/internal/store"
)

// Session is a single client connection through the MySQL proxy.
type Session struct {
	server     *Server
	clientConn net.Conn

	// Populated as the handshake progresses.
	user         *store.User
	database     *store.Database
	grant        *store.Grant
	requestedDB  string // captured by Handler.UseDB during handshake
	authComplete bool

	// go-mysql server-side wrapper around the client connection. Set after
	// NewCustomizedConn returns (i.e., after handshake completes).
	serverConn *gomysqlserver.Conn

	// Upstream MySQL connection.
	upstreamConn *gomysqlclient.Conn

	// DBBat connection record (insert on connect, close on disconnect).
	connection *store.Connection

	// Optional packet dump for the post-auth phase (matches PG behavior).
	dumpWriter *dump.Writer

	logger *slog.Logger
	ctx    context.Context //nolint:containedctx // Session-scoped context
}

func newSession(clientConn net.Conn, server *Server) *Session {
	return &Session{
		server:     server,
		clientConn: clientConn,
		logger:     server.logger,
		ctx:        server.ctx,
	}
}

// Run drives the session lifecycle:
//  1. MySQL handshake with auth termination
//  2. Connect to upstream MySQL using stored credentials
//  3. Record the connection in the DBBat store
//  4. Loop dispatching client commands via the Handler interface
//  5. Mark the connection closed on shutdown
func (s *Session) Run() error {
	authHandler := &dbbatAuthHandler{session: s}
	commandHandler := &handler{session: s}

	conn, err := s.server.gomysqlServer.NewCustomizedConn(s.clientConn, authHandler, commandHandler)
	if err != nil {
		return fmt.Errorf("MySQL handshake: %w", err)
	}

	s.serverConn = conn

	if err := s.connectUpstream(); err != nil {
		return err
	}

	defer s.closeUpstream()

	if err := s.recordConnection(); err != nil {
		// Don't fail the session if the audit insert fails — log and continue.
		s.logger.WarnContext(s.ctx, "MySQL connection insert failed", slog.Any("error", err))
	}

	defer s.recordDisconnect()

	s.startDumpIfConfigured()
	defer s.closeDump()

	s.logger.InfoContext(s.ctx, "MySQL session ready",
		slog.String("user", s.user.Username),
		slog.String("database", s.database.Name),
		slog.Any("remote_addr", s.clientConn.RemoteAddr()))

	return s.commandLoop()
}

// commandLoop dispatches client commands until the connection closes.
// HandleCommand returns nil only for COM_QUIT and io.EOF; all other terminal
// errors are returned and surface to the caller.
func (s *Session) commandLoop() error {
	for {
		if err := s.serverConn.HandleCommand(); err != nil {
			if errors.Is(err, io.EOF) || s.serverConn.Closed() {
				return nil
			}

			return fmt.Errorf("MySQL command: %w", err)
		}

		if s.serverConn.Closed() {
			return nil
		}
	}
}

func (s *Session) recordConnection() error {
	conn, err := s.server.store.CreateConnection(
		s.ctx,
		s.user.UID,
		s.database.UID,
		store.ExtractSourceIP(s.clientConn.RemoteAddr()),
	)
	if err != nil {
		return fmt.Errorf("create connection: %w", err)
	}

	s.connection = conn

	return nil
}

func (s *Session) recordDisconnect() {
	if s.connection == nil {
		return
	}

	if err := s.server.store.CloseConnection(s.ctx, s.connection.UID); err != nil {
		s.logger.WarnContext(s.ctx, "MySQL connection close failed",
			slog.Any("connection_id", s.connection.UID),
			slog.Any("error", err))
	}
}

// startDumpIfConfigured opens a packet-dump file for this session and tees the
// underlying client connection through it. We only capture the post-auth phase
// (matching the PG proxy) — the dump is wired up after recordConnection has
// run, so the auth handshake is already done by the time bytes start flowing
// through the tap.
//
// For TLS-upgraded connections the library has already replaced
// c.Conn.Conn with a *tls.Conn before this point; the tap will see TLS
// records (encrypted application data), which is fine — the dump captures
// timing and packet boundaries even when payload is opaque.
func (s *Session) startDumpIfConfigured() {
	if s.server.dumpConfig.Dir == "" || s.connection == nil || s.serverConn == nil {
		return
	}

	dumpPath := filepath.Join(s.server.dumpConfig.Dir, s.connection.UID.String()+dump.FileExt)

	dw, err := dump.NewWriter(dumpPath, dump.Header{
		SessionID: s.connection.UID.String(),
		Protocol:  dump.ProtocolMySQL,
		StartTime: time.Now(),
		Connection: map[string]any{
			"database":      s.database.DatabaseName,
			"user":          s.user.Username,
			"upstream_addr": net.JoinHostPort(s.database.Host, fmt.Sprintf("%d", s.database.Port)),
			"protocol":      s.database.Protocol,
		},
	}, s.server.dumpConfig.MaxSize)
	if err != nil {
		s.logger.WarnContext(s.ctx, "MySQL dump writer create failed", slog.Any("error", err))

		return
	}

	s.dumpWriter = dw

	// Replace the underlying net.Conn on the live packet.Conn with a tap.
	// Subsequent reads/writes from go-mysql go through the tap; the buffered
	// reader inside packet.Conn ultimately calls the embedded conn's Read,
	// which now hits our tap.
	if s.serverConn.Conn != nil {
		s.serverConn.Conn.Conn = dump.NewTapConn(
			s.serverConn.Conn.Conn,
			dw,
			dump.DirClientToServer,
			dump.DirServerToClient,
		)
	}
}

func (s *Session) closeDump() {
	if s.dumpWriter == nil {
		return
	}

	if err := s.dumpWriter.Close(); err != nil {
		s.logger.WarnContext(s.ctx, "MySQL dump writer close failed", slog.Any("error", err))
	}

	s.dumpWriter = nil
}
