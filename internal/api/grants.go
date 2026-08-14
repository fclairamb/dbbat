package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/fclairamb/dbbat/internal/store"
)

// AssignGrantRequest is the body for POST /grants: an admin issuing a grant
// directly, without the user having to file a request.
//
// There is deliberately no way to describe the grant's *shape* here. A grant
// is an instance of a grant definition and nothing else; ad-hoc grants — an
// admin typing controls and quotas into a form — are what made definitions
// untrustworthy as the policy source of truth, so the shape comes from the
// definition or the grant does not exist.
type AssignGrantRequest struct {
	// GrantDefinitionID identifies the definition to instantiate — either its
	// uid or its slug, resolved the same way every other definition reference
	// is. A slug always resolves to the live version.
	GrantDefinitionID string    `json:"grant_definition_id" binding:"required"`
	UserID            uuid.UUID `json:"user_id" binding:"required"`
	DatabaseID        uuid.UUID `json:"database_id" binding:"required"`
	// StartsAt defaults to now. The window's *length* is the definition's
	// duration_seconds and is not negotiable here — that is part of the shape.
	StartsAt *time.Time `json:"starts_at"`
}

// handleAssignGrant issues a grant to a user by instantiating a grant
// definition — the admin-initiated equivalent of an approved grant request,
// and the only way a grant is created outside that flow.
func (s *Server) handleAssignGrant(c *gin.Context) {
	var req AssignGrantRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, "invalid request: "+err.Error())
		return
	}

	ctx := c.Request.Context()

	// Issuing always happens from the live version of the lineage: an edit
	// exists precisely to change what gets issued from here on.
	def, ok := s.resolveLiveGrantDefinition(c, req.GrantDefinitionID)
	if !ok {
		return
	}

	if !def.IsActive {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, "grant definition is not active")

		return
	}

	if _, err := s.store.GetUserByUID(ctx, req.UserID); err != nil {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, "user does not exist")

		return
	}

	// The target must be a database, never an SSH bastion (a dial path).
	target, err := s.store.GetServerByUID(ctx, req.DatabaseID)
	if err != nil {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, "database does not exist")

		return
	}

	if target.IsTunnel() {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, "cannot grant access to a "+target.Protocol+" tunnel server")

		return
	}

	// The definition's server-group scope is enforced: a shape declared to
	// apply only to certain servers must not authorize another one, whoever is
	// issuing it. Its *user-group* scope is not — that scope governs who may
	// self-request the definition, and an admin assigning access is the
	// authority on who gets it.
	serverGroupUIDs, err := s.store.ListServerGroupUIDsForServer(ctx, req.DatabaseID)
	if err != nil {
		writeInternalError(c, s.logger, err, "failed to list server groups")

		return
	}

	if !def.AppliesToServerGroups(serverGroupUIDs) {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError,
			"this grant definition cannot be used for this database")

		return
	}

	// "Starts now" means the store's now, not this process's: the auth path
	// admits a session with `starts_at <= NOW()` evaluated by PostgreSQL, so a
	// window opened from an API server whose clock runs ahead of the store is
	// refused by every proxy until the skew elapses. An explicitly requested
	// start is taken as given — that one is the caller's wall clock by
	// definition.
	startsAt, err := s.store.Now(ctx)
	if err != nil {
		writeInternalError(c, s.logger, err, "failed to read the database clock")

		return
	}

	if req.StartsAt != nil {
		startsAt = *req.StartsAt
	}

	currentUser := getCurrentUser(c)

	result, err := s.store.CreateGrant(ctx,
		store.BuildGrantFromDefinition(def, req.UserID, req.DatabaseID, currentUser.UID, startsAt))
	if err != nil {
		writeInternalError(c, s.logger, err, "failed to create grant")
		return
	}

	// Log audit event
	details, _ := json.Marshal(map[string]interface{}{
		"grant_uid":           result.UID,
		"user_id":             result.UserID,
		"database_id":         result.DatabaseID,
		"grant_definition_id": result.GrantDefinitionID,
		"controls":            result.Controls(),
		"starts_at":           result.StartsAt,
		"expires_at":          result.ExpiresAt,
		"priority":            result.Priority,
	})
	_ = s.store.LogAuditEvent(ctx, &store.AuditEvent{
		EventType:   "grant.created",
		UserID:      &result.UserID,
		PerformedBy: &currentUser.UID,
		Details:     details,
	})

	successResponse(c, result)
}

// handleListGrants lists grants with optional filters based on user role
func (s *Server) handleListGrants(c *gin.Context) {
	currentUser := getCurrentUser(c)
	filter := store.GrantFilter{}

	// Parse query parameters
	if userID := c.Query("user_id"); userID != "" {
		if uid, err := uuid.Parse(userID); err == nil {
			filter.UserID = &uid
		}
	}

	if databaseID := c.Query("database_id"); databaseID != "" {
		if uid, err := uuid.Parse(databaseID); err == nil {
			filter.DatabaseID = &uid
		}
	}

	if c.Query("active_only") == "true" {
		filter.ActiveOnly = true
	}

	// Connector can only see their own grants
	if !currentUser.IsAdmin() && !currentUser.IsViewer() {
		filter.UserID = &currentUser.UID
	}

	grants, err := s.store.ListGrants(c.Request.Context(), filter)
	if err != nil {
		writeInternalError(c, s.logger, err, "failed to list grants")
		return
	}

	successResponse(c, gin.H{"grants": grants})
}

// handleGetGrant retrieves a specific grant based on user role
func (s *Server) handleGetGrant(c *gin.Context) {
	uid, err := parseUIDParam(c)
	if err != nil {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, "invalid grant UID")
		return
	}

	currentUser := getCurrentUser(c)

	grant, err := s.store.GetGrantByUID(c.Request.Context(), uid)
	if err != nil {
		writeError(c, http.StatusNotFound, ErrCodeNotFound, "grant not found")
		return
	}

	// Connector can only see their own grants
	if !currentUser.IsAdmin() && !currentUser.IsViewer() {
		if grant.UserID != currentUser.UID {
			writeError(c, http.StatusForbidden, ErrCodeForbidden, "no access to this grant")
			return
		}
	}

	successResponse(c, grant)
}

// handleRevokeGrant revokes a grant
func (s *Server) handleRevokeGrant(c *gin.Context) {
	uid, err := parseUIDParam(c)
	if err != nil {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, "invalid grant UID")
		return
	}

	currentUser := getCurrentUser(c)
	if err := s.store.RevokeGrant(c.Request.Context(), uid, currentUser.UID); err != nil {
		writeInternalError(c, s.logger, err, "failed to revoke grant")
		return
	}

	// Signal any live proxy sessions authenticated under this grant so they
	// stop accepting queries and disconnect, rather than staying usable until
	// their next reconnect. Purely in-process; a no-op when nothing is live.
	if signaled := s.store.Revocations().Revoke(uid); signaled > 0 {
		s.logger.InfoContext(c.Request.Context(), "grant revoked: signaled live sessions",
			slog.String("grant_uid", uid.String()),
			slog.Int("sessions", signaled))
	}

	// Log audit event
	grant, _ := s.store.GetGrantByUID(c.Request.Context(), uid)
	var userID *uuid.UUID
	if grant != nil {
		userID = &grant.UserID
	}
	details, _ := json.Marshal(map[string]interface{}{
		"grant_uid": uid,
	})
	_ = s.store.LogAuditEvent(c.Request.Context(), &store.AuditEvent{
		EventType:   "grant.revoked",
		UserID:      userID,
		PerformedBy: &currentUser.UID,
		Details:     details,
	})

	successResponse(c, gin.H{"message": "grant revoked"})
}
