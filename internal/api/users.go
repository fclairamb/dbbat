package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

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
	// UserGroupUIDs, when non-nil, replaces the user's user-group memberships
	// wholesale. Admin-only, like Roles.
	UserGroupUIDs []uuid.UUID `json:"user_group_uids"`
	// RetiredGroupUIDs catches the pre-rename spelling of UserGroupUIDs. It
	// is refused rather than folded onto UserGroupUIDs — silently ignoring a
	// scope restriction fails *open*. See errRetiredGroupUIDs.
	RetiredGroupUIDs []uuid.UUID `json:"group_uids"`
}

// setMongoVerifier derives and stores the user's MongoDB SCRAM-SHA-256 verifier
// from their new plaintext password, so they can authenticate to the MongoDB
// proxy with the driver-default SCRAM-SHA-256 instead of PLAIN. It is a
// best-effort optimisation layered on top of the Argon2id password hash: any
// failure is logged but never fails the password change (PLAIN stays available).
func (s *Server) setMongoVerifier(c *gin.Context, userID uuid.UUID, password string) {
	if err := s.store.SetUserMongoVerifier(c.Request.Context(), userID, password, s.encryptionKey); err != nil {
		s.logger.WarnContext(c.Request.Context(), "failed to store MongoDB SCRAM verifier",
			"user_uid", userID, "error", err)
	}
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
		if errors.Is(err, store.ErrUserNameConflict) {
			writeError(c, http.StatusConflict, ErrCodeDuplicateName, err.Error())
			return
		}
		writeInternalError(c, s.logger, err, "failed to create user")
		return
	}

	// Store the MongoDB SCRAM verifier so the user can use SCRAM-SHA-256.
	s.setMongoVerifier(c, user.UID, req.Password)

	// Log audit event
	currentUser := getCurrentUser(c)
	details, _ := json.Marshal(map[string]interface{}{
		"username": user.Username,
		"roles":    user.Roles,
	})
	_ = s.store.LogAuditEvent(c.Request.Context(), &store.AuditEvent{
		EventType:   AuditEventUserCreated,
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

// handleListRoleSyncs returns the latest directory role sync per user.
//
// One row per user, computed in the database, rather than a slice of the audit
// log filtered client-side: on an instance where hundreds of people sign in
// through SSO, a user whose last sync fell outside such a slice would render as
// "never synced" — the same thing shown for a user the directory has genuinely
// never touched. Absent from this response is therefore the *exact* meaning of
// "never synced".
//
// Admin-or-viewer, matching GET /audit, which is where these rows are read
// from and where the same entries can be listed one by one.
func (s *Server) handleListRoleSyncs(c *gin.Context) {
	syncs, err := s.store.ListLatestEventPerUser(c.Request.Context(), AuditEventOAuthRolesSynced)
	if err != nil {
		writeInternalError(c, s.logger, err, "failed to list role syncs")
		return
	}

	successResponse(c, gin.H{"role_syncs": syncs})
}

// handleGetUser retrieves a specific user.
//
// Visibility matches handleListUsers: admins and viewers see everyone, anyone
// else sees only themselves. A foreign UID is answered with the same 404 as a
// UID that does not exist, so a connector cannot use this endpoint to
// enumerate who has an account — a 403 would confirm the user is real.
func (s *Server) handleGetUser(c *gin.Context) {
	uid, err := parseUIDParam(c)
	if err != nil {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, "invalid user UID")
		return
	}

	currentUser := getCurrentUser(c)
	if currentUser == nil {
		writeError(c, http.StatusUnauthorized, ErrCodeUnauthorized, "authentication required")
		return
	}

	if !currentUser.IsAdmin() && !currentUser.IsViewer() && currentUser.UID != uid {
		writeError(c, http.StatusNotFound, ErrCodeNotFound, "user not found")
		return
	}

	user, err := s.store.GetUserByUID(c.Request.Context(), uid)
	if err != nil {
		writeError(c, http.StatusNotFound, ErrCodeNotFound, "user not found")
		return
	}

	// Groups ride along on the detail response so the user editor can render
	// (and edit) membership without a second round-trip.
	groups, err := s.store.ListUserGroupsForUser(c.Request.Context(), uid)
	if err != nil {
		writeInternalError(c, s.logger, err, "failed to list user groups")
		return
	}

	successResponse(c, userDetailResponse{User: user, Groups: groups})
}

// userDetailResponse is a user plus the groups they belong to.
type userDetailResponse struct {
	*store.User

	Groups []store.UserGroup `json:"user_groups"`
}

// bindUpdateUserRequest decodes the PUT /users/:uid body and rejects it up
// front when it is malformed or still carries the retired pre-rename
// group_uids spelling — refusing rather than folding, since silently
// ignoring a membership change would fail *open*. Returns false, having
// already written the error response, when the update must not proceed.
func (s *Server) bindUpdateUserRequest(c *gin.Context, req *UpdateUserRequest) bool {
	if err := c.ShouldBindJSON(req); err != nil {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, "invalid request: "+err.Error())
		return false
	}

	if req.RetiredGroupUIDs != nil {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, errRetiredGroupUIDs)
		return false
	}

	return true
}

// handleUpdateUser updates a user
func (s *Server) handleUpdateUser(c *gin.Context) {
	uid, err := parseUIDParam(c)
	if err != nil {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, "invalid user UID")
		return
	}

	var req UpdateUserRequest
	if !s.bindUpdateUserRequest(c, &req) {
		return
	}

	// API keys cannot change passwords (security restriction)
	if req.Password != nil && isAPIKeyAuth(c) {
		writeError(c, http.StatusForbidden, ErrCodeForbidden, "password changes require password authentication")
		return
	}

	// In demo mode, prevent changing the admin user's password
	if req.Password != nil && s.config != nil && s.config.IsDemoMode() {
		targetUser, err := s.store.GetUserByUID(c.Request.Context(), uid)
		if err == nil && targetUser.Username == "admin" {
			writeError(c, http.StatusForbidden, ErrCodeForbidden, "cannot change admin password in demo mode")
			return
		}
	}

	currentUser := getCurrentUser(c)

	if !s.checkSelfUpdateAllowed(c, uid, currentUser, &req) {
		return
	}

	if !s.checkGroupsExist(c, req.UserGroupUIDs) {
		return
	}

	// Prevent a roles update that would leave the instance without any admin
	if s.rejectLastAdminDemotion(c, uid, req.Roles) {
		return
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

	if req.UserGroupUIDs != nil {
		if err := s.store.SetUserGroups(c.Request.Context(), uid, req.UserGroupUIDs); err != nil {
			writeInternalError(c, s.logger, err, "failed to update user groups")
			return
		}
	}

	// Refresh the MongoDB SCRAM verifier when the password changed.
	if req.Password != nil {
		s.setMongoVerifier(c, uid, *req.Password)
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

	// Membership is access-relevant (it gates grant definitions), so record
	// it as its own event rather than burying it in user.updated.
	if req.UserGroupUIDs != nil {
		groupDetails, _ := json.Marshal(map[string]interface{}{
			"user_uid":        uid,
			"user_group_uids": req.UserGroupUIDs,
		})
		_ = s.store.LogAuditEvent(c.Request.Context(), &store.AuditEvent{
			EventType:   "user_group.membership_set",
			UserID:      &uid,
			PerformedBy: &currentUser.UID,
			Details:     groupDetails,
		})
	}

	successResponse(c, gin.H{"message": "user updated"})
}

// checkSelfUpdateAllowed enforces the non-admin restrictions on a user
// update: you may only touch your own account, and only your password —
// roles and groups are both access-control levers, so both stay admin-only.
// Writes an error response and returns false when the update is refused.
func (s *Server) checkSelfUpdateAllowed(
	c *gin.Context,
	uid uuid.UUID,
	currentUser *store.User,
	req *UpdateUserRequest,
) bool {
	if currentUser.IsAdmin() {
		return true
	}

	if uid != currentUser.UID {
		writeError(c, http.StatusForbidden, ErrCodeForbidden, "can only update your own user")
		return false
	}

	if req.Roles != nil {
		writeError(c, http.StatusForbidden, ErrCodeForbidden, "cannot change roles")
		return false
	}

	if req.UserGroupUIDs != nil {
		writeError(c, http.StatusForbidden, ErrCodeForbidden, "cannot change groups")
		return false
	}

	return true
}

// checkGroupsExist writes an error response and returns false when any uid
// does not name a real group. Membership gates which grant definitions a user
// may request, so a bogus uid must not be silently persisted.
func (s *Server) checkGroupsExist(c *gin.Context, groupUIDs []uuid.UUID) bool {
	for _, groupUID := range groupUIDs {
		if _, err := s.store.GetUserGroup(c.Request.Context(), groupUID); err != nil {
			writeError(c, http.StatusBadRequest, ErrCodeValidationError,
				"user group does not exist: "+groupUID.String())
			return false
		}
	}

	return true
}

// rejectLastAdminDemotion writes an error response and returns true when the
// requested roles update would remove the admin role from the last remaining
// admin user (or when the target user cannot be loaded).
func (s *Server) rejectLastAdminDemotion(c *gin.Context, uid uuid.UUID, roles []string) bool {
	if roles == nil || slices.Contains(roles, store.RoleAdmin) {
		return false
	}

	targetUser, err := s.store.GetUserByUID(c.Request.Context(), uid)
	if err != nil {
		writeError(c, http.StatusNotFound, ErrCodeNotFound, "user not found")
		return true
	}

	last, err := s.isLastAdmin(c.Request.Context(), targetUser)
	if err != nil {
		writeInternalError(c, s.logger, err, "failed to count admin users")
		return true
	}

	if last {
		writeError(c, http.StatusConflict, ErrCodeConflict, "cannot remove the admin role from the last admin user")
		return true
	}

	return false
}

// isLastAdmin reports whether user is the only admin left, i.e. whether taking
// the admin role away would leave the instance with nobody able to administer
// it. Shared by the users API (which answers 409) and the OIDC group mapping
// (which silently retains the role), so the two can never drift apart on what
// "the last admin" means.
func (s *Server) isLastAdmin(ctx context.Context, user *store.User) (bool, error) {
	if !user.IsAdmin() {
		return false, nil
	}

	adminCount, err := s.store.CountAdmins(ctx)
	if err != nil {
		return false, fmt.Errorf("count admin users: %w", err)
	}

	return adminCount <= 1, nil
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

	userToDelete, err := s.store.GetUserByUID(c.Request.Context(), uid)
	if err != nil {
		writeError(c, http.StatusNotFound, ErrCodeNotFound, "user not found")
		return
	}

	// In demo mode, prevent deleting the admin user
	if s.config != nil && s.config.IsDemoMode() && userToDelete.Username == "admin" {
		writeError(c, http.StatusForbidden, ErrCodeForbidden, "cannot delete admin user in demo mode")
		return
	}

	// Prevent deleting the last remaining admin. This is defense in depth:
	// deletes require an admin actor and self-deletion is blocked, so the
	// actor is normally a second admin — but the invariant must never break.
	if userToDelete.IsAdmin() {
		adminCount, err := s.store.CountAdmins(c.Request.Context())
		if err != nil {
			writeInternalError(c, s.logger, err, "failed to count admin users")
			return
		}

		if adminCount <= 1 {
			writeError(c, http.StatusConflict, ErrCodeConflict, "cannot delete the last admin user")
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
