package mongodb

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/fclairamb/dbbat/internal/cache"
	"github.com/fclairamb/dbbat/internal/config"
	"github.com/fclairamb/dbbat/internal/dump"
	"github.com/fclairamb/dbbat/internal/proxy/shared"
	"github.com/fclairamb/dbbat/internal/store"
)

// Server is the MongoDB proxy server. It accepts MongoDB client connections,
// authenticates them against the DBBat user store (SASL PLAIN / dbb_ API
// keys), and proxies commands to the upstream MongoDB configured for the
// requested database.
type Server struct {
	// approvalDeps carries the approval-hold collaborators. Zero value =
	// feature off, which is the default.
	approvalDeps shared.ApprovalDeps

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

	// tlsConfig supports client-facing TLS termination. Nil when TLS is
	// explicitly disabled in config.
	tlsConfig *tls.Config

	// connCounter feeds the synthetic connectionId advertised in hello.
	connCounter atomic.Int32

	// serviceID is a stable per-process identifier advertised in the hello
	// reply when a client connects with loadBalanced=true (MongoDB 5.0+), so
	// drivers pin cursors/transactions to this connection. Generated once at
	// startup — designed exactly for an L4 proxy like dbbat.
	serviceID bson.ObjectID

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

// NewServer creates a new MongoDB proxy server.
func NewServer(
	dataStore *store.Store,
	encryptionKey []byte,
	queryStorage config.QueryStorageConfig,
	dumpConfig config.DumpConfig,
	authCache *cache.AuthCache,
	mongoConfig config.MongoConfig,
	logger *slog.Logger,
) (*Server, error) {
	tlsConfig, err := loadTLSConfig(mongoConfig)
	if err != nil {
		return nil, fmt.Errorf("MongoDB proxy TLS setup: %w", err)
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
		serviceID:     bson.NewObjectID(),
		logger:        logger,
		shutdown:      make(chan struct{}),
		ctx:           ctx,
		cancel:        cancel,
	}, nil
}

// Start starts the proxy server on addr.
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
	s.logger.InfoContext(s.ctx, "MongoDB proxy server listening", slog.String("addr", addr))

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
				s.logger.ErrorContext(s.ctx, "MongoDB accept failed", slog.Any("error", err))

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
// started accepting connections yet.
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
			s.logger.ErrorContext(ctx, "failed to close MongoDB listener", slog.Any("error", err))
		}
	}

	done := make(chan struct{})

	go func() {
		s.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		s.logger.InfoContext(ctx, "MongoDB proxy server shutdown complete")

		return nil
	case <-ctx.Done():
		s.logger.WarnContext(ctx, "MongoDB proxy server shutdown timeout")

		return ctx.Err()
	}
}

// handleConnection runs one client session on a goroutine of its own, which is
// why the recover is not optional: a panic anywhere in the session's own leg
// (transport setup, auth, the BSON decode before the pumps split off) would
// otherwise end the process and every other live session with it. The two pumps
// are covered separately, in relay — a recover here does not reach them.
func (s *Server) handleConnection(clientConn net.Conn) {
	defer func() {
		if r := recover(); r != nil {
			s.logger.ErrorContext(s.ctx, "MongoDB session panic",
				slog.Any("panic", r),
				slog.String("stack", string(debug.Stack())),
				slog.Any("remote_addr", clientConn.RemoteAddr()))
		}

		if err := clientConn.Close(); err != nil {
			s.logger.DebugContext(s.ctx, "client close error", slog.Any("error", err))
		}
	}()

	s.logger.DebugContext(s.ctx, "MongoDB connection accepted",
		slog.Any("remote_addr", clientConn.RemoteAddr()))

	session := newSession(clientConn, s)
	if err := session.Run(); err != nil {
		s.logger.InfoContext(s.ctx, "MongoDB session ended",
			slog.Any("remote_addr", clientConn.RemoteAddr()),
			slog.Any("error", err))
	}
}

const dumpCleanupInterval = 1 * time.Hour

// goroutineNameDumpRetention is what a panic in a retention sweep is logged under.
const goroutineNameDumpRetention = "mongodb dump retention sweep"

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
					s.logger.ErrorContext(s.ctx, "MongoDB dump cleanup failed", slog.Any("error", err))
				} else if deleted > 0 {
					s.logger.InfoContext(s.ctx, "MongoDB dump cleanup", slog.Int("deleted", deleted))
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
