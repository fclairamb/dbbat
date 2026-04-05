package postgresql

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/fclairamb/dbbat/internal/crypto"
	"github.com/fclairamb/dbbat/internal/store"
)

// authenticate performs DBBat authentication.
func (s *Session) authenticate() error {
	// Receive startup message directly from connection
	startupMsg, err := s.receiveStartupMessage()
	if err != nil {
		return fmt.Errorf("failed to receive startup message: %w", err)
	}

	// Extract username and database from startup message
	startup, ok := startupMsg.(*pgproto3.StartupMessage)
	if !ok {
		s.sendError("invalid startup message")

		return ErrExpectedStartupMessage
	}

	username := startup.Parameters["user"]
	databaseName := startup.Parameters["database"]
	s.clientApplicationName = startup.Parameters["application_name"]

	if username == "" || databaseName == "" {
		s.sendError("username and database required")

		return ErrMissingCredentials
	}

	// Look up user
	user, err := s.store.GetUserByUsername(s.ctx, username)
	if err != nil {
		s.sendError("authentication failed")

		return fmt.Errorf("user not found: %w", err)
	}

	s.user = user

	// Look up database configuration
	database, err := s.store.GetDatabaseByName(s.ctx, databaseName)
	if err != nil {
		s.sendError("database not found")

		return fmt.Errorf("database not found: %w", err)
	}

	s.database = database

	// Check for active grant
	grant, err := s.store.GetActiveGrant(s.ctx, user.UID, database.UID)
	if err != nil {
		s.sendError("access denied: no valid grant")

		return fmt.Errorf("no active grant: %w", err)
	}

	s.grant = grant

	// Check quotas
	if err := s.checkQuotas(); err != nil {
		s.sendError(err.Error())

		return err
	}

	// Request password from client (cleartext for simplicity)
	authRequest := &pgproto3.AuthenticationCleartextPassword{}

	buf, err := authRequest.Encode(nil)
	if err != nil {
		return fmt.Errorf("failed to encode auth request: %w", err)
	}

	if _, err := s.clientConn.Write(buf); err != nil {
		return fmt.Errorf("failed to send auth request: %w", err)
	}

	// Receive password - read directly since PasswordMessage comes before frontend is set up
	passwordMsg, err := s.receivePasswordMessage()
	if err != nil {
		return fmt.Errorf("failed to receive password: %w", err)
	}

	// Try API key authentication if password looks like an API key
	if isAPIKey(passwordMsg.Password) {
		if err := s.authenticateWithAPIKey(passwordMsg.Password); err != nil {
			s.sendError("authentication failed")

			return ErrInvalidPassword
		}

		s.authenticated = true

		return nil
	}

	// Verify password (using cache if available)
	var valid bool
	if s.authCache != nil {
		valid, err = s.authCache.VerifyPassword(s.ctx, user.UID.String(), passwordMsg.Password, user.PasswordHash)
	} else {
		valid, err = crypto.VerifyPassword(user.PasswordHash, passwordMsg.Password)
	}
	if err != nil || !valid {
		s.sendError("authentication failed")

		return ErrInvalidPassword
	}

	s.authenticated = true

	return nil
}

// isAPIKey checks if a password looks like a dbbat API key.
func isAPIKey(password string) bool {
	return len(password) >= store.APIKeyPrefixLength &&
		strings.HasPrefix(password, store.APIKeyPrefix)
}

// authenticateWithAPIKey verifies the password as an API key and checks ownership.
func (s *Session) authenticateWithAPIKey(apiKey string) error {
	verified, err := s.store.VerifyAPIKey(s.ctx, apiKey)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrAPIKeyVerifyFailed, err)
	}

	// Ensure the API key belongs to the user from the StartupMessage
	if verified.UserID != s.user.UID {
		return ErrAPIKeyOwnerMismatch
	}

	// Increment usage asynchronously
	go func() { _ = s.store.IncrementAPIKeyUsage(context.Background(), verified.ID) }()

	return nil
}

// checkQuotas verifies that quotas have not been exceeded.
func (s *Session) checkQuotas() error {
	if s.grant.MaxQueryCounts != nil && s.grant.QueryCount >= *s.grant.MaxQueryCounts {
		return ErrQueryLimitExceeded
	}

	if s.grant.MaxBytesTransferred != nil && s.grant.BytesTransferred >= *s.grant.MaxBytesTransferred {
		return ErrDataLimitExceeded
	}

	return nil
}
