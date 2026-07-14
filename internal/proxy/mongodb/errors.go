package mongodb

import (
	"errors"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// MongoDB server error codes used by the proxy (contract §7).
const (
	codeAuthenticationFailed = 18
	codeUnauthorized         = 13
)

// Code names paired with the codes above.
const (
	codeNameAuthenticationFailed = "AuthenticationFailed"
	codeNameUnauthorized         = "Unauthorized"
)

// Sentinel errors for the MongoDB proxy session lifecycle. The user-facing
// text is what a mongosh / driver surfaces in the errmsg field (contract §7).
var (
	// ErrAuthenticationFailed — SASL PLAIN verification failed (bad password,
	// unknown user, or API-key mismatch).
	ErrAuthenticationFailed = errors.New("Authentication failed.")
	// ErrDatabaseNotResolvable — the target dbbat database could not be
	// resolved from authSource / user#db / a single active grant.
	ErrDatabaseNotResolvable = errors.New("dbbat: could not resolve target database; " +
		"connect with authSource=<dbbat-database-name> (or username 'user#database')")
	// ErrNoActiveGrant — the user has no current grant on the resolved database.
	ErrNoActiveGrant = errors.New("dbbat: no active grant for this database")
	// ErrTLSRequired — a PLAIN auth attempt arrived on a non-TLS connection.
	ErrTLSRequired = errors.New("dbbat: PLAIN authentication requires TLS")
	// ErrUpstreamConnect — the outbound connection to the upstream MongoDB failed.
	ErrUpstreamConnect = errors.New("dbbat: upstream MongoDB connection failed")
	// ErrCommandBlocked — a command was refused by grant controls or the
	// always-blocked list.
	ErrCommandBlocked = errors.New("dbbat: command not permitted")
	// ErrPreAuthNotAllowed — a non-allowlisted command was issued before auth.
	ErrPreAuthNotAllowed = errors.New("dbbat: authentication required")
	// ErrQueryLimitExceeded — the grant's max_query_count quota has been reached.
	ErrQueryLimitExceeded = errors.New("dbbat: query count limit exceeded for this grant")
	// ErrDataLimitExceeded — the grant's max_bytes_transferred quota has been reached.
	ErrDataLimitExceeded = errors.New("dbbat: data transfer limit exceeded for this grant")
)

// errorDoc builds the §7 error reply document:
// {ok: 0.0, errmsg, code, codeName}.
func errorDoc(code int32, codeName, errmsg string) bson.D {
	return bson.D{
		{Key: "ok", Value: 0.0},
		{Key: "errmsg", Value: errmsg},
		{Key: "code", Value: code},
		{Key: "codeName", Value: codeName},
	}
}

// authFailedDoc is the standard authentication-failure reply (contract §5/§7).
func authFailedDoc() bson.D {
	return errorDoc(codeAuthenticationFailed, codeNameAuthenticationFailed, ErrAuthenticationFailed.Error())
}

// unauthorizedDoc builds an Unauthorized (13) reply with a dbbat reason.
func unauthorizedDoc(reason string) bson.D {
	return errorDoc(codeUnauthorized, codeNameUnauthorized, reason)
}
