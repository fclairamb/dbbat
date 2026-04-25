package mysql

import "errors"

// Sentinel errors for the MySQL proxy session lifecycle.
var (
	// ErrUserNotFound — the username from HandshakeResponse41 has no DBBat user.
	ErrUserNotFound = errors.New("user not found")
	// ErrDatabaseNotFound — no DBBat database matches the requested schema name.
	ErrDatabaseNotFound = errors.New("database not found")
	// ErrNoActiveGrant — the user has no current grant on the requested database.
	ErrNoActiveGrant = errors.New("no active grant")
	// ErrInvalidPassword — the supplied password failed Argon2id verification.
	ErrInvalidPassword = errors.New("invalid password")
	// ErrUnsupportedAuthPlugin — auth provider was asked for a plugin we don't implement.
	ErrUnsupportedAuthPlugin = errors.New("unsupported authentication plugin")
	// ErrSSLNotSupported — client sent SSL Request; v1 only supports plaintext.
	ErrSSLNotSupported = errors.New("TLS connections not supported by this proxy")
	// ErrUpstreamConnect — outbound connection to the upstream MySQL failed.
	ErrUpstreamConnect = errors.New("upstream connection failed")
	// ErrCommandNotPermitted — protocol-level command refused (admin/replication).
	ErrCommandNotPermitted = errors.New("command not permitted through dbbat")
	// ErrSwitchDatabaseDenied — COM_INIT_DB tried to change the session database.
	ErrSwitchDatabaseDenied = errors.New("switching database not permitted through dbbat")
	// ErrAPIKeyOwnerMismatch — the API key authenticates a different user than the handshake claimed.
	ErrAPIKeyOwnerMismatch = errors.New("API key does not belong to authenticating user")
	// ErrQueryLimitExceeded — the grant's max_query_count quota has been reached.
	ErrQueryLimitExceeded = errors.New("query count limit exceeded for this grant")
	// ErrDataLimitExceeded — the grant's max_bytes_transferred quota has been reached.
	ErrDataLimitExceeded = errors.New("data transfer limit exceeded for this grant")
)
