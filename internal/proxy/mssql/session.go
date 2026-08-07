package mssql

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/fclairamb/dbbat/internal/dump"
	"github.com/fclairamb/dbbat/internal/proxy/shared"
	"github.com/fclairamb/dbbat/internal/proxy/upstream"
	"github.com/fclairamb/dbbat/internal/store"
)

// handshakeTimeout bounds the whole PRELOGIN → TLS → LOGIN7 exchange. A client
// that opens a socket and says nothing must not hold a goroutine forever.
const handshakeTimeout = 30 * time.Second

// Session-level errors.
var (
	// ErrUnexpectedFirstMessage — the connection did not start with PRELOGIN.
	ErrUnexpectedFirstMessage = errors.New("mssql: connection did not start with a PRELOGIN message")
	// ErrExpectedLogin7 — the message after the handshake was not LOGIN7.
	ErrExpectedLogin7 = errors.New("mssql: expected a LOGIN7 message")
	// ErrMARSUnsupported — the client insisted on Multiple Active Result Sets.
	ErrMARSUnsupported = errors.New("mssql: MultipleActiveResultSets is not supported")
)

// session drives one client connection: the TDS handshake, authentication
// against dbbat's own users, the upstream login, and then the relay.
type session struct {
	server *Server
	conn   net.Conn
	logger *slog.Logger

	// stream is what TDS is read from and written to. It starts as the raw
	// socket, becomes the TLS session after an encapsulated handshake, and —
	// for ENCRYPT_OFF — reverts to the raw socket once LOGIN7 has been read.
	stream *revertibleConn

	// pkt frames TDS packets over stream.
	pkt *packetRW

	// encryption records what the PRELOGIN negotiation decided.
	encryption encryptionMode

	// Populated by authenticate.
	user     *store.User
	database *store.Server
	grant    *store.Grant

	// upstream is the authenticated TDS connection to the target.
	upstream *UpstreamConn

	// connection is the dbbat audit record: inserted once the upstream is up,
	// closed on teardown.
	connection *store.Connection

	// dumpWriter captures the post-auth packet stream when captures are
	// enabled. dumpMu guards it, because the two relay pumps tap it from
	// different goroutines.
	dumpWriter *dump.Writer
	dumpMu     sync.Mutex

	// onClientMessage / onServerPacket are the interception seams stage 3
	// fills. Both nil here — see relay.go.
	onClientMessage clientMessageHook
	onServerPacket  serverPacketHook
}

// newSession prepares a session over an accepted connection.
func newSession(conn net.Conn, server *Server) *session {
	_ = shared.EnableClientKeepAlive(conn)

	stream := newRevertibleConn(conn)
	pkt := newPacketRW(stream)
	pkt.setSPID(server.nextSPID())

	return &session{
		server: server,
		conn:   conn,
		logger: server.logger,
		stream: stream,
		pkt:    pkt,
	}
}

// Run drives the session lifecycle:
//  1. PRELOGIN, the encapsulated TLS handshake, LOGIN7
//  2. authenticate the client against dbbat's users / API keys, resolve the
//     target database and its grant
//  3. open the upstream with the stored credentials, replaying the client's
//     own LOGIN7 with those credentials swapped in
//  4. record the connection, start the capture
//  5. hand the upstream's login response to the client and relay
//
// The returned error describes why the session ended. A refusal the client was
// properly told about returns nil: it is an outcome, not a fault.
func (s *session) Run(ctx context.Context) error {
	if err := s.conn.SetDeadline(time.Now().Add(handshakeTimeout)); err != nil {
		return fmt.Errorf("mssql: set handshake deadline: %w", err)
	}

	client, err := s.readPrelogin()
	if err != nil {
		return err
	}

	if err := s.handlePrelogin(ctx, client); err != nil {
		return err
	}

	login, err := s.readLogin7()
	if err != nil {
		return err
	}

	// ENCRYPT_OFF encrypts the login packet and nothing else: now that LOGIN7
	// has been read, both ends drop back to cleartext TDS.
	if s.encryption == encryptionLoginOnly {
		s.stream.revert()
		s.logger.DebugContext(ctx, "MSSQL reverted to cleartext after the login packet",
			slog.Any("remote_addr", s.conn.RemoteAddr()))
	}

	s.pkt.SetPacketSize(int(login.PacketSize))

	s.logger.InfoContext(ctx, "MSSQL handshake completed",
		slog.Any("remote_addr", s.conn.RemoteAddr()),
		slog.String("user", login.UserName),
		slog.String("database", login.Database),
		slog.String("tds_version", tdsVersionName(login.TDSVersion)),
		slog.String("client_library", login.CltIntName),
		slog.String("encryption", s.encryption.String()))

	if err := login.Validate(); err != nil {
		return s.reject(ctx, login.UserName, err)
	}

	if err := s.authenticate(ctx, login); err != nil {
		s.logger.InfoContext(ctx, "MSSQL authentication refused",
			slog.Any("remote_addr", s.conn.RemoteAddr()),
			slog.String("user", login.UserName),
			slog.String("database", login.Database),
			slog.Any("error", err))

		return s.reject(ctx, login.UserName, err)
	}

	if err := s.connectUpstream(ctx, login); err != nil {
		// The upstream's own message is for the operator, not the client: it
		// can name hosts and stored credentials. The client gets the class of
		// failure and nothing else.
		s.logger.WarnContext(ctx, "MSSQL upstream connect failed",
			slog.String("user", s.user.Username),
			slog.String("database", s.database.Name),
			slog.Any("error", err))

		return s.reject(ctx, login.UserName, ErrUpstreamConnect)
	}

	defer s.closeUpstream(ctx)

	return s.serve(ctx)
}

// serve runs everything that only makes sense once both legs are up: the audit
// record, the capture, the forwarded login response, and the relay itself.
func (s *session) serve(ctx context.Context) error {
	if err := s.recordConnection(ctx); err != nil {
		// An audit insert failure must not take the session down — log it and
		// carry on, as the other proxies do.
		s.logger.WarnContext(ctx, "MSSQL connection insert failed", slog.Any("error", err))
	}

	defer s.recordDisconnect(ctx)

	s.startDumpIfConfigured(ctx)
	defer s.closeDump(ctx)

	// The handshake deadline must go before the relay starts: a session is
	// allowed to sit idle for as long as the client keeps it open.
	if err := s.conn.SetDeadline(time.Time{}); err != nil {
		return fmt.Errorf("mssql: clear handshake deadline: %w", err)
	}

	// The client gets the upstream's own login response — LOGINACK, the
	// ENVCHANGEs for database/collation/packet size, any INFO messages — rather
	// than something dbbat synthesised, so it sees the real server it is
	// talking to.
	if err := s.pkt.WriteMessage(packetTypeReply, s.upstream.LoginResponse); err != nil {
		return err
	}

	s.logger.InfoContext(ctx, "MSSQL session ready",
		slog.String("user", s.user.Username),
		slog.String("database", s.database.Name),
		slog.Bool("upstream_tls", s.upstream.TLS),
		slog.Any("remote_addr", s.conn.RemoteAddr()))

	return s.relay(ctx)
}

// connectUpstream opens the upstream session with the stored credentials,
// decrypted in memory through the per-database AAD key. The decrypted password
// never leaves this call chain and is never logged.
//
// The connect itself is ConnectUpstream — the same call the connectivity check
// makes, so the two cannot drift on ssl_mode policy or login shape.
func (s *session) connectUpstream(ctx context.Context, login *Login7) error {
	if err := s.database.DecryptPassword(s.server.encryptionKey); err != nil {
		return fmt.Errorf("decrypt upstream password: %w", err)
	}

	up, err := ConnectUpstream(ctx, s.dialUpstream, upstream.MSSQLConfig{
		Host:     s.database.Host,
		Port:     s.database.Port,
		Username: s.database.Username,
		Password: s.database.Password,
		Database: s.database.DatabaseName,
		AppName:  buildUpstreamAppName(s.user.Username, login.AppName),
		SSLMode:  s.database.SSLMode,
	}, login)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrUpstreamConnect, err)
	}

	s.upstream = up

	s.logger.DebugContext(ctx, "upstream SQL Server connected",
		slog.String("addr", net.JoinHostPort(s.database.Host, strconv.Itoa(s.database.Port))),
		slog.String("user", s.database.Username),
		slog.String("database", s.database.DatabaseName),
		slog.Bool("tls", up.TLS))

	return nil
}

// dialUpstream opens the transport to the target: a direct TCP dial, or a
// tunnel through the SSH bastion chain when the server row's via_uid is set.
func (s *session) dialUpstream(ctx context.Context) (net.Conn, error) {
	return shared.DialUpstream(ctx, s.server.store, s.server.encryptionKey, s.database)
}

// recordConnection inserts the dbbat audit row for this session.
func (s *session) recordConnection(ctx context.Context) error {
	conn, err := s.server.store.CreateConnection(
		ctx,
		s.user.UID,
		s.database.UID,
		store.ExtractSourceIP(s.conn.RemoteAddr()),
		store.WithUpstreamTLS(s.upstream.TLS),
		store.WithGrantUID(s.grant.UID),
	)
	if err != nil {
		return fmt.Errorf("create connection: %w", err)
	}

	s.connection = conn

	return nil
}

// recordDisconnect closes the audit row.
func (s *session) recordDisconnect(ctx context.Context) {
	if s.connection == nil {
		return
	}

	if err := s.server.store.CloseConnection(ctx, s.connection.UID); err != nil {
		s.logger.WarnContext(ctx, "MSSQL connection close failed",
			slog.Any("connection_id", s.connection.UID),
			slog.Any("error", err))
	}
}

// startDumpIfConfigured opens a packet capture for this session and taps the
// client-leg codec into it.
//
// The tap sits on the codec rather than around the socket on purpose: on an
// encrypted client leg a socket-level tap records TLS records and nothing
// legible, whereas the codec sees the TDS packets themselves. Only the post-auth
// stream is captured, matching the other proxies — the handshake is already over
// by the time this runs.
func (s *session) startDumpIfConfigured(ctx context.Context) {
	if s.server.dumpConfig.Dir == "" || s.connection == nil {
		return
	}

	dumpPath := filepath.Join(s.server.dumpConfig.Dir, s.connection.UID.String()+dump.FileExt)

	writer, err := dump.NewWriter(dumpPath, dump.Header{
		SessionID: s.connection.UID.String(),
		Protocol:  dump.ProtocolMSSQL,
		StartTime: time.Now(),
		Connection: map[string]any{
			"database":      s.database.DatabaseName,
			"user":          s.user.Username,
			"upstream_addr": net.JoinHostPort(s.database.Host, strconv.Itoa(s.database.Port)),
			"protocol":      s.database.Protocol,
		},
	}, s.server.dumpConfig.MaxSize)
	if err != nil {
		s.logger.WarnContext(ctx, "MSSQL dump writer create failed", slog.Any("error", err))

		return
	}

	s.dumpWriter = writer

	s.pkt.setTaps(
		func(frame []byte) { s.dumpPacket(dump.DirClientToServer, frame) },
		func(frame []byte) { s.dumpPacket(dump.DirServerToClient, frame) },
	)
}

// dumpPacket records one framed packet. The two relay pumps call it
// concurrently, hence the lock.
func (s *session) dumpPacket(direction byte, frame []byte) {
	s.dumpMu.Lock()
	defer s.dumpMu.Unlock()

	if s.dumpWriter == nil {
		return
	}

	_ = s.dumpWriter.WritePacket(direction, frame)
}

// closeDump finishes the capture and hands it to the uploader.
func (s *session) closeDump(ctx context.Context) {
	s.dumpMu.Lock()
	writer := s.dumpWriter
	s.dumpWriter = nil
	s.dumpMu.Unlock()

	if writer == nil {
		return
	}

	if err := writer.Close(); err != nil {
		s.logger.WarnContext(ctx, "MSSQL dump writer close failed", slog.Any("error", err))
	}

	// The file is complete now; hand it to the uploader. Finish is a no-op on a
	// nil uploader, which is the default when uploads are not configured.
	if s.connection != nil {
		s.server.dumpUploader.Finish(ctx, s.connection.UID)
	}
}

// readPrelogin reads and parses the first message, which must be PRELOGIN.
func (s *session) readPrelogin() (*preloginMessage, error) {
	msgType, payload, err := s.pkt.ReadMessage()
	if err != nil {
		return nil, err
	}

	if msgType != packetTypePrelogin {
		return nil, fmt.Errorf("%w: got %s", ErrUnexpectedFirstMessage, packetTypeName(msgType))
	}

	return parsePrelogin(payload)
}

// handlePrelogin answers the client's PRELOGIN and, when the negotiation calls
// for it, runs the encapsulated TLS handshake.
func (s *session) handlePrelogin(ctx context.Context, client *preloginMessage) error {
	s.logger.DebugContext(ctx, "MSSQL PRELOGIN received",
		slog.Any("remote_addr", s.conn.RemoteAddr()),
		slog.Any("options", client.optionNames()),
		slog.String("instance", client.InstanceName()),
		slog.String("client_encryption", encryptionName(client.Encryption())))

	response, mode, negotiateErr := negotiateEncryption(client.Encryption(), s.server.tlsConfig != nil)
	s.encryption = mode

	// The PRELOGIN *response* is a Tabular Result (0x04) packet, not a PRELOGIN
	// (0x12) one — an asymmetry in MS-TDS that clients enforce strictly
	// (go-mssqldb refuses anything else outright). The TLS handshake packets
	// that follow go back to 0x12 in both directions.
	if err := s.pkt.WriteMessage(packetTypeReply, buildPreloginResponse(client, response).serialize()); err != nil {
		return err
	}

	// The negotiation failure is reported *after* the response, so the client
	// learns why it is being refused instead of seeing the socket drop.
	if negotiateErr != nil {
		return negotiateErr
	}

	if client.MARSRequested() {
		// The response already answered MARS=0x00. Clients that require it
		// give up on their own; saying so here makes the proxy log explain it.
		s.logger.InfoContext(ctx, "MSSQL client requested MARS, which dbbat refuses",
			slog.Any("remote_addr", s.conn.RemoteAddr()))
	}

	if mode == encryptionNone {
		return nil
	}

	return s.performTLSHandshake(ctx)
}

// performTLSHandshake runs the TLS handshake encapsulated in PRELOGIN packets,
// then switches the session's stream to the TLS session.
func (s *session) performTLSHandshake(ctx context.Context) error {
	adapter := newHandshakeConn(s.conn, s.pkt)
	tlsConn := tls.Server(adapter, s.server.tlsConfig)

	if err := tlsConn.HandshakeContext(ctx); err != nil {
		return fmt.Errorf("mssql: encapsulated TLS handshake: %w", err)
	}

	// Order matters: stop encapsulating (flushing whatever the handshake left
	// pending) before any application byte is written, or the first TLS record
	// after the handshake would still be wrapped in a TDS packet.
	if err := adapter.deactivate(); err != nil {
		return err
	}

	s.stream.switchTo(tlsConn)

	s.logger.DebugContext(ctx, "MSSQL TLS handshake completed",
		slog.Any("remote_addr", s.conn.RemoteAddr()),
		slog.String("version", tlsVersionName(tlsConn.ConnectionState().Version)),
		slog.String("mode", s.encryption.String()))

	return nil
}

// readLogin7 reads and parses the LOGIN7 message.
func (s *session) readLogin7() (*Login7, error) {
	msgType, payload, err := s.pkt.ReadMessage()
	if err != nil {
		return nil, err
	}

	if msgType != packetTypeLogin7 {
		return nil, fmt.Errorf("%w: got %s", ErrExpectedLogin7, packetTypeName(msgType))
	}

	return parseLogin7(payload)
}

// reject reports a refusal to the client as a proper login failure, so the
// driver surfaces the reason instead of a closed socket.
//
// What the client is told comes from clientMessageFor, which derives it from
// the class of failure alone — never from the underlying error's text.
func (s *session) reject(ctx context.Context, username string, reason error) error {
	number, message := clientMessageFor(username, reason)

	if err := s.pkt.WriteMessage(packetTypeReply, buildLoginFailure(number, message)); err != nil {
		return err
	}

	s.logger.InfoContext(ctx, "MSSQL login rejected",
		slog.Any("remote_addr", s.conn.RemoteAddr()),
		slog.Any("reason", reason))

	return nil
}

// tlsVersionName renders a negotiated TLS version for logs.
func tlsVersionName(v uint16) string {
	switch v {
	case tls.VersionTLS10:
		return "1.0"
	case tls.VersionTLS11:
		return "1.1"
	case tls.VersionTLS12:
		return "1.2"
	case tls.VersionTLS13:
		return "1.3"
	default:
		return fmt.Sprintf("%#04x", v)
	}
}
