package postgresql

import (
	"errors"

	"github.com/fclairamb/dbbat/internal/proxy/upstream"
)

// Authentication and authorization errors.
var (
	ErrExpectedStartupMessage   = errors.New("expected StartupMessage")
	ErrMissingCredentials       = errors.New("missing username or database")
	ErrInvalidPassword          = errors.New("invalid password")
	ErrQueryLimitExceeded       = errors.New("query limit exceeded")
	ErrDataLimitExceeded        = errors.New("data transfer limit exceeded")
	ErrWriteNotPermitted        = errors.New("write operations not permitted with read-only access")
	ErrPasswordChangeNotAllowed = errors.New("password modification is not allowed through the proxy")
	ErrReadOnlyBypassAttempt    = errors.New("attempt to disable read-only mode is not permitted: " +
		"your access grant is read-only and cannot be changed for this session")
	ErrDDLNotPermitted  = errors.New("DDL operations not permitted: your access grant blocks schema modifications")
	ErrCopyNotPermitted = errors.New("COPY not permitted: your access grant blocks COPY commands")

	ErrAPIKeyOwnerMismatch = errors.New("API key does not belong to user")
	ErrAPIKeyVerifyFailed  = errors.New("API key verification failed")

	// Startup negotiation errors. SSL/GSS encryption probes are length-8
	// frames with a magic version code; anything else of that shape is
	// rejected, and runaway clients are bounded by the round limit.
	ErrUnknownStartupMagic      = errors.New("unknown length-8 startup magic")
	ErrTooManyNegotiationRounds = errors.New("too many SSL/GSS negotiation rounds")

	// ErrStartupMessageTooLarge / ErrPasswordMessageTooLarge reject a
	// pre-auth frame whose declared length exceeds what the protocol allows.
	// The length is four attacker-controlled bytes on an unauthenticated
	// socket, so it has to be checked *before* it sizes an allocation.
	ErrStartupMessageTooLarge  = errors.New("startup message exceeds the maximum length")
	ErrPasswordMessageTooLarge = errors.New("password message exceeds the maximum length")
	// ErrMalformedMessageLength rejects a declared length that cannot even
	// cover its own 4-byte length field (including the negative values a
	// high bit in the first byte would otherwise produce).
	ErrMalformedMessageLength = errors.New("declared message length is malformed")

	// ErrCancelRequestHandled ends a connection that carried a CancelRequest.
	// Not a failure: a CancelRequest is a one-shot out-of-band signal on its
	// own TCP connection, and PostgreSQL closes it without a reply.
	ErrCancelRequestHandled = errors.New("cancel request handled")
)

// Upstream connect failures. The upstream login itself lives in
// internal/proxy/upstream (one implementation, shared with the connectivity
// check); these names are kept as aliases so callers and tests in this package
// — and anything outside it that already matched on them — keep working.
var (
	// ErrUpstreamAuthFailed is returned when the target refused the stored
	// database credentials.
	ErrUpstreamAuthFailed = upstream.ErrPostgresAuthFailed
	// ErrUpstreamTLSRequired is returned when the target refused TLS while
	// ssl_mode demanded it.
	ErrUpstreamTLSRequired = upstream.ErrPostgresTLSRequired
	// ErrUpstreamSSLResponse is returned when the target answered the
	// SSLRequest with neither 'S' nor 'N'.
	ErrUpstreamSSLResponse = upstream.ErrPostgresSSLResponse
)
