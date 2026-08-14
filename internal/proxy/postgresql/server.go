package postgresql

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"runtime/debug"
	"sync"
	"time"

	"github.com/fclairamb/dbbat/internal/cache"
	"github.com/fclairamb/dbbat/internal/config"
	"github.com/fclairamb/dbbat/internal/dump"
	"github.com/fclairamb/dbbat/internal/proxy/shared"
	"github.com/fclairamb/dbbat/internal/safe"
	"github.com/fclairamb/dbbat/internal/store"
)

// Server is the PostgreSQL proxy server.
type Server struct {
	store         *store.Store
	encryptionKey []byte
	queryStorage  config.QueryStorageConfig
	dumpConfig    config.DumpConfig
	// dumpUploader ships finished captures from the local spool to blob
	// storage. nil — the default — keeps them on local disk only.
	dumpUploader *dump.Uploader
	authCache    *cache.AuthCache
	logger       *slog.Logger

	// rowWriter batches captured result rows off the capture path. Defaults
	// to a writer private to this server; main.go replaces it with the
	// process-wide one so batches span protocols as well as sessions.
	rowWriter *shared.RowWriter

	// tlsConfig terminates client TLS at the proxy. nil when TLS is
	// disabled — sessions then refuse SSLRequest with 'N' as before.
	tlsConfig *tls.Config

	// approvalDeps carries the approval-hold collaborators (broker, registry,
	// store, Slack escalator). Zero value = feature off.
	approvalDeps shared.ApprovalDeps

	// cancels routes PostgreSQL CancelRequests, which arrive on their own TCP
	// connection, back to the session that owns the backend key.
	cancels *cancelRegistry

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

	var rowWriter *shared.RowWriter
	if dataStore != nil {
		rowWriter = shared.NewRowWriter(dataStore, logger)
	}

	return &Server{
		store:         dataStore,
		rowWriter:     rowWriter,
		encryptionKey: encryptionKey,
		queryStorage:  queryStorage,
		dumpConfig:    dumpConfig,
		authCache:     authCache,
		tlsConfig:     tlsConfig,
		logger:        logger,
		shutdown:      make(chan struct{}),
		ctx:           ctx,
		cancel:        cancel,
		cancels:       newCancelRegistry(),
	}, nil
}

// SetApprovalDeps installs the approval-hold collaborators. Called by the
// wiring in main; a server without them simply never holds anything.
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

// Start starts the proxy server.
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
//
// It runs on a goroutine of its own, so the recover is not optional: without it
// a panic anywhere in the session's own leg — auth, startup negotiation,
// anything before the relays split off — ends the process and with it every
// other live session. The relays themselves are covered separately, in
// proxyMessages, because a recover here does not reach them.
func (s *Server) handleConnection(clientConn net.Conn) {
	defer func() {
		if r := recover(); r != nil {
			s.logger.ErrorContext(s.ctx, "PostgreSQL session panic",
				slog.Any("panic", r),
				slog.String("stack", string(debug.Stack())),
				slog.Any("remote_addr", clientConn.RemoteAddr()))
		}

		if err := clientConn.Close(); err != nil {
			s.logger.ErrorContext(s.ctx, "failed to close client connection", slog.Any("error", err))
		}
	}()

	s.logger.DebugContext(s.ctx, "New connection", slog.Any("remote_addr", clientConn.RemoteAddr()))

	session := NewSession(clientConn, s.store, s.encryptionKey, s.logger, s.ctx, s.queryStorage, s.dumpConfig, s.authCache, s.tlsConfig, s.rowWriter)
	session.approvalDeps = s.approvalDeps
	session.cancels = s.cancels
	session.dumpUploader = s.dumpUploader

	if err := session.Run(); err != nil {
		// A CancelRequest is a normal, expected one-shot connection, not a
		// session failure — logging it as an error would be noise on every
		// client-side Ctrl-C.
		if errors.Is(err, ErrCancelRequestHandled) {
			return
		}

		s.logger.ErrorContext(s.ctx, "Session error", slog.Any("error", err), slog.Any("remote_addr", clientConn.RemoteAddr()))
	}
}

const dumpCleanupInterval = 1 * time.Hour

// goroutineNameDumpRetention is what a panic in a retention sweep is logged under.
const goroutineNameDumpRetention = "postgresql dump retention sweep"

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
			// for the life of the process either. See safe.RunMaintenance.
			safe.RunMaintenance(s.ctx, s.logger, goroutineNameDumpRetention, func() {
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

// SetDumpUploader installs the process-wide capture uploader, so this proxy's
// finished captures are shipped to blob storage on session close. Called by the
// process wiring; nil means local-only captures, which is the default.
func (s *Server) SetDumpUploader(uploader *dump.Uploader) {
	s.dumpUploader = uploader
}
