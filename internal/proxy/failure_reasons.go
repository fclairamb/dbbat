package proxy

// Proxy failure reasons
const (
	// Authentication failures
	FailureReasonProxyInvalidUsername = "invalid_username"    // Username not found
	FailureReasonProxyInvalidPassword = "invalid_password"    // Wrong password
	FailureReasonProxyUserDisabled    = "user_disabled"       // Account disabled

	// Authorization failures
	FailureReasonNoGrant          = "no_grant"            // No grant for database
	FailureReasonGrantExpired     = "grant_expired"       // Grant expired
	FailureReasonGrantNotStarted  = "grant_not_started"   // Grant not yet active
	FailureReasonWrongAccessLevel = "wrong_access_level"  // Write attempt with read-only grant

	// Quota failures
	FailureReasonQueryQuotaExceeded = "query_quota_exceeded" // Max queries reached
	FailureReasonBytesQuotaExceeded = "bytes_quota_exceeded" // Max bytes reached

	// Database failures
	FailureReasonDatabaseNotFound   = "database_not_found"     // Database config doesn't exist
	FailureReasonDatabaseDisabled   = "database_disabled"      // Database disabled by admin
	FailureReasonUpstreamConnFailed = "upstream_conn_failed"   // Can't connect to target database
)
