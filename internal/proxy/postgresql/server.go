package postgresql

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"os"
	"sync"
	"time"

	"github.com/fclairamb/dbbat/internal/cache"
	"github.com/fclairamb/dbbat/internal/config"
	"github.com/fclairamb/dbbat/internal/dump"
	"github.com/fclairamb/dbbat/internal/store"
)

// Server is the PostgreSQL proxy server.
type Server struct {
	store         *store.Store
	encryptionKey []byte
	queryStorage  config.QueryStorageConfig
	dumpConfig    config.DumpConfig
	authCache     *cache.AuthCache
	logger        *slog.Logger

	// tlsConfig terminates client TLS at the proxy. nil when TLS is
	// disabled — sessions then refuse SSLRequest with 'N' as before.
	tlsConfig *tls.Config

	// listenerMu guards listener, which is written by Start and read
	// concurrently by Addr/Shutdown (e.g. tests polling Addr while Start runs
	// in a goroutine).
	listenerMu sync.Mutex
	listener   net.Listener
	wg         sync.WaitGroup
	shutdown   chan struct{}
	ctx        context.Context //nolint:containedctx // Context is needed for the server lifecycle
	cancel     context.CancelFunc
}

// NewServer creates a new proxy server.
func NewServer(
	dataStore *store.Store,
	encryptionKey []byte,
	queryStorage config.QueryStorageConfig,
	dumpConfig config.DumpConfig,
	authCache *cache.AuthCache,
	pgConfig config.PGConfig,
	logger *slog.Logger,
) (*Server, error) {
	tlsConfig, err := loadTLS(pgConfig)
	if err != nil {
		return nil, fmt.Errorf("PostgreSQL proxy TLS setup: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	return &Server{
		store:         dataStore,
		encryptionKey: encryptionKey,
		queryStorage:  queryStorage,
		dumpConfig:    dumpConfig,
		authCache:     authCache,
		tlsConfig:     tlsConfig,
		logger:        logger,
		shutdown:      make(chan struct{}),
		ctx:           ctx,
		cancel:        cancel,
	}, nil
}

// Start starts the proxy server.
func (s *Server) Start(addr string) error {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("failed to listen: %w", err)
	}

	s.setListener(listener)
	s.logger.InfoContext(s.ctx, "Proxy server listening", slog.String("addr", addr))

	// Start dump cleanup goroutine if dumps are enabled
	if s.dumpConfig.Dir != "" {
		if err := os.MkdirAll(s.dumpConfig.Dir, 0o755); err != nil {
			s.logger.ErrorContext(s.ctx, "failed to create dump directory", slog.String("dir", s.dumpConfig.Dir), slog.Any("error", err))
		} else {
			go s.runDumpCleanup()
		}
	}

	// Accept connections
	for {
		conn, err := listener.Accept()
		if err != nil {
			select {
			case <-s.shutdown:
				return nil
			default:
				s.logger.ErrorContext(s.ctx, "failed to accept connection", slog.Any("error", err))

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

// Addr returns the listener's bound address, or nil if the server has not
// started accepting connections yet. Useful in tests that pass ":0" for the
// listen address and need to discover the OS-assigned port.
func (s *Server) Addr() net.Addr {
	listener := s.getListener()
	if listener == nil {
		return nil
	}

	return listener.Addr()
}

// setListener stores the active listener under the guard.
func (s *Server) setListener(l net.Listener) {
	s.listenerMu.Lock()
	defer s.listenerMu.Unlock()
	s.listener = l
}

// getListener reads the active listener under the guard.
func (s *Server) getListener() net.Listener {
	s.listenerMu.Lock()
	defer s.listenerMu.Unlock()

	return s.listener
}

// Shutdown gracefully shuts down the server.
func (s *Server) Shutdown(ctx context.Context) error {
	close(s.shutdown)
	s.cancel()

	if listener := s.getListener(); listener != nil {
		if err := listener.Close(); err != nil {
			s.logger.ErrorContext(ctx, "failed to close listener", slog.Any("error", err))
		}
	}

	// Wait for all connections to finish (with timeout from context)
	done := make(chan struct{})

	go func() {
		s.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		s.logger.InfoContext(ctx, "Proxy server shutdown complete")

		return nil
	case <-ctx.Done():
		s.logger.WarnContext(ctx, "Proxy server shutdown timeout")

		return ctx.Err()
	}
}

// handleConnection handles a single client connection.
func (s *Server) handleConnection(clientConn net.Conn) {
	defer func() {
		if err := clientConn.Close(); err != nil {
			s.logger.ErrorContext(s.ctx, "failed to close client connection", slog.Any("error", err))
		}
	}()

	s.logger.DebugContext(s.ctx, "New connection", slog.Any("remote_addr", clientConn.RemoteAddr()))

	session := NewSession(clientConn, s.store, s.encryptionKey, s.logger, s.ctx, s.queryStorage, s.dumpConfig, s.authCache, s.tlsConfig)
	if err := session.Run(); err != nil {
		s.logger.ErrorContext(s.ctx, "Session error", slog.Any("error", err), slog.Any("remote_addr", clientConn.RemoteAddr()))
	}
}

const dumpCleanupInterval = 1 * time.Hour

// runDumpCleanup periodically cleans up old dump files.
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
				s.logger.ErrorContext(s.ctx, "dump cleanup failed", slog.Any("error", err))
			} else if deleted > 0 {
				s.logger.InfoContext(s.ctx, "cleaned up old dumps", slog.Int("deleted", deleted))
			}
		case <-s.shutdown:
			return
		}
	}
}
