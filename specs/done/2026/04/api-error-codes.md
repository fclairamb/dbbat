# Standardized API Error Codes

## Goal

Replace ad-hoc `gin.H{"error": "message"}` responses with a consistent error structure using typed error codes. This makes errors machine-parseable for the frontend, simplifies error handling, and prevents leaking implementation details to clients.

Inspired by solidping's `ErrorCode` pattern and [RFC 7807 Problem Details](https://datatracker.ietf.org/doc/html/rfc7807).

## Prerequisites

- None (can be done independently)

## Outcome

- `ErrorCode` type with all error codes defined as constants
- `ErrorResponse` struct with `code`, `message`, and optional `detail` fields
- Helper functions: `WriteError`, `WriteValidationError`, `WriteInternalError`
- All existing handlers migrated to use the new error helpers
- Frontend updated to handle error codes consistently
- No change to success response formats

## Non-Goals

- Changing success response structures
- Adding request/correlation IDs (future enhancement)
- Sentry/error reporting integration (future enhancement)

---

## Current State

dbbat currently has two error patterns:

**Pattern 1 — `errorResponse` helper** (most endpoints):
```go
errorResponse(c, http.StatusBadRequest, "protocol must be 'postgresql' or 'oracle'")
// → {"error": "protocol must be 'postgresql' or 'oracle'"}
```

**Pattern 2 — inline `gin.H`** (auth endpoints):
```go
c.JSON(http.StatusForbidden, gin.H{
    "error":   "password_change_required",
    "message": "You must change your password before logging in",
})
```

Problems:
1. No consistent structure — some have `error` + `message`, others just `error`
2. Error codes exist only for 2 cases (`password_change_required`, `auth_rate_limited`)
3. Frontend has to match on free-text strings, which is fragile
4. Internal errors sometimes leak implementation details

## Proposed Error Response

```json
{
  "code": "VALIDATION_ERROR",
  "message": "protocol must be 'postgresql' or 'oracle'",
  "detail": "Allowed values: postgresql, oracle"
}
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `code` | string | Yes | Machine-readable error code (e.g., `VALIDATION_ERROR`) |
| `message` | string | Yes | Human-readable message suitable for display |
| `detail` | string | No | Additional context (never contains sensitive data) |
| `retry_after` | int | No | Seconds until retry (only for rate-limited responses) |

## Error Codes

**File**: `internal/api/errors.go`

```go
package api

// ErrorCode is a machine-readable error code returned in API responses.
type ErrorCode string

const (
    // General errors
    ErrCodeInternalError   ErrorCode = "INTERNAL_ERROR"
    ErrCodeValidationError ErrorCode = "VALIDATION_ERROR"
    ErrCodeNotFound        ErrorCode = "NOT_FOUND"

    // Auth errors
    ErrCodeUnauthorized          ErrorCode = "UNAUTHORIZED"
    ErrCodeForbidden             ErrorCode = "FORBIDDEN"
    ErrCodeInvalidCredentials    ErrorCode = "INVALID_CREDENTIALS"
    ErrCodePasswordChangeRequired ErrorCode = "PASSWORD_CHANGE_REQUIRED"
    ErrCodeWeakPassword          ErrorCode = "WEAK_PASSWORD"
    ErrCodeRateLimited           ErrorCode = "RATE_LIMITED"

    // OAuth errors
    ErrCodeOAuthFailed           ErrorCode = "OAUTH_FAILED"
    ErrCodeOAuthStateMismatch    ErrorCode = "OAUTH_STATE_MISMATCH"
    ErrCodeOAuthProviderError    ErrorCode = "OAUTH_PROVIDER_ERROR"
    ErrCodeOAuthUserNotLinked    ErrorCode = "OAUTH_USER_NOT_LINKED"
    ErrCodeOAuthWrongWorkspace   ErrorCode = "OAUTH_WRONG_WORKSPACE"

    // Resource errors
    ErrCodeDuplicateName    ErrorCode = "DUPLICATE_NAME"
    ErrCodeTargetMatchesSelf ErrorCode = "TARGET_MATCHES_SELF"
    ErrCodeGrantExpired     ErrorCode = "GRANT_EXPIRED"
    ErrCodeQuotaExceeded    ErrorCode = "QUOTA_EXCEEDED"
)

// ErrorResponse is the standard error response structure.
type ErrorResponse struct {
    Code       ErrorCode `json:"code"`
    Message    string    `json:"message"`
    Detail     string    `json:"detail,omitempty"`
    RetryAfter int       `json:"retry_after,omitempty"`
}
```

## Helper Functions

```go
// writeError sends a structured error response.
func writeError(c *gin.Context, status int, code ErrorCode, message string) {
    c.JSON(status, ErrorResponse{
        Code:    code,
        Message: message,
    })
}

// writeErrorDetail sends a structured error with additional detail.
func writeErrorDetail(c *gin.Context, status int, code ErrorCode, message, detail string) {
    c.JSON(status, ErrorResponse{
        Code:    code,
        Message: message,
        Detail:  detail,
    })
}

// writeRateLimited sends a 429 response with retry information.
func writeRateLimited(c *gin.Context, retryAfter int) {
    c.Header("Retry-After", strconv.Itoa(retryAfter))
    c.JSON(http.StatusTooManyRequests, ErrorResponse{
        Code:       ErrCodeRateLimited,
        Message:    "Too many requests. Try again later.",
        RetryAfter: retryAfter,
    })
}

// writeInternalError logs the error and sends a generic 500 response.
// The actual error is never sent to the client.
func writeInternalError(c *gin.Context, logger *slog.Logger, err error, context string) {
    logger.ErrorContext(c.Request.Context(), context, slog.Any("error", err))
    c.JSON(http.StatusInternalServerError, ErrorResponse{
        Code:    ErrCodeInternalError,
        Message: "An internal error occurred. Please try again.",
    })
}
```

## Migration Examples

### Before:
```go
errorResponse(c, http.StatusBadRequest, "invalid request: username and password required")
```

### After:
```go
writeError(c, http.StatusBadRequest, ErrCodeValidationError, "username and password are required")
```

### Before:
```go
c.JSON(http.StatusForbidden, gin.H{
    "error":   "password_change_required",
    "message": "You must change your password before logging in",
})
```

### After:
```go
writeError(c, http.StatusForbidden, ErrCodePasswordChangeRequired, "You must change your password before logging in")
```

### Before:
```go
c.Header("Retry-After", strconv.Itoa(retryAfter))
c.JSON(http.StatusTooManyRequests, gin.H{
    "error":       ErrCodeAuthRateLimited,
    "message":     "Too many failed login attempts. Try again later.",
    "retry_after": retryAfter,
})
```

### After:
```go
writeRateLimited(c, retryAfter)
```

## Frontend Changes

**File**: `front/src/api/client.ts`

Update error handling to parse the new structure:

```typescript
interface ApiError {
  code: string;
  message: string;
  detail?: string;
  retry_after?: number;
}

// In the API middleware or error handler:
if (response.status >= 400) {
  const body = await response.json() as ApiError;

  switch (body.code) {
    case "PASSWORD_CHANGE_REQUIRED":
      // Redirect to password change
      break;
    case "RATE_LIMITED":
      // Show retry message with body.retry_after
      break;
    case "UNAUTHORIZED":
      // Redirect to login
      break;
    default:
      // Show body.message to user
      break;
  }
}
```

The frontend should match on `code` (stable, machine-readable) rather than `message` (may change).

## OpenAPI Spec

Add the `ErrorResponse` schema and reference it from all error responses:

```yaml
components:
  schemas:
    ErrorResponse:
      type: object
      required: [code, message]
      properties:
        code:
          type: string
          description: Machine-readable error code
          enum:
            - INTERNAL_ERROR
            - VALIDATION_ERROR
            - NOT_FOUND
            - UNAUTHORIZED
            - FORBIDDEN
            - INVALID_CREDENTIALS
            - PASSWORD_CHANGE_REQUIRED
            - WEAK_PASSWORD
            - RATE_LIMITED
            - OAUTH_FAILED
            - OAUTH_STATE_MISMATCH
            - OAUTH_PROVIDER_ERROR
            - OAUTH_USER_NOT_LINKED
            - OAUTH_WRONG_WORKSPACE
            - DUPLICATE_NAME
            - TARGET_MATCHES_SELF
            - GRANT_EXPIRED
            - QUOTA_EXCEEDED
        message:
          type: string
          description: Human-readable error message
        detail:
          type: string
          description: Additional context
        retry_after:
          type: integer
          description: Seconds until retry (rate-limited responses only)
```

## Implementation Plan

This can be done incrementally:

1. **Add `errors.go`** with `ErrorCode`, `ErrorResponse`, and helper functions
2. **Migrate auth handlers** (highest impact — frontend already checks some codes)
3. **Migrate resource handlers** (databases, grants, users, keys)
4. **Migrate observability handlers** (connections, queries, audit)
5. **Update frontend** error handling to use `code` field
6. **Update OpenAPI spec** with `ErrorResponse` schema
7. **Remove old `errorResponse`** function

Each step is independently deployable. Steps 1-3 are the critical path.

## Tests

```go
func TestErrorResponse_Format(t *testing.T) {
    t.Run("validation error", func(t *testing.T) {
        w := httptest.NewRecorder()
        c, _ := gin.CreateTestContext(w)
        writeError(c, http.StatusBadRequest, ErrCodeValidationError, "name is required")

        assert.Equal(t, http.StatusBadRequest, w.Code)

        var resp ErrorResponse
        json.Unmarshal(w.Body.Bytes(), &resp)
        assert.Equal(t, ErrCodeValidationError, resp.Code)
        assert.Equal(t, "name is required", resp.Message)
    })

    t.Run("rate limited includes retry_after", func(t *testing.T) {
        w := httptest.NewRecorder()
        c, _ := gin.CreateTestContext(w)
        writeRateLimited(c, 30)

        assert.Equal(t, http.StatusTooManyRequests, w.Code)
        assert.Equal(t, "30", w.Header().Get("Retry-After"))

        var resp ErrorResponse
        json.Unmarshal(w.Body.Bytes(), &resp)
        assert.Equal(t, ErrCodeRateLimited, resp.Code)
        assert.Equal(t, 30, resp.RetryAfter)
    })

    t.Run("internal error hides details", func(t *testing.T) {
        w := httptest.NewRecorder()
        c, _ := gin.CreateTestContext(w)
        writeInternalError(c, slog.Default(), errors.New("db connection lost"), "failed to list users")

        var resp ErrorResponse
        json.Unmarshal(w.Body.Bytes(), &resp)
        assert.Equal(t, ErrCodeInternalError, resp.Code)
        assert.NotContains(t, resp.Message, "db connection")
    })
}
```

## Files Summary

| File | Type | Description |
|------|------|-------------|
| `internal/api/errors.go` | New | Error codes, response struct, helper functions |
| `internal/api/errors_test.go` | New | Tests for error helpers |
| `internal/api/auth.go` | Modified | Migrate to new error helpers |
| `internal/api/databases.go` | Modified | Migrate to new error helpers |
| `internal/api/grants.go` | Modified | Migrate to new error helpers |
| `internal/api/users.go` | Modified | Migrate to new error helpers |
| `internal/api/keys.go` | Modified | Migrate to new error helpers |
| `internal/api/middleware.go` | Modified | Migrate rate limit errors, remove old constants |
| `internal/api/server.go` | Modified | Remove old `errorResponse` function |
| `internal/api/openapi.yml` | Modified | Add `ErrorResponse` schema |
| `front/src/api/client.ts` | Modified | Parse error codes |
| `front/src/routes/login.tsx` | Modified | Handle error codes from API |

## Acceptance Criteria

1. All API error responses use `{code, message}` structure
2. No error response contains `gin.H` — all use `ErrorResponse`
3. Frontend can switch on `code` field for all error handling
4. Internal errors (500) never leak implementation details
5. Rate-limited responses include `retry_after` in body and `Retry-After` header
6. Old `errorResponse` function is removed
7. OpenAPI spec documents the error response schema
8. All existing tests pass (response format change may require test updates)

## Estimated Size

~100 lines errors.go + ~50 lines tests + ~200 lines handler migration + ~50 lines frontend = **~400 lines changed**
