package mysql

import (
	"context"
	"log/slog"
	"strings"

	gomysql "github.com/go-mysql-org/go-mysql/mysql"
	gomysqlserver "github.com/go-mysql-org/go-mysql/server"

	"github.com/fclairamb/dbbat/internal/crypto"
	"github.com/fclairamb/dbbat/internal/store"
)

// newGoMySQLServer builds the shared go-mysql server config for all incoming
// connections. We advertise mysql_clear_password as the default auth method so
// the client sends the password in cleartext (verified against the user's
// Argon2id hash from the DBBat store, the same way the PostgreSQL proxy does).
//
// TLS is disabled in v1 — the proxy refuses SSL Request packets. Deploy on a
// private network. caching_sha2_password and TLS termination are tracked as
// v2 follow-ups in docs/mysql.md.
func newGoMySQLServer(s *Server) *gomysqlserver.Server {
	return gomysqlserver.NewServerWithAuth(
		"8.4.0-dbbat",
		gomysql.DEFAULT_COLLATION_ID,
		gomysql.AUTH_CLEAR_PASSWORD,
		nil, // no RSA key (only needed for caching_sha2/sha256)
		nil, // no TLS — refuse SSL Request from clients
		&dbbatAuthProvider{server: s},
	)
}

// dbbatAuthProvider verifies the cleartext password (or API key) the client
// sends as auth_response against the DBBat user store.
//
// It is shared across all sessions for a given Server instance. It carries
// no per-session state — username comes from the *gomysqlserver.Conn.
type dbbatAuthProvider struct {
	server *Server
}

func (p *dbbatAuthProvider) Validate(plugin string) bool {
	return plugin == gomysql.AUTH_CLEAR_PASSWORD
}

func (p *dbbatAuthProvider) Authenticate(c *gomysqlserver.Conn, plugin string, authData []byte) error {
	if plugin != gomysql.AUTH_CLEAR_PASSWORD {
		return ErrUnsupportedAuthPlugin
	}

	password := string(authData)
	// mysql_clear_password may be null-terminated; strip the trailing NUL.
	password = strings.TrimRight(password, "\x00")

	username := c.GetUser()

	// API key path: prefix-match before falling back to user password.
	if isAPIKey(password) {
		return p.authenticateAPIKey(username, password)
	}

	user, err := p.server.store.GetUserByUsername(p.server.ctx, username)
	if err != nil {
		return gomysqlserver.ErrAccessDenied
	}

	var valid bool
	if p.server.authCache != nil {
		valid, err = p.server.authCache.VerifyPassword(p.server.ctx, user.UID.String(), password, user.PasswordHash)
	} else {
		valid, err = crypto.VerifyPassword(user.PasswordHash, password)
	}

	if err != nil || !valid {
		return gomysqlserver.ErrAccessDenied
	}

	return nil
}

func (p *dbbatAuthProvider) authenticateAPIKey(username, key string) error {
	verified, err := p.server.store.VerifyAPIKey(p.server.ctx, key)
	if err != nil {
		return gomysqlserver.ErrAccessDenied
	}

	user, err := p.server.store.GetUserByUsername(p.server.ctx, username)
	if err != nil || user.UID != verified.UserID {
		return ErrAPIKeyOwnerMismatch
	}

	go func() { _ = p.server.store.IncrementAPIKeyUsage(context.Background(), verified.ID) }()

	return nil
}

// isAPIKey reports whether the given password looks like a DBBat API key.
func isAPIKey(password string) bool {
	return len(password) >= store.APIKeyPrefixLength &&
		strings.HasPrefix(password, store.APIKeyPrefix)
}

// dbbatAuthHandler is the per-connection AuthenticationHandler. It hands the
// library a placeholder credential after verifying the user exists, so the
// library proceeds with the AUTH_CLEAR_PASSWORD flow that our authProvider
// actually verifies. After the password check passes, OnAuthSuccess looks up
// the database and grant.
type dbbatAuthHandler struct {
	session *Session
}

func (h *dbbatAuthHandler) GetCredential(username string) (gomysqlserver.Credential, bool, error) {
	user, err := h.session.server.store.GetUserByUsername(h.session.ctx, username)
	if err != nil {
		// Treat any lookup failure as "user not found" — go-mysql will respond
		// with ER_NO_SUCH_USER. We deliberately do not propagate the underlying
		// store error to avoid leaking schema/connectivity details to clients.
		return gomysqlserver.Credential{}, false, nil //nolint:nilerr // intentional: hide store errors as not-found
	}

	h.session.user = user

	// The Passwords field is required to be non-empty by the library, but its
	// content is never used — our dbbatAuthProvider.Authenticate does the real
	// verification against the Argon2id hash.
	return gomysqlserver.Credential{
		Passwords:      []string{""},
		AuthPluginName: gomysql.AUTH_CLEAR_PASSWORD,
	}, true, nil
}

func (h *dbbatAuthHandler) OnAuthSuccess(_ *gomysqlserver.Conn) error {
	s := h.session

	if s.requestedDB == "" {
		return ErrDatabaseNotFound
	}

	db, err := s.server.store.GetDatabaseByName(s.ctx, s.requestedDB)
	if err != nil {
		return ErrDatabaseNotFound
	}

	if db.Protocol != store.ProtocolMySQL {
		return ErrDatabaseNotFound
	}

	s.database = db

	grant, err := s.server.store.GetActiveGrant(s.ctx, s.user.UID, db.UID)
	if err != nil {
		return ErrNoActiveGrant
	}

	s.grant = grant

	if err := checkQuotas(grant); err != nil {
		return err
	}

	s.authComplete = true

	return nil
}

func (h *dbbatAuthHandler) OnAuthFailure(c *gomysqlserver.Conn, err error) {
	h.session.logger.WarnContext(h.session.ctx, "MySQL auth failed",
		slog.String("user", c.GetUser()),
		slog.Any("error", err))
}

// checkQuotas verifies the grant's count/byte quotas have not been exceeded.
func checkQuotas(grant *store.Grant) error {
	if grant.MaxQueryCounts != nil && grant.QueryCount >= *grant.MaxQueryCounts {
		return ErrQueryLimitExceeded
	}

	if grant.MaxBytesTransferred != nil && grant.BytesTransferred >= *grant.MaxBytesTransferred {
		return ErrDataLimitExceeded
	}

	return nil
}
