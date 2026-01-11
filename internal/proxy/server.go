package proxy

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync"

	"github.com/fclairamb/dbbat/internal/cache"
	"github.com/fclairamb/dbbat/internal/config"
	"github.com/fclairamb/dbbat/internal/store"
)

// Server is the PostgreSQL proxy server.
type Server struct {
	store         *store.Store
	encryptionKey []byte
	queryStorage  config.QueryStorageConfig
	authCache     *cache.AuthCache
	logger        *slog.Logger
	listener      net.Listener
	wg            sync.WaitGroup
	shutdown      chan struct{}
	ctx           context.Context //nolint:containedctx // Context is needed for the server lifecycle
	cancel        context.CancelFunc
}

// NewServer creates a new proxy server.
func NewServer(
	dataStore *store.Store,
	encryptionKey []byte,
	queryStorage config.QueryStorageConfig,
	authCache *cache.AuthCache,
	logger *slog.Logger,
) *Server {
	ctx, cancel := context.WithCancel(context.Background())

	return &Server{
		store:         dataStore,
		encryptionKey: encryptionKey,
		queryStorage:  queryStorage,
		authCache:     authCache,
		logger:        logger,
		shutdown:      make(chan struct{}),
		ctx:           ctx,
		cancel:        cancel,
	}
}

// Start starts the proxy server.
func (s *Server) Start(addr string) error {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("failed to listen: %w", err)
	}

	s.listener = listener
	s.logger.Info("Proxy server listening", "addr", addr)

	// Accept connections
	for {
		conn, err := listener.Accept()
		if err != nil {
			select {
			case <-s.shutdown:
				return nil
			default:
				s.logger.Error("failed to accept connection", "error", err)

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
			s.logger.Error("failed to close listener", "error", err)
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
		s.logger.Info("Proxy server shutdown complete")

		return nil
	case <-ctx.Done():
		s.logger.Warn("Proxy server shutdown timeout")

		return ctx.Err()
	}
}

// handleConnection handles a single client connection.
func (s *Server) handleConnection(clientConn net.Conn) {
	defer func() {
		if err := clientConn.Close(); err != nil {
			s.logger.Error("failed to close client connection", "error", err)
		}
	}()

	s.logger.Debug("New connection", "remote_addr", clientConn.RemoteAddr())

	session := NewSession(clientConn, s.store, s.encryptionKey, s.logger, s.ctx, s.queryStorage, s.authCache)
	if err := session.Run(); err != nil {
		s.logger.Error("Session error", "error", err, "remote_addr", clientConn.RemoteAddr())
	}
}
