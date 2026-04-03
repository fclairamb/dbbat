package api

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
)

// API errors.
var (
	ErrInvalidUID = errors.New("invalid UID")
)

// ErrorCode is a machine-readable error code returned in API responses.
type ErrorCode string

const (
	// ErrCodeInternalError indicates an unexpected server error.
	ErrCodeInternalError ErrorCode = "INTERNAL_ERROR"
	// ErrCodeValidationError indicates invalid input.
	ErrCodeValidationError ErrorCode = "VALIDATION_ERROR"
	// ErrCodeNotFound indicates the requested resource was not found.
	ErrCodeNotFound ErrorCode = "NOT_FOUND"
	// ErrCodeUnauthorized indicates authentication is required.
	ErrCodeUnauthorized ErrorCode = "UNAUTHORIZED"
	// ErrCodeForbidden indicates insufficient permissions.
	ErrCodeForbidden ErrorCode = "FORBIDDEN"
	// ErrCodeInvalidCredentials indicates wrong username or password.
	ErrCodeInvalidCredentials ErrorCode = "INVALID_CREDENTIALS"
	// ErrCodePasswordChangeRequired indicates the user must change their password.
	ErrCodePasswordChangeRequired ErrorCode = "PASSWORD_CHANGE_REQUIRED"
	// ErrCodeWeakPassword indicates the password does not meet requirements.
	ErrCodeWeakPassword ErrorCode = "WEAK_PASSWORD"
	// ErrCodeRateLimited indicates too many requests.
	ErrCodeRateLimited ErrorCode = "RATE_LIMITED"
	// ErrCodeOAuthFailed indicates an OAuth authentication failure.
	ErrCodeOAuthFailed ErrorCode = "OAUTH_FAILED"
	// ErrCodeOAuthStateMismatch indicates an invalid or expired OAuth state.
	ErrCodeOAuthStateMismatch ErrorCode = "OAUTH_STATE_MISMATCH"
	// ErrCodeOAuthProviderError indicates the OAuth provider returned an error.
	ErrCodeOAuthProviderError ErrorCode = "OAUTH_PROVIDER_ERROR"
	// ErrCodeOAuthUserNotLinked indicates no account is linked to the OAuth identity.
	ErrCodeOAuthUserNotLinked ErrorCode = "OAUTH_USER_NOT_LINKED"
	// ErrCodeOAuthWrongWorkspace indicates the wrong OAuth workspace was used.
	ErrCodeOAuthWrongWorkspace ErrorCode = "OAUTH_WRONG_WORKSPACE"
	// ErrCodeDuplicateName indicates a resource with that name already exists.
	ErrCodeDuplicateName ErrorCode = "DUPLICATE_NAME"
	// ErrCodeTargetMatchesSelf indicates the target matches the storage database.
	ErrCodeTargetMatchesSelf ErrorCode = "TARGET_MATCHES_SELF"
	// ErrCodeGrantExpired indicates the access grant has expired.
	ErrCodeGrantExpired ErrorCode = "GRANT_EXPIRED"
	// ErrCodeQuotaExceeded indicates a usage quota was exceeded.
	ErrCodeQuotaExceeded ErrorCode = "QUOTA_EXCEEDED"
)

// ErrorBody is the standard error response structure.
type ErrorBody struct {
	Code       ErrorCode `json:"code"`
	Message    string    `json:"message"`
	Detail     string    `json:"detail,omitempty"`
	RetryAfter int       `json:"retry_after,omitempty"`
}

// writeError sends a structured error response.
func writeError(c *gin.Context, status int, code ErrorCode, message string) {
	c.JSON(status, ErrorBody{
		Code:    code,
		Message: message,
	})
}

// writeRateLimited sends a 429 response with retry information.
func writeRateLimited(c *gin.Context, retryAfter int) {
	c.Header("Retry-After", strconv.Itoa(retryAfter))
	c.JSON(http.StatusTooManyRequests, ErrorBody{
		Code:       ErrCodeRateLimited,
		Message:    "Too many requests. Try again later.",
		RetryAfter: retryAfter,
	})
}

// writeInternalError logs the error and sends a generic 500 response.
// The actual error is never sent to the client.
func writeInternalError(c *gin.Context, logger *slog.Logger, err error, ctx string) {
	logger.ErrorContext(c.Request.Context(), ctx, slog.Any("error", err))
	c.JSON(http.StatusInternalServerError, ErrorBody{
		Code:    ErrCodeInternalError,
		Message: "An internal error occurred. Please try again.",
	})
}
