package api

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/fclairamb/dbbat/internal/crypto"
	"github.com/fclairamb/dbbat/internal/store"
)

// CreateUserRequest represents the request to create a user
type CreateUserRequest struct {
	Username string   `json:"username" binding:"required"`
	Password string   `json:"password" binding:"required"`
	Roles    []string `json:"roles"`
}

// UpdateUserRequest represents the request to update a user
type UpdateUserRequest struct {
	Password *string  `json:"password"`
	Roles    []string `json:"roles"`
}

// handleCreateUser creates a new user
func (s *Server) handleCreateUser(c *gin.Context) {
	var req CreateUserRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		errorResponse(c, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}

	// Hash password
	passwordHash, err := crypto.HashPassword(req.Password)
	if err != nil {
		s.logger.ErrorContext(c.Request.Context(), "failed to hash password", slog.Any("error", err))
		errorResponse(c, http.StatusInternalServerError, "failed to create user")
		return
	}

	// Create user
	user, err := s.store.CreateUser(c.Request.Context(), req.Username, passwordHash, req.Roles)
	if err != nil {
		s.logger.ErrorContext(c.Request.Context(), "failed to create user", slog.Any("error", err))
		errorResponse(c, http.StatusInternalServerError, "failed to create user")
		return
	}

	// Log audit event
	currentUser := getCurrentUser(c)
	details, _ := json.Marshal(map[string]interface{}{
		"username": user.Username,
		"roles":    user.Roles,
	})
	_ = s.store.LogAuditEvent(c.Request.Context(), &store.AuditEvent{
		EventType:   "user.created",
		UserID:      &user.UID,
		PerformedBy: &currentUser.UID,
		Details:     details,
	})

	successResponse(c, user)
}

// handleListUsers lists users
// Admins and viewers see all users, others see only themselves
func (s *Server) handleListUsers(c *gin.Context) {
	currentUser := getCurrentUser(c)

	// Admins and viewers can see all users
	if currentUser.IsAdmin() || currentUser.IsViewer() {
		users, err := s.store.ListUsers(c.Request.Context())
		if err != nil {
			s.logger.ErrorContext(c.Request.Context(), "failed to list users", slog.Any("error", err))
			errorResponse(c, http.StatusInternalServerError, "failed to list users")
			return
		}
		successResponse(c, gin.H{"users": users})
		return
	}

	// Others can only see themselves
	successResponse(c, gin.H{"users": []any{currentUser}})
}

// handleGetUser retrieves a specific user
func (s *Server) handleGetUser(c *gin.Context) {
	uid, err := parseUIDParam(c)
	if err != nil {
		errorResponse(c, http.StatusBadRequest, "invalid user UID")
		return
	}

	user, err := s.store.GetUserByUID(c.Request.Context(), uid)
	if err != nil {
		s.logger.ErrorContext(c.Request.Context(), "failed to get user", slog.Any("error", err))
		errorResponse(c, http.StatusNotFound, "user not found")
		return
	}

	successResponse(c, user)
}

// handleUpdateUser updates a user
func (s *Server) handleUpdateUser(c *gin.Context) {
	uid, err := parseUIDParam(c)
	if err != nil {
		errorResponse(c, http.StatusBadRequest, "invalid user UID")
		return
	}

	var req UpdateUserRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		errorResponse(c, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}

	// API keys cannot change passwords (security restriction)
	if req.Password != nil && isAPIKeyAuth(c) {
		errorResponse(c, http.StatusForbidden, "password changes require password authentication")
		return
	}

	currentUser := getCurrentUser(c)

	// Non-admins can only update their own password
	if !currentUser.IsAdmin() {
		if uid != currentUser.UID {
			errorResponse(c, http.StatusForbidden, "can only update your own user")
			return
		}
		if len(req.Roles) > 0 {
			errorResponse(c, http.StatusForbidden, "cannot change roles")
			return
		}
	}

	updates := store.UserUpdate{
		Roles: req.Roles,
	}

	// Hash password if provided
	if req.Password != nil {
		passwordHash, err := crypto.HashPassword(*req.Password)
		if err != nil {
			s.logger.ErrorContext(c.Request.Context(), "failed to hash password", slog.Any("error", err))
			errorResponse(c, http.StatusInternalServerError, "failed to update user")
			return
		}
		updates.PasswordHash = &passwordHash
	}

	if err := s.store.UpdateUser(c.Request.Context(), uid, updates); err != nil {
		s.logger.ErrorContext(c.Request.Context(), "failed to update user", slog.Any("error", err))
		errorResponse(c, http.StatusInternalServerError, "failed to update user")
		return
	}

	// Log audit event
	details, _ := json.Marshal(map[string]interface{}{
		"updated_fields": req,
	})
	_ = s.store.LogAuditEvent(c.Request.Context(), &store.AuditEvent{
		EventType:   "user.updated",
		UserID:      &uid,
		PerformedBy: &currentUser.UID,
		Details:     details,
	})

	successResponse(c, gin.H{"message": "user updated"})
}

// handleDeleteUser deletes a user
func (s *Server) handleDeleteUser(c *gin.Context) {
	uid, err := parseUIDParam(c)
	if err != nil {
		errorResponse(c, http.StatusBadRequest, "invalid user UID")
		return
	}

	currentUser := getCurrentUser(c)

	// Prevent deleting yourself
	if uid == currentUser.UID {
		errorResponse(c, http.StatusBadRequest, "cannot delete your own user")
		return
	}

	if err := s.store.DeleteUser(c.Request.Context(), uid); err != nil {
		s.logger.ErrorContext(c.Request.Context(), "failed to delete user", slog.Any("error", err))
		errorResponse(c, http.StatusInternalServerError, "failed to delete user")
		return
	}

	// Log audit event
	details, _ := json.Marshal(map[string]interface{}{
		"user_uid": uid,
	})
	_ = s.store.LogAuditEvent(c.Request.Context(), &store.AuditEvent{
		EventType:   "user.deleted",
		UserID:      &uid,
		PerformedBy: &currentUser.UID,
		Details:     details,
	})

	successResponse(c, gin.H{"message": "user deleted"})
}
