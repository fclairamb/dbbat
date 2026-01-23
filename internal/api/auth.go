package api

import (
	"log/slog"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/fclairamb/dbbat/internal/crypto"
	"github.com/fclairamb/dbbat/internal/store"
)

// LoginRequest represents the request body for login
type LoginRequest struct {
	Username string `json:"username" binding:"required"`
	Password string `json:"password" binding:"required"`
}

// LoginResponse represents the response for a successful login
type LoginResponse struct {
	Token     string       `json:"token"`
	ExpiresAt string       `json:"expires_at"`
	User      UserResponse `json:"user"`
}

// UserResponse represents user info in login/me responses
type UserResponse struct {
	UID                    string   `json:"uid"`
	Username               string   `json:"username"`
	Roles                  []string `json:"roles"`
	PasswordChangeRequired bool     `json:"password_change_required"`
}

// SessionResponse represents session info in me response
type SessionResponse struct {
	ExpiresAt string `json:"expires_at"`
	CreatedAt string `json:"created_at"`
}

// MeResponse represents the response for /auth/me
type MeResponse struct {
	UID                    string          `json:"uid"`
	Username               string          `json:"username"`
	Roles                  []string        `json:"roles"`
	PasswordChangeRequired bool            `json:"password_change_required"`
	Session                SessionResponse `json:"session"`
}

// handleLogin creates a web session for the user
// POST /api/auth/login
func (s *Server) handleLogin(c *gin.Context) {
	var req LoginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		errorResponse(c, http.StatusBadRequest, "invalid request: username and password required")
		return
	}

	// Check rate limit BEFORE verifying credentials
	// Skip rate limiting in test mode
	if !s.isTestMode() {
		if allowed, retryAfter := s.authFailureTracker.checkRateLimit(req.Username); !allowed {
			c.Header("Retry-After", strconv.Itoa(retryAfter))
			c.JSON(http.StatusTooManyRequests, gin.H{
				"error":       ErrCodeAuthRateLimited,
				"message":     "Too many failed login attempts. Try again later.",
				"retry_after": retryAfter,
			})
			return
		}
	}

	ctx := c.Request.Context()

	// Look up user
	user, err := s.store.GetUserByUsername(ctx, req.Username)
	if err != nil {
		s.authFailureTracker.recordFailure(req.Username)
		errorResponse(c, http.StatusUnauthorized, "invalid credentials")
		return
	}

	// Verify password
	valid, err := crypto.VerifyPassword(user.PasswordHash, req.Password)
	if err != nil || !valid {
		s.authFailureTracker.recordFailure(req.Username)
		errorResponse(c, http.StatusUnauthorized, "invalid credentials")
		return
	}

	// Reset failure count on successful login
	s.authFailureTracker.resetFailures(req.Username)

	// Check if password change is required BEFORE creating a session
	// Users with unchanged passwords cannot login - they must change password first
	if !user.HasChangedPassword() {
		c.JSON(http.StatusForbidden, gin.H{
			"error":   ErrCodePasswordChangeRequired,
			"message": "You must change your password before logging in",
		})
		return
	}

	// Create web session
	apiKey, plainKey, err := s.store.CreateWebSession(ctx, user.UID)
	if err != nil {
		s.logger.ErrorContext(ctx, "failed to create web session", slog.Any("error", err), slog.String("user", user.Username))
		errorResponse(c, http.StatusInternalServerError, "failed to create session")
		return
	}

	// Format expiration time
	expiresAt := ""
	if apiKey.ExpiresAt != nil {
		expiresAt = apiKey.ExpiresAt.Format("2006-01-02T15:04:05Z07:00")
	}

	c.JSON(http.StatusOK, LoginResponse{
		Token:     plainKey,
		ExpiresAt: expiresAt,
		User: UserResponse{
			UID:                    user.UID.String(),
			Username:               user.Username,
			Roles:                  user.Roles,
			PasswordChangeRequired: !user.HasChangedPassword(),
		},
	})
}

// handleLogout revokes the current web session
// POST /api/auth/logout
func (s *Server) handleLogout(c *gin.Context) {
	// Get the API key from context (set by auth middleware)
	apiKeyVal, exists := c.Get(contextKeyAPIKey)
	if !exists {
		// No API key in context - might be Basic Auth
		// Just return success (nothing to revoke)
		c.Status(http.StatusNoContent)
		return
	}

	apiKey, ok := apiKeyVal.(*store.APIKey)
	if !ok || apiKey == nil {
		c.Status(http.StatusNoContent)
		return
	}

	// Revoke the session
	currentUser := getCurrentUser(c)
	if currentUser != nil {
		if err := s.store.RevokeAPIKey(c.Request.Context(), apiKey.ID, currentUser.UID); err != nil {
			s.logger.ErrorContext(c.Request.Context(), "failed to revoke web session", slog.Any("error", err))
			// Don't return error - logout should always succeed from client perspective
		}
	}

	c.Status(http.StatusNoContent)
}

// handleMe returns the current user and session information
// GET /api/auth/me
func (s *Server) handleMe(c *gin.Context) {
	currentUser := getCurrentUser(c)
	if currentUser == nil {
		errorResponse(c, http.StatusUnauthorized, "not authenticated")
		return
	}

	// Get session info if authenticated via API key
	var session SessionResponse
	apiKeyVal, exists := c.Get(contextKeyAPIKey)
	if exists {
		apiKey, ok := apiKeyVal.(*store.APIKey)
		if ok && apiKey != nil {
			if apiKey.ExpiresAt != nil {
				session.ExpiresAt = apiKey.ExpiresAt.Format("2006-01-02T15:04:05Z07:00")
			}
			session.CreatedAt = apiKey.CreatedAt.Format("2006-01-02T15:04:05Z07:00")
		}
	}

	c.JSON(http.StatusOK, MeResponse{
		UID:                    currentUser.UID.String(),
		Username:               currentUser.Username,
		Roles:                  currentUser.Roles,
		PasswordChangeRequired: !currentUser.HasChangedPassword(),
		Session:                session,
	})
}

// ChangePasswordRequest represents the request body for authenticated password change
// Requires re-authentication via username/password (not Bearer token)
// Username is optional when changing your own password (inferred from :uid param)
type ChangePasswordRequest struct {
	Username        string `json:"username"`
	CurrentPassword string `json:"current_password" binding:"required"`
	NewPassword     string `json:"new_password" binding:"required"`
}

// PreLoginPasswordChangeRequest represents the request body for pre-login password change
type PreLoginPasswordChangeRequest struct {
	Username        string `json:"username" binding:"required"`
	CurrentPassword string `json:"current_password" binding:"required"`
	NewPassword     string `json:"new_password" binding:"required"`
}

// Minimum password length
const minPasswordLength = 8

// handlePreLoginPasswordChange allows users to change their password before logging in
// This is the bootstrap path for new users who must change their initial password
// PUT /api/v1/auth/password
func (s *Server) handlePreLoginPasswordChange(c *gin.Context) {
	var req PreLoginPasswordChangeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		errorResponse(c, http.StatusBadRequest, "username, current_password, and new_password are required")
		return
	}

	// Check rate limit BEFORE verifying credentials
	// Skip rate limiting in test mode
	if !s.isTestMode() {
		if allowed, retryAfter := s.authFailureTracker.checkRateLimit(req.Username); !allowed {
			c.Header("Retry-After", strconv.Itoa(retryAfter))
			c.JSON(http.StatusTooManyRequests, gin.H{
				"error":       ErrCodeAuthRateLimited,
				"message":     "Too many failed attempts. Try again later.",
				"retry_after": retryAfter,
			})
			return
		}
	}

	ctx := c.Request.Context()

	// Look up user
	user, err := s.store.GetUserByUsername(ctx, req.Username)
	if err != nil {
		s.authFailureTracker.recordFailure(req.Username)
		c.JSON(http.StatusUnauthorized, gin.H{
			"error":   "invalid_credentials",
			"message": "Invalid username or current password",
		})
		return
	}

	// Verify current password
	valid, err := crypto.VerifyPassword(user.PasswordHash, req.CurrentPassword)
	if err != nil || !valid {
		s.authFailureTracker.recordFailure(req.Username)
		c.JSON(http.StatusUnauthorized, gin.H{
			"error":   "invalid_credentials",
			"message": "Invalid username or current password",
		})
		return
	}

	// Reset failure count on successful credential verification
	s.authFailureTracker.resetFailures(req.Username)

	// Validate new password strength
	if len(req.NewPassword) < minPasswordLength {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   "weak_password",
			"message": "Password must be at least 8 characters",
		})
		return
	}

	// Hash new password
	newHash, err := crypto.HashPassword(req.NewPassword)
	if err != nil {
		s.logger.ErrorContext(ctx, "failed to hash password", slog.Any("error", err))
		errorResponse(c, http.StatusInternalServerError, "failed to change password")
		return
	}

	// Update password
	if err := s.store.UpdateUser(ctx, user.UID, store.UserUpdate{PasswordHash: &newHash}); err != nil {
		s.logger.ErrorContext(ctx, "failed to update password", slog.Any("error", err))
		errorResponse(c, http.StatusInternalServerError, "failed to change password")
		return
	}

	s.logger.InfoContext(ctx, "pre-login password changed", slog.String("user", user.Username), slog.Any("uid", user.UID))

	c.JSON(http.StatusOK, gin.H{"message": "Password changed successfully"})
}

// handleChangePassword changes a user's password
// Requires re-authentication via username/password in the request body (NOT Bearer token)
// - User can only change their own password (:uid must match authenticated user)
// - Admin users can change any user's password (with their own admin credentials)
// PUT /api/v1/users/:uid/password
//
//nolint:funlen,nestif // Authentication handlers require comprehensive validation which increases length
func (s *Server) handleChangePassword(c *gin.Context) {
	targetUID, err := parseUIDParam(c)
	if err != nil {
		errorResponse(c, http.StatusBadRequest, "invalid user ID")
		return
	}

	var req ChangePasswordRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		errorResponse(c, http.StatusBadRequest, "current_password and new_password are required")
		return
	}

	ctx := c.Request.Context()

	// Determine the authenticating user:
	// - If username is provided, look up by username (admin changing another user's password)
	// - If username is not provided, look up by target UID (user changing their own password)
	var authUser *store.User
	var rateLimitKey string

	if req.Username != "" {
		// Username provided - authenticate via username
		rateLimitKey = req.Username

		// Check rate limit BEFORE verifying credentials
		if !s.isTestMode() {
			if allowed, retryAfter := s.authFailureTracker.checkRateLimit(rateLimitKey); !allowed {
				c.Header("Retry-After", strconv.Itoa(retryAfter))
				c.JSON(http.StatusTooManyRequests, gin.H{
					"error":       ErrCodeAuthRateLimited,
					"message":     "Too many failed attempts. Try again later.",
					"retry_after": retryAfter,
				})
				return
			}
		}

		var err error
		authUser, err = s.store.GetUserByUsername(ctx, req.Username)
		if err != nil {
			s.authFailureTracker.recordFailure(rateLimitKey)
			c.JSON(http.StatusUnauthorized, gin.H{
				"error":   "invalid_credentials",
				"message": "Invalid username or current password",
			})
			return
		}
	} else {
		// No username - user is changing their own password, use target UID
		rateLimitKey = targetUID.String()

		// Check rate limit BEFORE verifying credentials
		if !s.isTestMode() {
			if allowed, retryAfter := s.authFailureTracker.checkRateLimit(rateLimitKey); !allowed {
				c.Header("Retry-After", strconv.Itoa(retryAfter))
				c.JSON(http.StatusTooManyRequests, gin.H{
					"error":       ErrCodeAuthRateLimited,
					"message":     "Too many failed attempts. Try again later.",
					"retry_after": retryAfter,
				})
				return
			}
		}

		var err error
		authUser, err = s.store.GetUserByUID(ctx, targetUID)
		if err != nil {
			s.authFailureTracker.recordFailure(rateLimitKey)
			c.JSON(http.StatusUnauthorized, gin.H{
				"error":   "invalid_credentials",
				"message": "Invalid current password",
			})
			return
		}
	}

	// Verify the authenticating user's password
	valid, err := crypto.VerifyPassword(authUser.PasswordHash, req.CurrentPassword)
	if err != nil || !valid {
		s.authFailureTracker.recordFailure(rateLimitKey)
		c.JSON(http.StatusUnauthorized, gin.H{
			"error":   "invalid_credentials",
			"message": "Invalid current password",
		})
		return
	}

	// Reset failure count on successful credential verification
	s.authFailureTracker.resetFailures(rateLimitKey)

	// Get the target user whose password will be changed
	// If no username was provided, authUser IS the target user (already looked up by targetUID)
	var targetUser *store.User
	if req.Username == "" {
		targetUser = authUser
	} else {
		targetUser, err = s.store.GetUserByUID(ctx, targetUID)
		if err != nil {
			errorResponse(c, http.StatusNotFound, "user not found")
			return
		}
	}

	// Check authorization:
	// - Users can only change their own password
	// - Admins can change any user's password
	if authUser.UID != targetUID && !authUser.IsAdmin() {
		c.JSON(http.StatusForbidden, gin.H{
			"error":   "forbidden",
			"message": "You can only change your own password",
		})
		return
	}

	// Validate new password strength
	if len(req.NewPassword) < minPasswordLength {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   "weak_password",
			"message": "Password must be at least 8 characters",
		})
		return
	}

	// Hash new password
	newHash, err := crypto.HashPassword(req.NewPassword)
	if err != nil {
		s.logger.ErrorContext(ctx, "failed to hash password", slog.Any("error", err))
		errorResponse(c, http.StatusInternalServerError, "failed to change password")
		return
	}

	// Update password
	if err := s.store.UpdateUser(ctx, targetUID, store.UserUpdate{PasswordHash: &newHash}); err != nil {
		s.logger.ErrorContext(ctx, "failed to update password", slog.Any("error", err))
		errorResponse(c, http.StatusInternalServerError, "failed to change password")
		return
	}

	s.logger.InfoContext(ctx, "password changed", slog.String("auth_user", authUser.Username), slog.String("target_user", targetUser.Username), slog.Any("target_uid", targetUID))

	c.JSON(http.StatusOK, gin.H{"message": "Password changed successfully"})
}

// ResetPasswordRequest represents the request body for admin password reset
type ResetPasswordRequest struct {
	NewPassword string `json:"new_password" binding:"required"`
}

// handleResetPassword allows admins to reset any user's password via web token
// This endpoint requires web session authentication (not API keys)
// POST /api/v1/users/:uid/reset-password
func (s *Server) handleResetPassword(c *gin.Context) {
	// 1. Get current user from context
	currentUser := getCurrentUser(c)
	if currentUser == nil {
		errorResponse(c, http.StatusUnauthorized, "authentication required")
		return
	}

	// 2. Verify web session (not API key)
	if isAPIKeyAuth(c) {
		errorResponse(c, http.StatusForbidden, "web session required for password reset")
		return
	}

	// 3. Verify admin role
	if !currentUser.IsAdmin() {
		errorResponse(c, http.StatusForbidden, "admin access required")
		return
	}

	// 4. Get target user UID
	targetUID, err := parseUIDParam(c)
	if err != nil {
		errorResponse(c, http.StatusBadRequest, "invalid user ID")
		return
	}

	// 5. Parse request body
	var req ResetPasswordRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		errorResponse(c, http.StatusBadRequest, "new_password is required")
		return
	}

	// 6. Validate password length
	if len(req.NewPassword) < minPasswordLength {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   "weak_password",
			"message": "Password must be at least 8 characters",
		})
		return
	}

	ctx := c.Request.Context()

	// 7. Get target user
	targetUser, err := s.store.GetUserByUID(ctx, targetUID)
	if err != nil {
		errorResponse(c, http.StatusNotFound, "user not found")
		return
	}

	// 8. Hash new password
	hashedPassword, err := crypto.HashPassword(req.NewPassword)
	if err != nil {
		s.logger.ErrorContext(ctx, "failed to hash password", slog.Any("error", err))
		errorResponse(c, http.StatusInternalServerError, "failed to reset password")
		return
	}

	// 9. Update password (this clears password_change_required since password is being set)
	if err := s.store.UpdateUser(ctx, targetUID, store.UserUpdate{PasswordHash: &hashedPassword}); err != nil {
		s.logger.ErrorContext(ctx, "failed to update password", slog.Any("error", err))
		errorResponse(c, http.StatusInternalServerError, "failed to reset password")
		return
	}

	// 10. Log the action
	s.logger.InfoContext(ctx, "password reset by admin",
		slog.String("admin_user", currentUser.Username),
		slog.String("target_user", targetUser.Username),
		slog.Any("target_uid", targetUID))

	c.JSON(http.StatusOK, gin.H{"message": "Password reset successfully"})
}
