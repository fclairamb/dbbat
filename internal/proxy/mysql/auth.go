package mysql

import (
	"context"
	"crypto/rsa"
	"crypto/tls"
	"log/slog"
	"strings"

	gomysql "github.com/go-mysql-org/go-mysql/mysql"
	gomysqlserver "github.com/go-mysql-org/go-mysql/server"

	"github.com/fclairamb/dbbat/internal/crypto"
	"github.com/fclairamb/dbbat/internal/store"
)

// newGoMySQLServer builds the shared go-mysql server config.
//
// We advertise caching_sha2_password as the default auth plugin (matching
// MySQL 8.x), with full-auth driven manually by dbbatAuthProvider so we can
// verify the resulting cleartext against Argon2id.
//
// TLS termination is enabled when tlsConfig is non-nil — the library handles
// the SSL Request packet upgrade transparently. The RSA private key (if any)
// is reused for caching_sha2_password's public-key-retrieval path on non-TLS
// connections.
func newGoMySQLServer(s *Server, tlsConfig *tls.Config, rsaKey *rsa.PrivateKey) *gomysqlserver.Server {
	return gomysqlserver.NewServerWithAuth(
		"8.4.0-dbbat",
		gomysql.DEFAULT_COLLATION_ID,
		gomysql.AUTH_CACHING_SHA2_PASSWORD,
		rsaKey,
		tlsConfig,
		&dbbatAuthProvider{server: s},
	)
}

// dbbatAuthProvider verifies the password (or API key) the client sends as
// auth_response against the DBBat user store. It's shared across all
// sessions and stateless — username comes from the *gomysqlserver.Conn.
type dbbatAuthProvider struct {
	server *Server
}

// Validate is called by the library to confirm we know how to handle the
// requested plugin. We support caching_sha2_password (the default) and
// mysql_clear_password (legacy fallback for clients that explicitly pin to
// it). mysql_native_password is not supported because we cannot derive the
// scramble hash from an Argon2id stored password.
func (p *dbbatAuthProvider) Validate(plugin string) bool {
	switch plugin {
	case gomysql.AUTH_CACHING_SHA2_PASSWORD, gomysql.AUTH_CLEAR_PASSWORD:
		return true
	default:
		return false
	}
}

// Authenticate is called by go-mysql once the client has sent its initial
// auth response. We dispatch on the negotiated plugin; both supported
// plugins ultimately yield a cleartext password we verify against Argon2id.
func (p *dbbatAuthProvider) Authenticate(c *gomysqlserver.Conn, plugin string, authData []byte) error {
	username := c.GetUser()

	password, err := p.extractPlaintext(c, plugin, authData)
	if err != nil {
		return err
	}

	return p.verifyCredentials(username, password)
}

// extractPlaintext recovers the cleartext password (or API key) from the
// client's auth response, driving the protocol-specific exchange where
// needed (RSA key retrieval for non-TLS caching_sha2).
func (p *dbbatAuthProvider) extractPlaintext(c *gomysqlserver.Conn, plugin string, authData []byte) (string, error) {
	switch plugin {
	case gomysql.AUTH_CLEAR_PASSWORD:
		// mysql_clear_password sends the cleartext NUL-terminated.
		return trimTrailingNUL(string(authData)), nil

	case gomysql.AUTH_CACHING_SHA2_PASSWORD:
		// Empty auth_data means an empty password — verify against Argon2id
		// of "". driveCachingSha2FullAuth would otherwise wait for a
		// non-existent follow-up packet.
		if len(authData) == 0 {
			return "", nil
		}

		return driveCachingSha2FullAuth(c, p.server.rsaPrivateKey)

	default:
		return "", ErrUnsupportedAuthPlugin
	}
}

// verifyCredentials checks the cleartext against the user's Argon2id hash,
// or interprets it as an API key when it carries the dbb_ prefix.
func (p *dbbatAuthProvider) verifyCredentials(username, password string) error {
	if isAPIKey(password) {
		return p.authenticateAPIKey(username, password)
	}

	user, err := p.server.store.GetUserByUsername(p.server.ctx, username)
	if err != nil {
		return gomysqlserver.ErrAccessDenied
	}

	var (
		valid bool
		verr  error
	)

	if p.server.authCache != nil {
		valid, verr = p.server.authCache.VerifyPassword(p.server.ctx, user.UID.String(), password, user.PasswordHash)
	} else {
		valid, verr = crypto.VerifyPassword(user.PasswordHash, password)
	}

	if verr != nil || !valid {
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
// library a placeholder credential after verifying the user exists, then
// looks up the database and grant once auth completes.
//
// The placeholder credential carries AuthPluginName = caching_sha2_password
// to match the server default and short-circuit the library's
// AuthSwitchRequest path. dbbatAuthProvider.Authenticate does the real
// verification against Argon2id.
type dbbatAuthHandler struct {
	session *Session
}

func (h *dbbatAuthHandler) GetCredential(username string) (gomysqlserver.Credential, bool, error) {
	user, err := h.session.server.store.GetUserByUsername(h.session.ctx, username)
	if err != nil {
		// Any lookup failure is reported to the client as ER_NO_SUCH_USER.
		// Underlying store errors are deliberately swallowed to avoid
		// leaking schema/connectivity details across the handshake.
		return gomysqlserver.Credential{}, false, nil //nolint:nilerr // intentional: hide store errors as not-found
	}

	h.session.user = user

	// Passwords must be non-empty for the library to treat the user as
	// existing; the value itself is unused (our authProvider verifies the
	// real Argon2id hash).
	return gomysqlserver.Credential{
		Passwords:      []string{""},
		AuthPluginName: gomysql.AUTH_CACHING_SHA2_PASSWORD,
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

	if !store.IsMySQLFamily(db.Protocol) {
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
