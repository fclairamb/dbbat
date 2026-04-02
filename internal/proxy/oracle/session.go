package oracle

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"

	"github.com/google/uuid"

	"github.com/fclairamb/dbbat/internal/cache"
	"github.com/fclairamb/dbbat/internal/store"
)

// session represents a single Oracle proxy session.
type session struct {
	clientConn    net.Conn
	upstreamConn  net.Conn
	store         *store.Store
	encryptionKey []byte
	logger        *slog.Logger
	ctx           context.Context //nolint:containedctx
	authCache     *cache.AuthCache

	// Connection metadata
	serviceName   string
	username      string
	database      *store.Database
	user          *store.User
	grant         *store.Grant
	connectionUID uuid.UUID
}

// newSession creates a new Oracle proxy session.
func newSession(
	clientConn net.Conn,
	dataStore *store.Store,
	encryptionKey []byte,
	logger *slog.Logger,
	ctx context.Context, //nolint:revive
	authCache *cache.AuthCache,
) *session {
	return &session{
		clientConn:    clientConn,
		store:         dataStore,
		encryptionKey: encryptionKey,
		logger:        logger,
		ctx:           ctx,
		authCache:     authCache,
	}
}

// run executes the full session lifecycle.
func (s *session) run() error {
	defer s.cleanup()

	// Step 1: Receive TNS Connect from client
	connectPkt, err := readTNSPacket(s.clientConn)
	if err != nil {
		return fmt.Errorf("failed to read connect packet: %w", err)
	}

	if connectPkt.Type != TNSPacketTypeConnect {
		s.sendRefuse("expected TNS Connect packet")

		return fmt.Errorf("expected Connect packet, got %s", connectPkt.Type)
	}

	// Step 2: Parse connect descriptor to find target database
	connectStr := extractConnectString(connectPkt.Payload)
	cd := parseConnectDescriptor(connectStr)
	s.serviceName = cd.ServiceName

	// Also try EZ Connect if no service name found
	if s.serviceName == "" {
		s.serviceName = parseServiceNameEZConnect(connectStr)
	}

	// Try SID as fallback
	if s.serviceName == "" {
		s.serviceName = cd.SID
	}

	if s.serviceName == "" {
		s.sendRefuse("missing SERVICE_NAME in connect descriptor")

		return fmt.Errorf("no SERVICE_NAME in connect descriptor")
	}

	s.logger = s.logger.With("service_name", s.serviceName)

	// Step 3: Look up database in store
	db, err := s.store.GetDatabaseByName(s.ctx, s.serviceName)
	if err != nil {
		s.sendRefuse("database not found")

		return fmt.Errorf("%w: %s: %w", ErrDatabaseNotFound, s.serviceName, err)
	}

	s.database = db

	// Step 4: Connect to upstream Oracle
	upstreamAddr := net.JoinHostPort(db.Host, fmt.Sprintf("%d", db.Port))

	s.upstreamConn, err = net.Dial("tcp", upstreamAddr)
	if err != nil {
		s.sendRefuse("cannot reach upstream database")

		return fmt.Errorf("failed to connect to upstream %s: %w", upstreamAddr, err)
	}

	// Step 5: Forward the original Connect packet to upstream
	if err := writeTNSPacket(s.upstreamConn, connectPkt); err != nil {
		s.sendRefuse("upstream connection failed")

		return fmt.Errorf("failed to forward connect to upstream: %w", err)
	}

	// Step 6: Read upstream response (Accept, Refuse, or Redirect)
	upstreamResp, err := readTNSPacket(s.upstreamConn)
	if err != nil {
		s.sendRefuse("upstream did not respond")

		return fmt.Errorf("failed to read upstream response: %w", err)
	}

	if upstreamResp.Type == TNSPacketTypeRefuse {
		// Forward the refusal to client
		_ = writeTNSPacket(s.clientConn, upstreamResp)

		return fmt.Errorf("upstream refused connection")
	}

	// Forward the Accept to client
	if err := writeTNSPacket(s.clientConn, upstreamResp); err != nil {
		return fmt.Errorf("failed to forward accept to client: %w", err)
	}

	// Step 7: Relay TTC negotiation + AUTH with grant checking
	if err := s.handleAuthPhase(); err != nil {
		return fmt.Errorf("auth phase failed: %w", err)
	}

	// Step 8: Create connection record
	sourceIP := store.ExtractSourceIP(s.clientConn.RemoteAddr())

	conn, err := s.store.CreateConnection(s.ctx, s.user.UID, s.database.UID, sourceIP)
	if err != nil {
		s.logger.ErrorContext(s.ctx, "failed to create connection record", slog.Any("error", err))
	} else {
		s.connectionUID = conn.UID
	}

	s.logger = s.logger.With("connection_uid", s.connectionUID, "username", s.username)
	s.logger.InfoContext(s.ctx, "Oracle session established")

	// Step 9: Enter bidirectional raw relay
	return s.proxyMessages()
}

// proxyMessages relays TNS packets bidirectionally between client and upstream.
func (s *session) proxyMessages() error {
	errChan := make(chan error, 2)

	// Client → Upstream
	go func() {
		errChan <- s.relay(s.clientConn, s.upstreamConn, "client->upstream")
	}()

	// Upstream → Client
	go func() {
		errChan <- s.relay(s.upstreamConn, s.clientConn, "upstream->client")
	}()

	// Wait for either direction to close
	return <-errChan
}

// relay copies raw bytes from src to dst. This is a simple TCP relay for Phase 1.
func (s *session) relay(src, dst net.Conn, direction string) error {
	buf := make([]byte, 32*1024)

	for {
		n, err := src.Read(buf)
		if n > 0 {
			if _, writeErr := dst.Write(buf[:n]); writeErr != nil {
				return fmt.Errorf("%s write error: %w", direction, writeErr)
			}
		}

		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}

			return fmt.Errorf("%s read error: %w", direction, err)
		}
	}
}

// sendRefuse sends a TNS Refuse packet to the client.
func (s *session) sendRefuse(reason string) {
	// TNS Refuse payload: just the reason string for now
	// Real Oracle Refuse has a more structured format, but clients handle raw text too
	pkt := &TNSPacket{
		Type:    TNSPacketTypeRefuse,
		Payload: []byte(reason),
	}

	if err := writeTNSPacket(s.clientConn, pkt); err != nil {
		s.logger.ErrorContext(s.ctx, "failed to send refuse", slog.Any("error", err))
	}
}

// cleanup closes upstream connection and updates records.
func (s *session) cleanup() {
	if s.connectionUID != uuid.Nil {
		if err := s.store.CloseConnection(s.ctx, s.connectionUID); err != nil {
			s.logger.ErrorContext(s.ctx, "failed to close connection record", slog.Any("error", err))
		}
	}

	if s.upstreamConn != nil {
		if err := s.upstreamConn.Close(); err != nil {
			s.logger.ErrorContext(s.ctx, "failed to close upstream connection", slog.Any("error", err))
		}
	}
}
