package mssql

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"time"
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

// session drives one client connection through the TDS handshake.
//
// Stage 1 stops at the end of the handshake: once LOGIN7 has been parsed and
// validated, the session answers with a well-formed TDS error saying the proxy
// is not wired through, and closes. Stage 2 replaces that last step with the
// upstream connection.
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
}

// newSession prepares a session over an accepted connection.
func newSession(conn net.Conn, server *Server) *session {
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

// Run performs the handshake and then closes the session with the stage-1 stub
// error. The returned error describes why the session ended; a clean stub
// rejection returns nil.
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
		return s.reject(ctx, err)
	}

	// Stage 1 ends here, on purpose.
	if err := s.pkt.WriteMessage(packetTypeReply, buildStubResponse()); err != nil {
		return err
	}

	s.logger.InfoContext(ctx, "MSSQL session closed with the not-wired-through stub",
		slog.Any("remote_addr", s.conn.RemoteAddr()))

	return nil
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

	if err := s.pkt.WriteMessage(packetTypePrelogin, buildPreloginResponse(client, response).serialize()); err != nil {
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
func (s *session) reject(ctx context.Context, reason error) error {
	if err := s.pkt.WriteMessage(packetTypeReply, buildLoginRejected(reason.Error())); err != nil {
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
