package api

// REST API failure reasons
const (
	// Credential failures
	FailureReasonInvalidUsername   = "invalid_username"         // Username not found
	FailureReasonInvalidPassword   = "invalid_password"         // Wrong password
	FailureReasonPasswordChangeReq = "password_change_required" // Initial password not changed

	// Token failures
	FailureReasonTokenInvalid = "token_invalid" // Malformed or unknown token
	FailureReasonTokenExpired = "token_expired" // Token past expiration
	FailureReasonTokenRevoked = "token_revoked" // Token was revoked

	// Account status
	FailureReasonUserDisabled = "user_disabled" // Account disabled by admin
	FailureReasonUserDeleted  = "user_deleted"  // Account was deleted
)
