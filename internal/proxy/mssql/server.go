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

	"github.com/fclairamb/dbbat/internal/config"
)

// Server is the SQL Server (TDS) proxy.
//
// Stage 1 of the proxy speaks the handshake and nothing else: it accepts a
// client, negotiates PRELOGIN (including the TLS handshake encapsulated inside
// PRELOGIN packets), parses LOGIN7, and closes the session with a TDS error
// explaining that nothing is wired through yet.
//
// It therefore takes none of the store / auth-cache / dump / approval
// dependencies its siblings do: there is nothing yet to authenticate against,
// log, or capture. Stage 2 widens the constructor when the upstream leg lands.
type Server struct {
	// tlsConfig supports client-facing TLS termination. Nil when TLS is
	// explicitly disabled in config, in which case the proxy answers
	// ENCRYPT_NOT_SUP.
	tlsConfig *tls.Config

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
func NewServer(mssqlConfig config.MSSQLConfig, logger *slog.Logger) (*Server, error) {
	tlsConfig, err := loadTLSConfig(mssqlConfig)
	if err != nil {
		return nil, fmt.Errorf("SQL Server proxy TLS setup: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	return &Server{
		tlsConfig: tlsConfig,
		logger:    logger,
		shutdown:  make(chan struct{}),
		ctx:       ctx,
		cancel:    cancel,
	}, nil
}

// Start starts the proxy server on addr.
func (s *Server) Start(addr string) error {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("failed to listen: %w", err)
	}

	s.setListener(listener)
	s.logger.InfoContext(s.ctx, "SQL Server proxy listening", slog.String("addr", addr))

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

// nextSPID hands out the per-connection session id echoed in packet headers.
// It wraps at 16 bits and skips 0, which clients read as "no session".
func (s *Server) nextSPID() uint16 {
	//nolint:gosec // deliberately truncated to the 16 bits TDS gives the field
	spid := uint16(s.spidCounter.Add(1))
	if spid == 0 {
		//nolint:gosec // same truncation, one step past the wrap
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
