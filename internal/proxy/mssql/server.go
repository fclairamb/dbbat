package mssql

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fclairamb/dbbat/internal/cache"
	"github.com/fclairamb/dbbat/internal/config"
	"github.com/fclairamb/dbbat/internal/dump"
	"github.com/fclairamb/dbbat/internal/store"
)

// Server is the SQL Server (TDS) proxy.
//
// It accepts a client, negotiates PRELOGIN (including the TLS handshake
// encapsulated inside PRELOGIN packets), parses LOGIN7, authenticates the login
// against dbbat's own users and API keys, opens the upstream with the stored
// credentials, and relays the session.
//
// It still takes fewer dependencies than its siblings: query interception,
// grant enforcement per statement, approval holds and result accounting are
// stage 3, so there is no approval-gate, row-writer or query-storage
// configuration here yet.
type Server struct {
	// tlsConfig supports client-facing TLS termination. Nil when TLS is
	// explicitly disabled in config, in which case the proxy answers
	// ENCRYPT_NOT_SUP.
	tlsConfig *tls.Config

	store         *store.Store
	encryptionKey []byte
	authCache     *cache.AuthCache
	dumpConfig    config.DumpConfig
	// dumpUploader ships finished captures from the local spool to blob
	// storage. nil — the default — keeps them on local disk only.
	dumpUploader *dump.Uploader

	logger *slog.Logger

	// spidCounter feeds the synthetic SPID stamped on outbound packets, so a
	// client that displays @@SPID-ish diagnostics sees a stable per-connection
	// number rather than a constant zero.
	spidCounter atomic.Uint32

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

// NewServer creates a new SQL Server proxy.
func NewServer(
	dataStore *store.Store,
	encryptionKey []byte,
	dumpConfig config.DumpConfig,
	authCache *cache.AuthCache,
	mssqlConfig config.MSSQLConfig,
	logger *slog.Logger,
) (*Server, error) {
	tlsConfig, err := loadTLSConfig(mssqlConfig)
	if err != nil {
		return nil, fmt.Errorf("SQL Server proxy TLS setup: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	return &Server{
		tlsConfig:     tlsConfig,
		store:         dataStore,
		encryptionKey: encryptionKey,
		authCache:     authCache,
		dumpConfig:    dumpConfig,
		logger:        logger,
		shutdown:      make(chan struct{}),
		ctx:           ctx,
		cancel:        cancel,
	}, nil
}

// SetDumpUploader installs the process-wide capture uploader, so this proxy's
// finished captures are shipped to blob storage alongside every other one.
// nil (the default) keeps them on local disk.
func (s *Server) SetDumpUploader(uploader *dump.Uploader) {
	s.dumpUploader = uploader
}

// Start starts the proxy server on addr.
func (s *Server) Start(addr string) error {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("failed to listen: %w", err)
	}

	s.setListener(listener)
	s.logger.InfoContext(s.ctx, "SQL Server proxy listening", slog.String("addr", addr))

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
				s.logger.ErrorContext(s.ctx, "MSSQL accept failed", slog.Any("error", err))

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
			s.logger.ErrorContext(ctx, "failed to close MSSQL listener", slog.Any("error", err))
		}
	}

	done := make(chan struct{})

	go func() {
		s.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		s.logger.InfoContext(ctx, "SQL Server proxy shutdown complete")

		return nil
	case <-ctx.Done():
		s.logger.WarnContext(ctx, "SQL Server proxy shutdown timeout")

		return ctx.Err()
	}
}

// dumpCleanupInterval is how often the local capture spool is swept.
const dumpCleanupInterval = 1 * time.Hour

// runDumpCleanup deletes captures older than the configured retention. It
// applies to the local spool only — captures dbbat uploaded are the bucket's
// lifecycle policy to expire.
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
				s.logger.ErrorContext(s.ctx, "MSSQL dump cleanup failed", slog.Any("error", err))
			} else if deleted > 0 {
				s.logger.InfoContext(s.ctx, "MSSQL dump cleanup", slog.Int("deleted", deleted))
			}
		case <-s.shutdown:
			return
		}
	}
}

// nextSPID hands out the per-connection session id echoed in packet headers.
// It wraps at 16 bits and skips 0, which clients read as "no session".
func (s *Server) nextSPID() uint16 {
	spid := uint16(s.spidCounter.Add(1))
	if spid == 0 {
		spid = uint16(s.spidCounter.Add(1))
	}

	return spid
}

func (s *Server) handleConnection(clientConn net.Conn) {
	defer func() {
		if err := clientConn.Close(); err != nil {
			s.logger.DebugContext(s.ctx, "client close error", slog.Any("error", err))
		}
	}()

	s.logger.DebugContext(s.ctx, "MSSQL connection accepted",
		slog.Any("remote_addr", clientConn.RemoteAddr()))

	session := newSession(clientConn, s)

	if err := session.Run(s.ctx); err != nil && !isExpectedDisconnect(err) {
		s.logger.InfoContext(s.ctx, "MSSQL session ended",
			slog.Any("remote_addr", clientConn.RemoteAddr()),
			slog.Any("error", err))
	}
}

// isExpectedDisconnect reports whether an error is just a client hanging up.
// Probes, health checks and load balancers open and drop TDS connections all
// day; logging each one at info level would drown the real failures.
func isExpectedDisconnect(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed)
}
