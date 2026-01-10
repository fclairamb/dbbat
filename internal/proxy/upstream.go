package proxy

import (
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/jackc/pgx/v5/pgproto3"

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
			"application_name": buildApplicationName(s.clientApplicationName),
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
		return false, ErrSASLAuthNotSupported

	case *pgproto3.ParameterStatus:
		// Buffer ParameterStatus messages - they must come after AuthenticationOk
		// Make a copy since pgproto3 reuses the same struct
		s.logger.Debug("received ParameterStatus from upstream", "name", typedMsg.Name, "value", typedMsg.Value)
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
		s.logger.Debug("forwarding ParameterStatus messages to client", "count", len(s.bufferedParamStatus))
		for _, ps := range s.bufferedParamStatus {
			s.logger.Debug("forwarding ParameterStatus", "name", ps.Name, "value", ps.Value)
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
			s.logger.Error("failed to forward error to client", "error", err)
		}

		return false, fmt.Errorf("%w: %s", ErrUpstreamAuthFailed, typedMsg.Message)

	default:
		s.logger.Warn("unexpected message during upstream auth", "type", fmt.Sprintf("%T", typedMsg))

		return false, nil
	}
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

// buildApplicationName constructs the application_name for upstream connections.
// Format: "dbbat-{version}" or "dbbat-{version} / {client_app_name}"
func buildApplicationName(clientAppName string) string {
	dbbatName := "dbbat-" + version.Version

	clientAppName = strings.TrimSpace(clientAppName)
	if clientAppName == "" {
		return dbbatName
	}

	combined := dbbatName + " / " + clientAppName

	// Truncate if exceeds PostgreSQL limit
	if len(combined) > maxAppNameLen {
		// Calculate how much space we have for client app name
		// dbbatName + " / " = dbbatName length + 3
		maxClientLen := maxAppNameLen - len(dbbatName) - 3
		if maxClientLen > 0 {
			clientAppName = clientAppName[:maxClientLen]
			combined = dbbatName + " / " + clientAppName
		} else {
			combined = dbbatName
		}
	}

	return combined
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
