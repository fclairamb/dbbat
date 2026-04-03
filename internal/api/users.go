package api

import (
	"encoding/json"
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
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, "invalid request: "+err.Error())
		return
	}

	// Hash password
	passwordHash, err := crypto.HashPassword(req.Password)
	if err != nil {
		writeInternalError(c, s.logger, err, "failed to hash password")
		return
	}

	// Create user
	user, err := s.store.CreateUser(c.Request.Context(), req.Username, passwordHash, req.Roles)
	if err != nil {
		writeInternalError(c, s.logger, err, "failed to create user")
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
			writeInternalError(c, s.logger, err, "failed to list users")
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
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, "invalid user UID")
		return
	}

	user, err := s.store.GetUserByUID(c.Request.Context(), uid)
	if err != nil {
		writeError(c, http.StatusNotFound, ErrCodeNotFound, "user not found")
		return
	}

	successResponse(c, user)
}

// handleUpdateUser updates a user
func (s *Server) handleUpdateUser(c *gin.Context) {
	uid, err := parseUIDParam(c)
	if err != nil {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, "invalid user UID")
		return
	}

	var req UpdateUserRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, "invalid request: "+err.Error())
		return
	}

	// API keys cannot change passwords (security restriction)
	if req.Password != nil && isAPIKeyAuth(c) {
		writeError(c, http.StatusForbidden, ErrCodeForbidden, "password changes require password authentication")
		return
	}

	currentUser := getCurrentUser(c)

	// Non-admins can only update their own password
	if !currentUser.IsAdmin() {
		if uid != currentUser.UID {
			writeError(c, http.StatusForbidden, ErrCodeForbidden, "can only update your own user")
			return
		}
		if len(req.Roles) > 0 {
			writeError(c, http.StatusForbidden, ErrCodeForbidden, "cannot change roles")
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
			writeInternalError(c, s.logger, err, "failed to hash password")
			return
		}
		updates.PasswordHash = &passwordHash
	}

	if err := s.store.UpdateUser(c.Request.Context(), uid, updates); err != nil {
		writeInternalError(c, s.logger, err, "failed to update user")
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
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, "invalid user UID")
		return
	}

	currentUser := getCurrentUser(c)

	// Prevent deleting yourself
	if uid == currentUser.UID {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, "cannot delete your own user")
		return
	}

	// In demo mode, prevent deleting the admin user
	if s.config != nil && s.config.IsDemoMode() {
		userToDelete, err := s.store.GetUserByUID(c.Request.Context(), uid)
		if err != nil {
			writeError(c, http.StatusNotFound, ErrCodeNotFound, "user not found")
			return
		}

		if userToDelete.Username == "admin" {
			writeError(c, http.StatusForbidden, ErrCodeForbidden, "cannot delete admin user in demo mode")
			return
		}
	}

	if err := s.store.DeleteUser(c.Request.Context(), uid); err != nil {
		writeInternalError(c, s.logger, err, "failed to delete user")
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
