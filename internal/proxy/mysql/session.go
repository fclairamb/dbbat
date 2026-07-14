package mysql

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"sync/atomic"
	"time"

	gomysqlclient "github.com/go-mysql-org/go-mysql/client"
	gomysqlserver "github.com/go-mysql-org/go-mysql/server"

	"github.com/fclairamb/dbbat/internal/cache"
	"github.com/fclairamb/dbbat/internal/dump"
	"github.com/fclairamb/dbbat/internal/proxy/shared"
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

	// Upstream MySQL connection.
	upstreamConn *gomysqlclient.Conn

	// DBBat connection record (insert on connect, close on disconnect).
	connection *store.Connection

	// Optional packet dump for the post-auth phase (matches PG behavior).
	dumpWriter *dump.Writer

	logger *slog.Logger
	ctx    context.Context //nolint:containedctx // Session-scoped context

	// Wire-level byte counters for the client-facing socket. Together they
	// capture every byte the proxy exchanged with the client (handshake,
	// COM_QUERY/COM_STMT_EXECUTE requests, result-set responses, errors).
	bytesFromClient *atomic.Int64
	bytesToClient   *atomic.Int64
	// lastBytesSnapshot is the cumulative client-side byte count at the end
	// of the previous recorded query. The first query absorbs the
	// handshake/auth bytes, which is the right place for them.
	lastBytesSnapshot int64

	// guard enforces the grant's time-window and bandwidth limits. The
	// go-mysql library owns the wire and buffers whole results, so mid-stream
	// enforcement runs entirely through the watchdog (onLimitViolation), which
	// force-closes both conns.
	guard *shared.LimitGuard

	// revocation is signalled when this session's grant is revoked mid-flight,
	// so the next command is rejected and the watchdog tears the session down.
	revocation *cache.RevocationHandle
}

// cumulativeClientBytes returns the running total of bytes exchanged with
// the client. Nil-safe so tests that build sessions without going through
// newSession don't panic.
func (s *Session) cumulativeClientBytes() int64 {
	var total int64
	if s.bytesFromClient != nil {
		total += s.bytesFromClient.Load()
	}
	if s.bytesToClient != nil {
		total += s.bytesToClient.Load()
	}
	return total
}

func newSession(clientConn net.Conn, server *Server) *Session {
	bytesFromClient := &atomic.Int64{}
	bytesToClient := &atomic.Int64{}

	return &Session{
		server:          server,
		clientConn:      shared.NewCountingConn(clientConn, bytesFromClient, bytesToClient),
		bytesFromClient: bytesFromClient,
		bytesToClient:   bytesToClient,
		logger:          server.logger,
		ctx:             server.ctx,
	}
}

// Run drives the session lifecycle:
//  1. MySQL handshake with auth termination
//  2. Connect to upstream MySQL using stored credentials
//  3. Record the connection in the DBBat store
//  4. Loop dispatching client commands via the Handler interface
//  5. Mark the connection closed on shutdown
func (s *Session) Run() error {
	authHandler := &dbbatAuthHandler{session: s}
	commandHandler := &handler{session: s}

	conn, err := s.server.gomysqlServer.NewCustomizedConn(s.clientConn, authHandler, commandHandler)
	if err != nil {
		return fmt.Errorf("MySQL handshake: %w", err)
	}

	s.serverConn = conn

	// Auth succeeded, so the session is registered in the revocation registry
	// (see OnAuth). Deregister on the way out regardless of how the session
	// ends — deferred here, before any early return below, so no handle leaks.
	defer s.deregisterRevocation()

	if err := s.connectUpstream(); err != nil {
		return err
	}

	defer s.closeUpstream()

	if err := s.recordConnection(); err != nil {
		// Don't fail the session if the audit insert fails — log and continue.
		s.logger.WarnContext(s.ctx, "MySQL connection insert failed", slog.Any("error", err))
	}

	defer s.recordDisconnect()

	s.startDumpIfConfigured()
	defer s.closeDump()

	// Run a watchdog that tears the session down the moment a time/bandwidth
	// limit is crossed. The go-mysql library owns the wire and buffers whole
	// results, so there is no message boundary at which to inject a clean error
	// frame — force-closing the conns (in onLimitViolation) is the enforcement
	// mechanism. Canceled when the command loop exits so no goroutine leaks.
	//
	// The conn references are captured into locals so the watchdog goroutine
	// never reads the mutable s.upstreamConn field (closeUpstream nils it),
	// keeping the teardown race-free.
	watchCtx, cancelWatch := context.WithCancel(s.ctx)
	defer cancelWatch()

	upstreamConn := s.upstreamConn
	clientConn := s.clientConn

	go s.guard.Watch(watchCtx, shared.DefaultLimitPollInterval, func(err error) {
		s.onLimitViolation(upstreamConn, clientConn, err)
	})

	s.logger.InfoContext(s.ctx, "MySQL session ready",
		slog.String("user", s.user.Username),
		slog.String("database", s.database.Name),
		slog.Any("remote_addr", s.clientConn.RemoteAddr()))

	return s.commandLoop()
}

// onLimitViolation is invoked by the limit watchdog when a time/bandwidth limit
// is crossed mid-query. It force-closes both conns: closing the upstream
// unblocks a long-running Execute (the go-mysql client aborts its result read
// and returns an error, surfaced to the client as an ERR packet); closing the
// client ends the command loop. The go-mysql library owns the wire and buffers
// whole results, so there is no message boundary at which to inject a clean
// error frame — force-closing the conns is the enforcement mechanism, mirroring
// the PostgreSQL/Oracle onLimitViolation methods.
//
// The conns are passed in (captured as locals when the watchdog starts) rather
// than read from s.upstreamConn, which closeUpstream nils; this keeps the
// teardown race-free. Close is safe to call concurrently with a blocked
// Read/Write and safe to call twice (the deferred closeUpstream closes the same
// conn).
func (s *Session) onLimitViolation(upstreamConn, clientConn io.Closer, err error) {
	s.logger.WarnContext(s.ctx, "terminating MySQL session: grant no longer valid mid-stream",
		slog.Any("error", err))

	if upstreamConn != nil {
		_ = upstreamConn.Close()
	}

	if clientConn != nil {
		_ = clientConn.Close()
	}
}

// commandLoop dispatches client commands until the connection closes.
// HandleCommand returns nil only for COM_QUIT and io.EOF; all other terminal
// errors are returned and surface to the caller.
func (s *Session) commandLoop() error {
	for {
		if err := s.serverConn.HandleCommand(); err != nil {
			if errors.Is(err, io.EOF) || s.serverConn.Closed() {
				return nil
			}

			return fmt.Errorf("MySQL command: %w", err)
		}

		if s.serverConn.Closed() {
			return nil
		}
	}
}

func (s *Session) recordConnection() error {
	conn, err := s.server.store.CreateConnection(
		s.ctx,
		s.user.UID,
		s.database.UID,
		store.ExtractSourceIP(s.clientConn.RemoteAddr()),
	)
	if err != nil {
		return fmt.Errorf("create connection: %w", err)
	}

	s.connection = conn

	return nil
}

// deregisterRevocation drops this session's handle from the store's revocation
// registry. Safe to call when the session never registered (grant nil).
func (s *Session) deregisterRevocation() {
	if s.grant == nil || s.revocation == nil {
		return
	}

	s.server.store.Revocations().Deregister(s.grant.UID, s.revocation)
}

func (s *Session) recordDisconnect() {
	if s.connection == nil {
		return
	}

	if err := s.server.store.CloseConnection(s.ctx, s.connection.UID); err != nil {
		s.logger.WarnContext(s.ctx, "MySQL connection close failed",
			slog.Any("connection_id", s.connection.UID),
			slog.Any("error", err))
	}

	// Note: the very last query's response bytes are written by the
	// gomysql server after recordQuery has run, so they aren't picked up
	// by the per-query diff. For typical sessions (many queries, modest
	// per-query response) this is a small undercount. A trailing flush
	// would need a bytes-only Increment helper since IncrementConnectionStats
	// also bumps the query count — out of scope for this fix.
}

// startDumpIfConfigured opens a packet-dump file for this session and tees the
// underlying client connection through it. We only capture the post-auth phase
// (matching the PG proxy) — the dump is wired up after recordConnection has
// run, so the auth handshake is already done by the time bytes start flowing
// through the tap.
//
// For TLS-upgraded connections the library has already replaced
// c.Conn.Conn with a *tls.Conn before this point; the tap will see TLS
// records (encrypted application data), which is fine — the dump captures
// timing and packet boundaries even when payload is opaque.
func (s *Session) startDumpIfConfigured() {
	if s.server.dumpConfig.Dir == "" || s.connection == nil || s.serverConn == nil {
		return
	}

	dumpPath := filepath.Join(s.server.dumpConfig.Dir, s.connection.UID.String()+dump.FileExt)

	dw, err := dump.NewWriter(dumpPath, dump.Header{
		SessionID: s.connection.UID.String(),
		Protocol:  dump.ProtocolMySQL,
		StartTime: time.Now(),
		Connection: map[string]any{
			"database":      s.database.DatabaseName,
			"user":          s.user.Username,
			"upstream_addr": net.JoinHostPort(s.database.Host, fmt.Sprintf("%d", s.database.Port)),
			"protocol":      s.database.Protocol,
		},
	}, s.server.dumpConfig.MaxSize)
	if err != nil {
		s.logger.WarnContext(s.ctx, "MySQL dump writer create failed", slog.Any("error", err))

		return
	}

	s.dumpWriter = dw

	// Replace the underlying net.Conn on the live packet.Conn with a tap.
	// Subsequent reads/writes from go-mysql go through the tap; the buffered
	// reader inside packet.Conn ultimately calls the embedded conn's Read,
	// which now hits our tap.
	if s.serverConn.Conn != nil {
		s.serverConn.Conn.Conn = dump.NewTapConn(
			s.serverConn.Conn.Conn,
			dw,
			dump.DirClientToServer,
			dump.DirServerToClient,
		)
	}
}

func (s *Session) closeDump() {
	if s.dumpWriter == nil {
		return
	}

	if err := s.dumpWriter.Close(); err != nil {
		s.logger.WarnContext(s.ctx, "MySQL dump writer close failed", slog.Any("error", err))
	}

	s.dumpWriter = nil
}
