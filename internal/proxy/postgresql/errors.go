package postgresql

import "errors"

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

	ErrUpstreamAuthFailed  = errors.New("upstream authentication failed")
	ErrAPIKeyOwnerMismatch = errors.New("API key does not belong to user")
	ErrAPIKeyVerifyFailed  = errors.New("API key verification failed")

	// Startup negotiation errors. SSL/GSS encryption probes are length-8
	// frames with a magic version code; anything else of that shape is
	// rejected, and runaway clients are bounded by the round limit.
	ErrUnknownStartupMagic      = errors.New("unknown length-8 startup magic")
	ErrTooManyNegotiationRounds = errors.New("too many SSL/GSS negotiation rounds")

	// Upstream TLS errors raised when negotiating SSL with the target
	// Postgres server (see negotiateUpstreamSSL).
	ErrUpstreamTLSRequired = errors.New("upstream rejected TLS but ssl_mode requires it")
	ErrUpstreamSSLResponse = errors.New("unexpected upstream SSL response byte")

	// Upstream SCRAM/SASL errors raised when authenticating with the target
	// Postgres server using SCRAM-SHA-256.
	ErrSCRAMNoSupportedMechanism = errors.New("upstream offered no SCRAM mechanism we support")
	ErrSCRAMServerNonceMismatch  = errors.New("SCRAM server nonce did not extend client nonce")
	ErrSCRAMServerSignature      = errors.New("SCRAM server signature mismatch")
	ErrSCRAMUnexpectedMessage    = errors.New("unexpected SASL message from upstream")
	ErrSCRAMMalformedMessage     = errors.New("malformed SCRAM message from upstream")
)
