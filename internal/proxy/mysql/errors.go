package mysql

import "errors"

// Sentinel errors for the MySQL proxy session lifecycle.
var (
	// ErrUserNotFound — the username from HandshakeResponse41 has no DBBat user.
	ErrUserNotFound = errors.New("user not found")
	// ErrServerNotFound — no DBBat database matches the requested schema name.
	ErrServerNotFound = errors.New("database not found")
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
	// ErrSwitchDatabaseDenied — a client tried to change the session database,
	// through COM_INIT_DB, a text `USE`, or a `PREPARE … FROM '<USE …>'`.
	ErrSwitchDatabaseDenied = errors.New("switching database not permitted through dbbat")
	// ErrPreparedTextNotCheckable — a `PREPARE … FROM '<literal>'` whose
	// statement text dbbat could not read all the way down: an unterminated
	// literal, a text that is not one single literal (MySQL concatenates
	// adjacent ones), or a nested PREPARE. One level is unwrapped and checked;
	// a second is refused rather than unwrapped further, because stopping
	// silently would leave a hole the exact shape of the one the unwrapping
	// closed. `PREPARE … FROM @sql` is undecidable rather than unreadable and
	// is *not* refused — see docs/mysql.md.
	ErrPreparedTextNotCheckable = errors.New(
		"dbbat cannot read the statement text of this PREPARE, so it is not permitted: " +
			"prepare the inner statement directly")
	// ErrAPIKeyOwnerMismatch — the API key authenticates a different user than the handshake claimed.
	ErrAPIKeyOwnerMismatch = errors.New("API key does not belong to authenticating user")
	// ErrQueryLimitExceeded — the grant's max_query_count quota has been reached.
	ErrQueryLimitExceeded = errors.New("query count limit exceeded for this grant")
	// ErrDataLimitExceeded — the grant's max_bytes_transferred quota has been reached.
	ErrDataLimitExceeded = errors.New("data transfer limit exceeded for this grant")
	// ErrCachingSha2NeedsRSA — non-TLS caching_sha2 client tried full auth but no RSA key is configured.
	ErrCachingSha2NeedsRSA = errors.New("non-TLS caching_sha2_password requires an RSA key (configure mysql.tls or send password over TLS)")
	// ErrSaltFieldMissing — go-mysql library moved or removed the unexported Conn.salt field; readConnSalt cannot proceed.
	ErrSaltFieldMissing = errors.New("go-mysql Conn.salt field not found (library version mismatch?)")
	// ErrSaltFieldUnexpectedType — go-mysql library changed Conn.salt to a non-[]byte type.
	ErrSaltFieldUnexpectedType = errors.New("go-mysql Conn.salt field has unexpected type")
)
