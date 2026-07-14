package mongodb

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xdg-go/scram"

	"github.com/fclairamb/dbbat/internal/cache"
	"github.com/fclairamb/dbbat/internal/dump"
	"github.com/fclairamb/dbbat/internal/proxy/shared"
	"github.com/fclairamb/dbbat/internal/store"
)

// tlsHandshakeFirstByte is the first byte of a TLS ClientHello record
// (ContentType handshake = 22). MongoDB TLS is implicit-from-byte-0, so
// peeking this byte distinguishes a TLS client from a plaintext one on a
// single listener (contract §2).
const tlsHandshakeFirstByte = 0x16

// preAuthReplyRequestIDBase seeds the requestIDs the proxy stamps on its own
// pre-auth/auth replies. Client requestIDs are independent (contract §6), so
// the exact value is cosmetic — it just needs to be a valid non-zero counter.
const preAuthReplyRequestIDBase = 1 << 30

// Session is a single client connection through the MongoDB proxy.
type Session struct {
	server *Server

	// clientConn is the client-facing conn: a shared.CountingConn around the
	// raw socket, wrapped in a *tls.Conn when the client upgraded to TLS.
	clientConn net.Conn
	reader     *bufio.Reader
	tlsActive  bool

	// connID is the synthetic connectionId advertised in hello.
	connID int32
	// replyReqID stamps requestIDs on proxy-generated replies.
	replyReqID atomic.Int32

	// Populated as auth progresses.
	user          *store.User
	database      *store.Database
	grant         *store.Grant
	authenticated bool
	// authSource is the SASL authSource the client used; forwarded to the
	// upstream SCRAM exchange as $db (contract §5).
	authSource string

	// SCRAM-SHA-256 client-side auth state (item 1): the in-progress server
	// conversation and the dbbat user resolved by its credential lookup, carried
	// across the saslStart → saslContinue exchange.
	scramConv       *scram.ServerConversation
	scramUser       *store.User
	scramUserDBHint string

	// OP_COMPRESSED negotiation (item 4). clientCompressor is the compressor id
	// the client last used; compressReplies mirrors the client — replies are
	// compressed only once the client has itself sent a compressed frame, which
	// guarantees it will decompress ours (and keeps the first hello reply plain).
	clientCompressor uint8
	compressReplies  bool

	// upstream is the authenticated connection to the target MongoDB.
	upstream *upstreamConn

	// connection is the DBBat audit record (insert on auth, close on teardown).
	connection *store.Connection

	// dumpWriter captures post-auth framed traffic (plaintext) when enabled.
	dumpWriter *dump.Writer
	dumpMu     sync.Mutex

	// Wire-level byte counters for the client-facing socket.
	bytesFromClient   *atomic.Int64
	bytesToClient     *atomic.Int64
	lastBytesSnapshot int64

	// guard enforces the grant's time-window / bandwidth limits mid-stream.
	guard *shared.LimitGuard
	// revocation is signaled when this session's grant is revoked mid-flight.
	revocation *cache.RevocationHandle

	// pending correlates upstream replies to the query that produced them
	// (phase 3). Keyed by the client requestID.
	pendingMu sync.Mutex
	pending   map[int32]*pendingQuery

	// cursorOrigins links a server cursor id back to the find/aggregate that
	// opened it (item 6) so every getMore batch of one result set logs a shared
	// cursor_id + the originating command. Written by the upstream→client pump
	// (on cursor-opening replies) and read by the client→upstream pump (on
	// getMore), hence guarded by its own mutex.
	cursorMu      sync.Mutex
	cursorOrigins map[int64]cursorOrigin

	logger *slog.Logger
	ctx    context.Context //nolint:containedctx // Session-scoped context
}

func newSession(rawConn net.Conn, server *Server) *Session {
	bytesFromClient := &atomic.Int64{}
	bytesToClient := &atomic.Int64{}

	s := &Session{
		server:          server,
		clientConn:      shared.NewCountingConn(rawConn, bytesFromClient, bytesToClient),
		bytesFromClient: bytesFromClient,
		bytesToClient:   bytesToClient,
		connID:          server.connCounter.Add(1),
		pending:         make(map[int32]*pendingQuery),
		cursorOrigins:   make(map[int64]cursorOrigin),
		logger:          server.logger,
		ctx:             server.ctx,
	}
	s.replyReqID.Store(preAuthReplyRequestIDBase)

	return s
}

// Run drives the session lifecycle (contract §2):
//  1. optional TLS termination (peek first byte)
//  2. PRE-AUTH: answer hello/ping/etc — monitoring connections stay here
//     forever, never dialing upstream or recording a connection
//  3. on saslStart: authenticate, resolve grant, dial upstream, record
//  4. relay commands until either side closes
func (s *Session) Run() error {
	defer s.deregisterRevocation()

	if err := s.setupTransport(); err != nil {
		return err
	}

	// PRE-AUTH loop. Returns nil only once a saslStart authenticated and the
	// upstream/connection/guard are set up.
	if err := s.preAuthLoop(); err != nil {
		return err
	}

	defer s.closeUpstream()
	defer s.recordDisconnect()

	s.startDumpIfConfigured()
	defer s.closeDump()

	// Watchdog: tears the session down the moment a time/bandwidth/revocation
	// limit is crossed. Conn refs captured into locals so the goroutine never
	// races the mutable s.upstream field (closeUpstream nils it).
	watchCtx, cancelWatch := context.WithCancel(s.ctx)
	defer cancelWatch()

	upstream := s.upstream
	clientConn := s.clientConn

	go s.guard.Watch(watchCtx, shared.DefaultLimitPollInterval, func(err error) {
		s.onLimitViolation(upstream, clientConn, err)
	})

	s.logger.InfoContext(s.ctx, "MongoDB session ready",
		slog.String("user", s.user.Username),
		slog.String("database", s.database.Name),
		slog.Any("remote_addr", s.clientConn.RemoteAddr()))

	return s.relay()
}

// setupTransport peeks the first client byte to decide between a TLS upgrade
// and plaintext, then builds the buffered reader used for the rest of the
// session (contract §2).
func (s *Session) setupTransport() error {
	var first [1]byte
	if _, err := io.ReadFull(s.clientConn, first[:]); err != nil {
		return fmt.Errorf("peek first byte: %w", err)
	}

	prefixed := &prefixConn{Conn: s.clientConn, prefix: []byte{first[0]}}

	if first[0] == tlsHandshakeFirstByte && s.server.tlsConfig != nil {
		tlsConn := tls.Server(prefixed, s.server.tlsConfig)
		if err := tlsConn.HandshakeContext(s.ctx); err != nil {
			return fmt.Errorf("TLS handshake: %w", err)
		}

		s.clientConn = tlsConn
		s.tlsActive = true
		s.reader = bufio.NewReader(tlsConn)

		return nil
	}

	s.clientConn = prefixed
	s.reader = bufio.NewReader(prefixed)

	return nil
}

// preAuthLoop answers hello/ping/etc until a saslStart authenticates the
// session (returns nil) or the connection ends / errors.
func (s *Session) preAuthLoop() error {
	for {
		m, err := s.readClientMessage()
		if err != nil {
			return fmt.Errorf("pre-auth read: %w", err)
		}

		done, err := s.dispatchPreAuth(m)
		if err != nil {
			return err
		}

		if done {
			return nil
		}
	}
}

// readClientMessage reads one framed message from the client, transparently
// inflating an OP_COMPRESSED envelope into the logical message it carries
// (item 4). Receiving a compressed frame enables reply compression mirroring
// the client's chosen compressor.
func (s *Session) readClientMessage() (*message, error) {
	m, err := readMessage(s.reader)
	if err != nil {
		return nil, err
	}

	if m.opCode != opCodeCompressed {
		return m, nil
	}

	inner, compressorID, err := decompressMessage(m)
	if err != nil {
		return nil, err
	}

	s.clientCompressor = compressorID
	s.compressReplies = true

	return inner, nil
}

// dispatchPreAuth handles one pre-auth message. It returns done=true only when
// a saslStart successfully authenticated (upstream + connection now set up).
func (s *Session) dispatchPreAuth(m *message) (bool, error) {
	switch m.opCode {
	case opCodeQuery:
		return s.dispatchPreAuthOpQuery(m)
	case opCodeMsg:
		return s.dispatchPreAuthOpMsg(m)
	default:
		return false, fmt.Errorf("%w: %d", ErrUnknownOpCode, m.opCode)
	}
}

// dispatchPreAuthOpQuery handles the legacy OP_QUERY handshake (isMaster/hello
// on admin.$cmd), answering with OP_REPLY.
func (s *Session) dispatchPreAuthOpQuery(m *message) (bool, error) {
	q, err := parseOpQuery(m.body)
	if err != nil {
		return false, err
	}

	name := commandName(q.query)
	switch name {
	case "hello", "isMaster", "ismaster":
		reply, err := buildOpReply(s.nextReplyID(), m.requestID, s.helloDoc(name, q.query))
		if err != nil {
			return false, err
		}

		return false, s.writeClient(reply)
	default:
		// Legacy path is only ever used for the first hello; anything else is
		// unexpected. Refuse with Unauthorized.
		return false, s.replyOpReply(m.requestID, unauthorizedDoc(ErrPreAuthNotAllowed.Error()))
	}
}

// dispatchPreAuthOpMsg handles OP_MSG pre-auth commands.
func (s *Session) dispatchPreAuthOpMsg(m *message) (bool, error) {
	parsed, err := parseOpMsg(m.body)
	if err != nil {
		return false, err
	}

	body, ok := parsed.commandBody()
	if !ok {
		return false, ErrNoCommandBody
	}

	name := commandName(body)
	switch name {
	case "hello", "isMaster", "ismaster":
		return false, s.replyOpMsg(m.requestID, s.helloDoc(name, body))
	case "ping":
		return false, s.replyOpMsg(m.requestID, okDoc())
	case "buildInfo", "buildinfo":
		return false, s.replyOpMsg(m.requestID, buildInfoDoc())
	case "endSessions":
		return false, s.replyOpMsg(m.requestID, okDoc())
	case "saslStart":
		return s.handleSaslStart(m.requestID, body)
	case "saslContinue":
		// SCRAM-SHA-256 is multi-step; route to its continue handler when a
		// conversation is in flight. PLAIN is single-step, so an otherwise stray
		// saslContinue is an auth failure.
		if s.scramConv != nil {
			return s.handleScramContinue(m.requestID, body)
		}

		return false, s.failAuth(m.requestID)
	default:
		return false, s.replyOpMsg(m.requestID, unauthorizedDoc(ErrPreAuthNotAllowed.Error()))
	}
}

// relay pumps framed messages both ways after auth until either side ends.
func (s *Session) relay() error {
	errCh := make(chan error, 2)

	go func() { errCh <- s.pumpClientToUpstream() }()
	go func() { errCh <- s.pumpUpstreamToClient() }()

	err := <-errCh

	// Unblock the other pump.
	s.closeUpstream()

	if s.clientConn != nil {
		_ = s.clientConn.Close()
	}

	<-errCh

	if err == nil || errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
		return nil
	}

	return err
}

// pumpUpstreamToClient relays upstream replies to the client. Result capture
// (phase 3) correlates each reply to its originating query via responseTo. A
// listDatabases reply is rewritten to the grant's database before relay
// (item 5) so no cluster-wide database list leaks.
func (s *Session) pumpUpstreamToClient() error {
	for {
		m, err := readMessage(s.upstream.reader)
		if err != nil {
			return err
		}

		outRaw := m.raw
		if s.pendingCommand(m.responseTo) == "listDatabases" {
			if rewritten, ok := s.filterListDatabasesReply(m); ok {
				outRaw = rewritten
			}
		}

		s.captureResult(m)
		s.dumpPacket(dump.DirServerToClient, outRaw)

		if err := s.writeClient(outRaw); err != nil {
			return err
		}
	}
}

// writeClient writes a framed message to the client, compressing it into an
// OP_COMPRESSED envelope when the client has enabled compression (item 4).
func (s *Session) writeClient(raw []byte) error {
	out, err := s.maybeCompress(raw)
	if err != nil {
		return err
	}

	_, err = s.clientConn.Write(out)

	return err
}

// maybeCompress wraps an OP_MSG / OP_REPLY frame in OP_COMPRESSED when reply
// compression is active with a real (non-noop) compressor; other frames and the
// pre-negotiation phase pass through untouched.
func (s *Session) maybeCompress(raw []byte) ([]byte, error) {
	if !s.compressReplies || s.clientCompressor == compressorNoop || len(raw) < headerLen {
		return raw, nil
	}

	opCode := int32(binary.LittleEndian.Uint32(raw[12:16]))
	if opCode != opCodeMsg && opCode != opCodeReply {
		return raw, nil
	}

	return compressMessage(raw, s.clientCompressor)
}

// nextReplyID returns the next requestID for a proxy-generated reply.
func (s *Session) nextReplyID() int32 {
	return s.replyReqID.Add(1)
}

// replyOpMsg sends an OP_MSG kind-0 reply answering responseTo.
func (s *Session) replyOpMsg(responseTo int32, doc any) error {
	reply, err := buildOpMsgReply(s.nextReplyID(), responseTo, doc)
	if err != nil {
		return err
	}

	return s.writeClient(reply)
}

// replyOpReply sends a legacy OP_REPLY answering responseTo.
func (s *Session) replyOpReply(responseTo int32, doc any) error {
	reply, err := buildOpReply(s.nextReplyID(), responseTo, doc)
	if err != nil {
		return err
	}

	return s.writeClient(reply)
}

// cumulativeClientBytes returns the running total of client-side bytes.
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

// onLimitViolation force-closes both conns when the watchdog trips.
func (s *Session) onLimitViolation(upstream *upstreamConn, clientConn io.Closer, err error) {
	s.logger.WarnContext(s.ctx, "terminating MongoDB session: grant no longer valid mid-stream",
		slog.Any("error", err))

	if upstream != nil {
		upstream.close()
	}

	if clientConn != nil {
		_ = clientConn.Close()
	}
}

// recordConnection inserts the DBBat audit record (after auth).
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

// deregisterRevocation drops this session's revocation handle.
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

	// Flush any client-side bytes not yet attributed to a query.
	total := s.cumulativeClientBytes()
	if delta := total - s.lastBytesSnapshot; delta > 0 {
		s.lastBytesSnapshot = total

		if err := s.server.store.IncrementConnectionBytes(s.ctx, s.connection.UID, delta); err != nil {
			s.logger.DebugContext(s.ctx, "MongoDB trailing byte flush failed",
				slog.Any("connection_id", s.connection.UID),
				slog.Any("error", err))
		}
	}

	if err := s.server.store.CloseConnection(s.ctx, s.connection.UID); err != nil {
		s.logger.WarnContext(s.ctx, "MongoDB connection close failed",
			slog.Any("connection_id", s.connection.UID),
			slog.Any("error", err))
	}
}

// startDumpIfConfigured opens a packet-dump file capturing the post-auth
// (plaintext, TLS-terminated) framed traffic.
func (s *Session) startDumpIfConfigured() {
	if s.server.dumpConfig.Dir == "" || s.connection == nil {
		return
	}

	dumpPath := filepath.Join(s.server.dumpConfig.Dir, s.connection.UID.String()+dump.FileExt)

	dw, err := dump.NewWriter(dumpPath, dump.Header{
		SessionID: s.connection.UID.String(),
		Protocol:  dump.ProtocolMongo,
		StartTime: time.Now(),
		Connection: map[string]any{
			"database":      s.database.DatabaseName,
			"user":          s.user.Username,
			"upstream_addr": net.JoinHostPort(s.database.Host, fmt.Sprintf("%d", s.database.Port)),
			"protocol":      s.database.Protocol,
		},
	}, s.server.dumpConfig.MaxSize)
	if err != nil {
		s.logger.WarnContext(s.ctx, "MongoDB dump writer create failed", slog.Any("error", err))

		return
	}

	s.dumpWriter = dw
}

// dumpPacket records a framed message to the dump writer, if enabled.
func (s *Session) dumpPacket(direction byte, raw []byte) {
	s.dumpMu.Lock()
	defer s.dumpMu.Unlock()

	if s.dumpWriter == nil {
		return
	}

	_ = s.dumpWriter.WritePacket(direction, raw)
}

func (s *Session) closeDump() {
	s.dumpMu.Lock()
	defer s.dumpMu.Unlock()

	if s.dumpWriter == nil {
		return
	}

	if err := s.dumpWriter.Close(); err != nil {
		s.logger.WarnContext(s.ctx, "MongoDB dump writer close failed", slog.Any("error", err))
	}

	s.dumpWriter = nil
}

// closeUpstream closes the upstream connection if open.
func (s *Session) closeUpstream() {
	if s.upstream == nil {
		return
	}

	s.upstream.close()
	s.upstream = nil
}

// prefixConn returns a set of pre-read bytes before delegating to the wrapped
// conn. Used to "un-peek" the first byte after TLS detection.
type prefixConn struct {
	net.Conn
	prefix []byte
}

func (p *prefixConn) Read(b []byte) (int, error) {
	if len(p.prefix) > 0 {
		n := copy(b, p.prefix)
		p.prefix = p.prefix[n:]

		return n, nil
	}

	return p.Conn.Read(b)
}
