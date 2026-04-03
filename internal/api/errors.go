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
	// General errors.
	ErrCodeInternalError   ErrorCode = "INTERNAL_ERROR"
	ErrCodeValidationError ErrorCode = "VALIDATION_ERROR"
	ErrCodeNotFound        ErrorCode = "NOT_FOUND"

	// Auth errors.
	ErrCodeUnauthorized           ErrorCode = "UNAUTHORIZED"
	ErrCodeForbidden              ErrorCode = "FORBIDDEN"
	ErrCodeInvalidCredentials     ErrorCode = "INVALID_CREDENTIALS"
	ErrCodePasswordChangeRequired ErrorCode = "PASSWORD_CHANGE_REQUIRED"
	ErrCodeWeakPassword           ErrorCode = "WEAK_PASSWORD"
	ErrCodeRateLimited            ErrorCode = "RATE_LIMITED"

	// OAuth errors (for future Slack auth).
	ErrCodeOAuthFailed         ErrorCode = "OAUTH_FAILED"
	ErrCodeOAuthStateMismatch  ErrorCode = "OAUTH_STATE_MISMATCH"
	ErrCodeOAuthProviderError  ErrorCode = "OAUTH_PROVIDER_ERROR"
	ErrCodeOAuthUserNotLinked  ErrorCode = "OAUTH_USER_NOT_LINKED"
	ErrCodeOAuthWrongWorkspace ErrorCode = "OAUTH_WRONG_WORKSPACE"

	// Resource errors.
	ErrCodeDuplicateName     ErrorCode = "DUPLICATE_NAME"
	ErrCodeTargetMatchesSelf ErrorCode = "TARGET_MATCHES_SELF"
	ErrCodeGrantExpired      ErrorCode = "GRANT_EXPIRED"
	ErrCodeQuotaExceeded     ErrorCode = "QUOTA_EXCEEDED"
)

// APIError is the standard error response structure.
type APIError struct {
	Code       ErrorCode `json:"code"`
	Message    string    `json:"message"`
	Detail     string    `json:"detail,omitempty"`
	RetryAfter int       `json:"retry_after,omitempty"`
}

// writeError sends a structured error response.
func writeError(c *gin.Context, status int, code ErrorCode, message string) {
	c.JSON(status, APIError{
		Code:    code,
		Message: message,
	})
}

// writeErrorDetail sends a structured error with additional detail.
func writeErrorDetail(c *gin.Context, status int, code ErrorCode, message, detail string) {
	c.JSON(status, APIError{
		Code:    code,
		Message: message,
		Detail:  detail,
	})
}

// writeRateLimited sends a 429 response with retry information.
func writeRateLimited(c *gin.Context, retryAfter int) {
	c.Header("Retry-After", strconv.Itoa(retryAfter))
	c.JSON(http.StatusTooManyRequests, APIError{
		Code:       ErrCodeRateLimited,
		Message:    "Too many requests. Try again later.",
		RetryAfter: retryAfter,
	})
}

// writeInternalError logs the error and sends a generic 500 response.
// The actual error is never sent to the client.
func writeInternalError(c *gin.Context, logger *slog.Logger, err error, ctx string) {
	logger.ErrorContext(c.Request.Context(), ctx, slog.Any("error", err))
	c.JSON(http.StatusInternalServerError, APIError{
		Code:    ErrCodeInternalError,
		Message: "An internal error occurred. Please try again.",
	})
}
