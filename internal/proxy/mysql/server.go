package mysql

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	gomysqlserver "github.com/go-mysql-org/go-mysql/server"

	"github.com/fclairamb/dbbat/internal/cache"
	"github.com/fclairamb/dbbat/internal/config"
	"github.com/fclairamb/dbbat/internal/dump"
	"github.com/fclairamb/dbbat/internal/store"
)

// Server is the MySQL proxy server. It accepts MySQL client connections,
// authenticates them against the DBBat user store, and proxies commands to
// the upstream MySQL database configured for the requested schema.
type Server struct {
	store         *store.Store
	encryptionKey []byte
	queryStorage  config.QueryStorageConfig
	dumpConfig    config.DumpConfig
	authCache     *cache.AuthCache
	logger        *slog.Logger

	// Shared go-mysql server config used for every accepted connection.
	gomysqlServer *gomysqlserver.Server

	listener net.Listener
	wg       sync.WaitGroup
	shutdown chan struct{}
	ctx      context.Context //nolint:containedctx // Context is needed for the server lifecycle
	cancel   context.CancelFunc
}

// NewServer creates a new MySQL proxy server.
func NewServer(
	dataStore *store.Store,
	encryptionKey []byte,
	queryStorage config.QueryStorageConfig,
	dumpConfig config.DumpConfig,
	authCache *cache.AuthCache,
	logger *slog.Logger,
) *Server {
	ctx, cancel := context.WithCancel(context.Background())

	s := &Server{
		store:         dataStore,
		encryptionKey: encryptionKey,
		queryStorage:  queryStorage,
		dumpConfig:    dumpConfig,
		authCache:     authCache,
		logger:        logger,
		shutdown:      make(chan struct{}),
		ctx:           ctx,
		cancel:        cancel,
	}
	s.gomysqlServer = newGoMySQLServer(s)

	return s
}

// Start starts the proxy server.
func (s *Server) Start(addr string) error {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("failed to listen: %w", err)
	}

	s.listener = listener
	s.logger.InfoContext(s.ctx, "MySQL proxy server listening", slog.String("addr", addr))

	if s.dumpConfig.Dir != "" {
		go s.runDumpCleanup()
	}

	for {
		conn, err := listener.Accept()
		if err != nil {
			select {
			case <-s.shutdown:
				return nil
			default:
				s.logger.ErrorContext(s.ctx, "MySQL accept failed", slog.Any("error", err))

				continue
			}
		}

		s.wg.Add(1)

		go func() {
			defer s.wg.Done()
			s.handleConnection(conn)
		}()
	}
}

// Shutdown gracefully shuts down the server.
func (s *Server) Shutdown(ctx context.Context) error {
	close(s.shutdown)
	s.cancel()

	if s.listener != nil {
		if err := s.listener.Close(); err != nil {
			s.logger.ErrorContext(ctx, "failed to close MySQL listener", slog.Any("error", err))
		}
	}

	done := make(chan struct{})

	go func() {
		s.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		s.logger.InfoContext(ctx, "MySQL proxy server shutdown complete")

		return nil
	case <-ctx.Done():
		s.logger.WarnContext(ctx, "MySQL proxy server shutdown timeout")

		return ctx.Err()
	}
}

func (s *Server) handleConnection(clientConn net.Conn) {
	defer func() {
		if err := clientConn.Close(); err != nil {
			s.logger.DebugContext(s.ctx, "client close error", slog.Any("error", err))
		}
	}()

	s.logger.DebugContext(s.ctx, "MySQL connection accepted",
		slog.Any("remote_addr", clientConn.RemoteAddr()))

	session := newSession(clientConn, s)
	if err := session.Run(); err != nil {
		s.logger.InfoContext(s.ctx, "MySQL session ended",
			slog.Any("remote_addr", clientConn.RemoteAddr()),
			slog.Any("error", err))
	}
}

const dumpCleanupInterval = 1 * time.Hour

func (s *Server) runDumpCleanup() {
	retention, err := time.ParseDuration(s.dumpConfig.Retention)
	if err != nil {
		retention = 24 * time.Hour
	}

	ticker := time.NewTicker(dumpCleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			deleted, err := dump.CleanupOldFiles(s.dumpConfig.Dir, retention)
			if err != nil {
				s.logger.ErrorContext(s.ctx, "MySQL dump cleanup failed", slog.Any("error", err))
			} else if deleted > 0 {
				s.logger.InfoContext(s.ctx, "MySQL dump cleanup", slog.Int("deleted", deleted))
			}
		case <-s.shutdown:
			return
		}
	}
}
