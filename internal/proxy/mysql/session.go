package mysql

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"

	gomysqlclient "github.com/go-mysql-org/go-mysql/client"
	gomysqlserver "github.com/go-mysql-org/go-mysql/server"

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
