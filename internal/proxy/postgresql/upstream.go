package postgresql

import (
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"

	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/fclairamb/dbbat/internal/proxy/shared"
	"github.com/fclairamb/dbbat/internal/version"
)

// ErrUpstreamReadOnlyMode is returned when the upstream fails to set read-only mode.
var ErrUpstreamReadOnlyMode = errors.New("upstream error setting read-only mode")

// connectUpstream connects to the upstream PostgreSQL server.
func (s *Session) connectUpstream() error {
	// Decrypt database password
	if err := s.database.DecryptPassword(s.encryptionKey); err != nil {
		return fmt.Errorf("failed to decrypt database password: %w", err)
	}

	// Connect to upstream
	addr := net.JoinHostPort(s.database.Host, fmt.Sprintf("%d", s.database.Port))

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return fmt.Errorf("failed to connect to upstream: %w", err)
	}

	// Negotiate TLS with the upstream per ssl_mode (libpq semantics). Must
	// happen before any StartupMessage — Postgres expects the SSLRequest
	// preamble on a fresh connection, not interleaved with protocol traffic.
	upgraded, err := negotiateUpstreamSSL(s.ctx, conn, s.database.Host, s.database.SSLMode)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("upstream SSL negotiation: %w", err)
	}
	conn = upgraded

	s.upstreamConn = conn

	// Create frontend for upstream (we act as client to upstream)
	upstreamFrontend := pgproto3.NewFrontend(conn, conn)

	// Send startup message to upstream
	if err := s.sendStartupMessage(conn); err != nil {
		return err
	}

	// Handle upstream authentication
	return s.handleUpstreamAuth(upstreamFrontend)
}

// sendStartupMessage sends the startup message to upstream.
func (s *Session) sendStartupMessage(conn net.Conn) error {
	startupMsg := &pgproto3.StartupMessage{
		ProtocolVersion: pgproto3.ProtocolVersionNumber,
		Parameters: map[string]string{
			"user":             s.database.Username,
			"database":         s.database.DatabaseName,
			"application_name": buildApplicationName(s.user.Username, s.clientApplicationName),
		},
	}

	buf, err := startupMsg.Encode(nil)
	if err != nil {
		return fmt.Errorf("failed to encode startup message: %w", err)
	}

	if _, err := conn.Write(buf); err != nil {
		return fmt.Errorf("failed to send startup message to upstream: %w", err)
	}

	return nil
}

// handleUpstreamAuth handles the authentication flow with upstream.
func (s *Session) handleUpstreamAuth(upstreamFrontend *pgproto3.Frontend) error {
	for {
		msg, err := upstreamFrontend.Receive()
		if err != nil {
			return fmt.Errorf("failed to receive from upstream: %w", err)
		}

		done, err := s.processUpstreamAuthMessage(msg, upstreamFrontend)
		if err != nil {
			return err
		}

		if done {
			return nil
		}
	}
}

// sendToUpstream sends a message to upstream and flushes.
func sendToUpstream(frontend *pgproto3.Frontend, msg pgproto3.FrontendMessage) error {
	frontend.Send(msg)

	return frontend.Flush()
}

// sendToClient sends a message to client and flushes.
func (s *Session) sendToClient(msg pgproto3.BackendMessage) error {
	s.clientBackend.Send(msg)

	return s.clientBackend.Flush()
}

// processUpstreamAuthMessage processes a single authentication message from upstream.
// Returns true if authentication is complete.
//
//nolint:cyclop // Protocol handling requires a switch with many cases
func (s *Session) processUpstreamAuthMessage(
	msg pgproto3.BackendMessage,
	upstreamFrontend *pgproto3.Frontend,
) (bool, error) {
	switch typedMsg := msg.(type) {
	case *pgproto3.AuthenticationOk:
		// Authentication successful, continue to next message
		return false, nil

	case *pgproto3.AuthenticationCleartextPassword:
		// Send password
		passwordMsg := &pgproto3.PasswordMessage{
			Password: s.database.Password,
		}

		if err := sendToUpstream(upstreamFrontend, passwordMsg); err != nil {
			return false, fmt.Errorf("failed to send password: %w", err)
		}

		return false, nil

	case *pgproto3.AuthenticationMD5Password:
		// Compute MD5 password hash: md5(md5(password + username) + salt)
		passwordHash := computeMD5Password(s.database.Password, s.database.Username, typedMsg.Salt)
		passwordMsg := &pgproto3.PasswordMessage{
			Password: passwordHash,
		}

		if err := sendToUpstream(upstreamFrontend, passwordMsg); err != nil {
			return false, fmt.Errorf("failed to send MD5 password: %w", err)
		}

		return false, nil

	case *pgproto3.AuthenticationSASL:
		return false, s.beginUpstreamSCRAM(typedMsg, upstreamFrontend)

	case *pgproto3.AuthenticationSASLContinue:
		return false, s.continueUpstreamSCRAM(typedMsg, upstreamFrontend)

	case *pgproto3.AuthenticationSASLFinal:
		return false, s.finalizeUpstreamSCRAM(typedMsg)

	case *pgproto3.ParameterStatus:
		// Buffer ParameterStatus messages - they must come after AuthenticationOk
		// Make a copy since pgproto3 reuses the same struct
		s.logger.DebugContext(s.ctx, "received ParameterStatus from upstream", slog.String("name", typedMsg.Name), slog.String("value", typedMsg.Value))
		paramCopy := &pgproto3.ParameterStatus{
			Name:  typedMsg.Name,
			Value: typedMsg.Value,
		}
		s.bufferedParamStatus = append(s.bufferedParamStatus, paramCopy)

		return false, nil

	case *pgproto3.BackendKeyData:
		// Buffer BackendKeyData - must be sent after ParameterStatus messages
		// Make a copy since pgproto3 reuses the same struct
		s.bufferedBackendKeyData = &pgproto3.BackendKeyData{
			ProcessID: typedMsg.ProcessID,
			SecretKey: typedMsg.SecretKey,
		}

		return false, nil

	case *pgproto3.ReadyForQuery:
		// Upstream is ready, save the frontend for later use
		s.upstreamFrontend = upstreamFrontend

		// Enforce read-only mode at the database level if grant has read_only control
		if s.grant.IsReadOnly() {
			if err := s.setSessionReadOnly(); err != nil {
				return false, fmt.Errorf("failed to set read-only mode: %w", err)
			}
		}

		// Send authentication success to client
		if err := s.sendToClient(&pgproto3.AuthenticationOk{}); err != nil {
			return false, fmt.Errorf("failed to send auth ok: %w", err)
		}

		// Forward buffered ParameterStatus messages
		s.logger.DebugContext(s.ctx, "forwarding ParameterStatus messages to client", slog.Int("count", len(s.bufferedParamStatus)))
		for _, ps := range s.bufferedParamStatus {
			s.logger.DebugContext(s.ctx, "forwarding ParameterStatus", slog.String("name", ps.Name), slog.String("value", ps.Value))
			if err := s.sendToClient(ps); err != nil {
				return false, fmt.Errorf("failed to forward parameter status: %w", err)
			}
		}

		s.bufferedParamStatus = nil // Clear buffer

		// Forward BackendKeyData (required by JDBC and other clients)
		if s.bufferedBackendKeyData != nil {
			if err := s.sendToClient(s.bufferedBackendKeyData); err != nil {
				return false, fmt.Errorf("failed to forward backend key data: %w", err)
			}

			s.bufferedBackendKeyData = nil
		}

		// Forward ready message
		if err := s.sendToClient(typedMsg); err != nil {
			return false, fmt.Errorf("failed to forward ready message: %w", err)
		}

		return true, nil

	case *pgproto3.ErrorResponse:
		// Forward error to client
		if err := s.sendToClient(typedMsg); err != nil {
			s.logger.ErrorContext(s.ctx, "failed to forward error to client", slog.Any("error", err))
		}

		return false, fmt.Errorf("%w: %s", ErrUpstreamAuthFailed, typedMsg.Message)

	default:
		s.logger.WarnContext(s.ctx, "unexpected message during upstream auth", slog.String("type", fmt.Sprintf("%T", typedMsg)))

		return false, nil
	}
}

// beginUpstreamSCRAM handles the AuthenticationSASL message: it picks a
// supported mechanism, builds the client first message, and sends a
// SASLInitialResponse upstream. The SCRAM state is parked on the session
// until the matching Continue/Final messages arrive.
func (s *Session) beginUpstreamSCRAM(
	msg *pgproto3.AuthenticationSASL,
	upstreamFrontend *pgproto3.Frontend,
) error {
	mech := pickSCRAMMechanism(msg.AuthMechanisms)
	if mech == "" {
		return fmt.Errorf("%w: offered=%v", ErrSCRAMNoSupportedMechanism, msg.AuthMechanisms)
	}

	client, err := newSCRAMClient(s.database.Password)
	if err != nil {
		return fmt.Errorf("scram client: %w", err)
	}
	s.upstreamSCRAM = client

	resp := &pgproto3.SASLInitialResponse{
		AuthMechanism: mech,
		Data:          client.firstMessage(),
	}
	if err := sendToUpstream(upstreamFrontend, resp); err != nil {
		return fmt.Errorf("send SASLInitialResponse: %w", err)
	}
	return nil
}

// continueUpstreamSCRAM handles the AuthenticationSASLContinue (server first
// message): compute the client proof and send the SASLResponse.
func (s *Session) continueUpstreamSCRAM(
	msg *pgproto3.AuthenticationSASLContinue,
	upstreamFrontend *pgproto3.Frontend,
) error {
	if s.upstreamSCRAM == nil {
		return fmt.Errorf("%w: SASLContinue without SASL", ErrSCRAMUnexpectedMessage)
	}
	final, err := s.upstreamSCRAM.finalMessage(msg.Data)
	if err != nil {
		return err
	}
	if err := sendToUpstream(upstreamFrontend, &pgproto3.SASLResponse{Data: final}); err != nil {
		return fmt.Errorf("send SASLResponse: %w", err)
	}
	return nil
}

// finalizeUpstreamSCRAM handles AuthenticationSASLFinal: verify the server's
// signature so we know the upstream actually possesses the password's
// SaltedPassword, then wait for the upstream's AuthenticationOk on the next
// loop iteration.
func (s *Session) finalizeUpstreamSCRAM(msg *pgproto3.AuthenticationSASLFinal) error {
	if s.upstreamSCRAM == nil {
		return fmt.Errorf("%w: SASLFinal without SASL", ErrSCRAMUnexpectedMessage)
	}
	if err := s.upstreamSCRAM.verifyServerFinal(msg.Data); err != nil {
		return err
	}
	s.upstreamSCRAM = nil
	return nil
}

// computeMD5Password computes the PostgreSQL MD5 password hash.
// The hash is: "md5" + md5(md5(password + username) + salt)
func computeMD5Password(password, username string, salt [4]byte) string {
	// First hash: md5(password + username)
	h1 := md5.New()
	h1.Write([]byte(password))
	h1.Write([]byte(username))
	sum1 := hex.EncodeToString(h1.Sum(nil))

	// Second hash: md5(first_hash + salt)
	h2 := md5.New()
	h2.Write([]byte(sum1))
	h2.Write(salt[:])

	return "md5" + hex.EncodeToString(h2.Sum(nil))
}

// maxAppNameLen is the maximum length for PostgreSQL application_name (NAMEDATALEN - 1).
const maxAppNameLen = 63

// buildApplicationName constructs the application_name for upstream
// connections: "dbbat/$version @$username", plus " for $appName" when the
// client declared an application_name of its own. See
// shared.BuildUpstreamName for the truncation rules.
func buildApplicationName(username, clientAppName string) string {
	return shared.BuildUpstreamName(version.Version, username, clientAppName, maxAppNameLen)
}

// setSessionReadOnly sets the upstream session to read-only mode.
// This enforces read-only access at the PostgreSQL level for defense-in-depth.
func (s *Session) setSessionReadOnly() error {
	// Send SET SESSION command to upstream database
	query := &pgproto3.Query{
		String: "SET SESSION default_transaction_read_only = on;",
	}

	s.upstreamFrontend.Send(query)

	if err := s.upstreamFrontend.Flush(); err != nil {
		return fmt.Errorf("send SET SESSION: %w", err)
	}

	// Read response from upstream
	for {
		msg, err := s.upstreamFrontend.Receive()
		if err != nil {
			return fmt.Errorf("receive response: %w", err)
		}

		switch msg.(type) {
		case *pgproto3.CommandComplete:
			// Success - read-only mode is now enforced
			continue
		case *pgproto3.ReadyForQuery:
			// Session is ready
			return nil
		case *pgproto3.ErrorResponse:
			return fmt.Errorf("%w: %v", ErrUpstreamReadOnlyMode, msg)
		default:
			continue
		}
	}
}
