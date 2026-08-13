package oracle

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/fclairamb/dbbat/internal/cache"
	"github.com/fclairamb/dbbat/internal/config"
	"github.com/fclairamb/dbbat/internal/dump"
	"github.com/fclairamb/dbbat/internal/proxy/shared"
	"github.com/fclairamb/dbbat/internal/store"
)

// Server is the Oracle proxy server.
type Server struct {
	// approvalDeps carries the approval-hold collaborators. Zero value =
	// feature off, which is the default.
	approvalDeps shared.ApprovalDeps

	store         *store.Store
	encryptionKey []byte
	authCache     *cache.AuthCache

	// rowWriter batches captured result rows off the capture path. Defaults
	// to a writer private to this server; main.go replaces it with the
	// process-wide one so batches span protocols as well as sessions.
	rowWriter    *shared.RowWriter
	queryStorage config.QueryStorageConfig
	dumpConfig   config.DumpConfig
	// dumpUploader ships finished captures from the local spool to blob
	// storage. nil — the default — keeps them on local disk only.
	dumpUploader *dump.Uploader
	logger       *slog.Logger
	// listenerMu guards listener, which is written by Start and read
	// concurrently by Addr/Shutdown (e.g. tests polling Addr while Start runs
	// in a goroutine).
	listenerMu sync.Mutex
	listener   net.Listener
	listenAddr string
	wg         sync.WaitGroup
	shutdown   chan struct{}
	ctx        context.Context //nolint:containedctx
	cancel     context.CancelFunc
}

// NewServer creates a new Oracle proxy server.
func NewServer(
	dataStore *store.Store,
	encryptionKey []byte,
	authCache *cache.AuthCache,
	queryStorage config.QueryStorageConfig,
	dumpConfig config.DumpConfig,
	logger *slog.Logger,
) *Server {
	ctx, cancel := context.WithCancel(context.Background())

	var rowWriter *shared.RowWriter
	if dataStore != nil {
		rowWriter = shared.NewRowWriter(dataStore, logger)
	}

	return &Server{
		store:         dataStore,
		rowWriter:     rowWriter,
		encryptionKey: encryptionKey,
		authCache:     authCache,
		queryStorage:  queryStorage,
		dumpConfig:    dumpConfig,
		logger:        logger.With("component", "oracle-proxy"),
		shutdown:      make(chan struct{}),
		ctx:           ctx,
		cancel:        cancel,
	}
}

// Start starts the Oracle proxy server.
func (s *Server) Start(addr string) error {
	// Reserve a wg slot for Start's lifetime so Shutdown's wg.Wait never sees
	// a zero counter racing with the wg.Add for an in-flight accepted conn.
	s.wg.Add(1)
	defer s.wg.Done()

	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("failed to listen: %w", err)
	}

	s.setListener(listener)
	s.listenAddr = addr
	s.logger.InfoContext(s.ctx, "Oracle proxy server listening", slog.String("addr", addr),
		slog.String("dump_dir", s.dumpConfig.Dir))

	// Start dump cleanup goroutine if dumps are enabled
	if s.dumpConfig.Dir != "" {
		if err := os.MkdirAll(s.dumpConfig.Dir, 0o755); err != nil {
			s.logger.ErrorContext(s.ctx, "failed to create dump directory", slog.String("dir", s.dumpConfig.Dir), slog.Any("error", err))
		} else {
			go s.runDumpCleanup()
		}
	}

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

	if listener := s.getListener(); listener != nil {
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

// handleConnection handles a single Oracle client connection.
func (s *Server) handleConnection(clientConn net.Conn) {
	defer func() {
		if r := recover(); r != nil {
			s.logger.ErrorContext(s.ctx, "Oracle session panic",
				slog.Any("panic", r),
				slog.String("stack", string(debug.Stack())),
				slog.Any("remote_addr", clientConn.RemoteAddr()))
		}

		if err := clientConn.Close(); err != nil {
			s.logger.ErrorContext(s.ctx, "failed to close client connection", slog.Any("error", err))
		}
	}()

	s.logger.DebugContext(s.ctx, "New Oracle connection", slog.Any("remote_addr", clientConn.RemoteAddr()))

	session := newSession(clientConn, s.store, s.encryptionKey, s.logger, s.ctx, s.authCache, s.queryStorage, s.dumpConfig, s.rowWriter)
	session.approvalDeps = s.approvalDeps
	session.dumpUploader = s.dumpUploader
	if err := session.run(); err != nil {
		// Health check probes (NLB, etc.) connect and immediately close — log at debug level
		errStr := err.Error()
		if strings.Contains(errStr, "failed to read connect packet") || strings.Contains(errStr, "EOF") {
			s.logger.DebugContext(s.ctx, "Oracle connection closed early (likely health check)",
				slog.Any("remote_addr", clientConn.RemoteAddr()))
		} else {
			s.logger.ErrorContext(s.ctx, "Oracle session error",
				slog.Any("error", err),
				slog.Any("remote_addr", clientConn.RemoteAddr()))
		}
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
			// One turn under the guard rather than the whole loop: a panic in a
			// sweep must not take the process down, and must not retire retention
			// for the life of the process either. See shared.RunMaintenance.
			shared.RunMaintenance(s.ctx, s.logger, goroutineNameDumpRetention, func() {
				deleted, err := dump.CleanupOldFiles(s.dumpConfig.Dir, retention)
				if err != nil {
					s.logger.ErrorContext(s.ctx, "dump cleanup failed", slog.Any("error", err))
				} else if deleted > 0 {
					s.logger.InfoContext(s.ctx, "cleaned up old dumps", slog.Int("deleted", deleted))
				}
			})
		case <-s.shutdown:
			return
		}
	}
}

// SetApprovalDeps installs the approval-hold collaborators. A server without
// them never holds anything.
func (s *Server) SetApprovalDeps(deps shared.ApprovalDeps) {
	s.approvalDeps = deps
}

// SetRowWriter installs the process-wide result-row writer, replacing (and
// shutting down) the private one NewServer created. Batching across every
// protocol is where the design pays off most: a busy proxy with many small
// result sets then issues one INSERT per ~1000 rows overall rather than one
// per query.
func (s *Server) SetRowWriter(writer *shared.RowWriter) {
	if writer == nil || writer == s.rowWriter {
		return
	}

	previous := s.rowWriter
	s.rowWriter = writer

	previous.Close(s.ctx)
}

// SetDumpUploader installs the process-wide capture uploader, so this proxy's
// finished captures are shipped to blob storage on session close. Called by the
// process wiring; nil means local-only captures, which is the default.
func (s *Server) SetDumpUploader(uploader *dump.Uploader) {
	s.dumpUploader = uploader
}
