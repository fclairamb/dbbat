package mysql

import (
	"context"
	"fmt"
	"log/slog"
	"net"

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

// Run drives the session lifecycle.
//
// Phase 1b/1c: complete the MySQL handshake (TLS refusal, user/db lookup,
// password verification, grant check) then close. Phase 1d adds upstream
// connect and command forwarding.
func (s *Session) Run() error {
	authHandler := &dbbatAuthHandler{session: s}
	commandHandler := &handler{session: s}

	conn, err := s.server.gomysqlServer.NewCustomizedConn(s.clientConn, authHandler, commandHandler)
	if err != nil {
		return fmt.Errorf("MySQL handshake: %w", err)
	}

	s.serverConn = conn

	s.logger.InfoContext(s.ctx, "MySQL session authenticated",
		slog.String("user", s.user.Username),
		slog.String("database", s.database.Name),
		slog.Any("remote_addr", s.clientConn.RemoteAddr()))

	// Phase 1d will replace this with: connectUpstream, recordConnection,
	// then a command loop calling s.serverConn.HandleCommand().
	return nil
}
