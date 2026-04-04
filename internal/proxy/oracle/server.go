package oracle

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

// Server is the Oracle proxy server.
type Server struct {
	store         *store.Store
	encryptionKey []byte
	authCache     *cache.AuthCache
	queryStorage  config.QueryStorageConfig
	logger        *slog.Logger
	mu            sync.Mutex
	listener      net.Listener
	listenAddr    string
	wg            sync.WaitGroup
	shutdown      chan struct{}
	ctx           context.Context //nolint:containedctx
	cancel        context.CancelFunc
}

// NewServer creates a new Oracle proxy server.
func NewServer(
	dataStore *store.Store,
	encryptionKey []byte,
	authCache *cache.AuthCache,
	queryStorage config.QueryStorageConfig,
	logger *slog.Logger,
) *Server {
	ctx, cancel := context.WithCancel(context.Background())

	return &Server{
		store:         dataStore,
		encryptionKey: encryptionKey,
		authCache:     authCache,
		queryStorage:  queryStorage,
		logger:        logger.With("component", "oracle-proxy"),
		shutdown:      make(chan struct{}),
		ctx:           ctx,
		cancel:        cancel,
	}
}

// Start starts the Oracle proxy server.
func (s *Server) Start(addr string) error {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("failed to listen: %w", err)
	}

	s.mu.Lock()
	s.listener = listener
	s.listenAddr = addr
	s.mu.Unlock()
	s.logger.InfoContext(s.ctx, "Oracle proxy server listening", slog.String("addr", addr))

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

// Shutdown gracefully shuts down the server.
func (s *Server) Shutdown(ctx context.Context) error {
	close(s.shutdown)
	s.cancel()

	s.mu.Lock()
	listener := s.listener
	s.mu.Unlock()

	if listener != nil {
		if err := listener.Close(); err != nil {
			s.logger.ErrorContext(ctx, "failed to close listener", slog.Any("error", err))
		}
	}

	done := make(chan struct{})

	go func() {
		s.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		s.logger.InfoContext(ctx, "Oracle proxy server shutdown complete")

		return nil
	case <-ctx.Done():
		s.logger.WarnContext(ctx, "Oracle proxy server shutdown timeout")

		return ctx.Err()
	}
}

// Addr returns the listener address, useful for tests with ":0" port.
func (s *Server) Addr() net.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.listener != nil {
		return s.listener.Addr()
	}

	return nil
}

// handleConnection handles a single Oracle client connection.
func (s *Server) handleConnection(clientConn net.Conn) {
	defer func() {
		if r := recover(); r != nil {
			s.logger.ErrorContext(s.ctx, "Oracle session panic",
				slog.Any("panic", r),
				slog.Any("remote_addr", clientConn.RemoteAddr()))
		}

		if err := clientConn.Close(); err != nil {
			s.logger.ErrorContext(s.ctx, "failed to close client connection", slog.Any("error", err))
		}
	}()

	s.logger.DebugContext(s.ctx, "New Oracle connection", slog.Any("remote_addr", clientConn.RemoteAddr()))

	session := newSession(clientConn, s.store, s.encryptionKey, s.logger, s.ctx, s.authCache, s.queryStorage)
	if err := session.run(); err != nil {
		s.logger.ErrorContext(s.ctx, "Oracle session error",
			slog.Any("error", err),
			slog.Any("remote_addr", clientConn.RemoteAddr()))
	}
}
