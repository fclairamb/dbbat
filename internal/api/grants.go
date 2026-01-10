package api

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/fclairamb/dbbat/internal/store"
)

// CreateGrantRequest represents the request to create a grant
type CreateGrantRequest struct {
	UserID              uuid.UUID `json:"user_id" binding:"required"`
	DatabaseID          uuid.UUID `json:"database_id" binding:"required"`
	AccessLevel         string    `json:"access_level" binding:"required"`
	StartsAt            time.Time `json:"starts_at" binding:"required"`
	ExpiresAt           time.Time `json:"expires_at" binding:"required"`
	MaxQueryCounts      *int64    `json:"max_query_counts"`
	MaxBytesTransferred *int64    `json:"max_bytes_transferred"`
}

// handleCreateGrant creates a new access grant
func (s *Server) handleCreateGrant(c *gin.Context) {
	var req CreateGrantRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		errorResponse(c, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}

	// Validate access level
	if req.AccessLevel != "read" && req.AccessLevel != "write" {
		errorResponse(c, http.StatusBadRequest, "access_level must be 'read' or 'write'")
		return
	}

	// Validate time window
	if !req.StartsAt.Before(req.ExpiresAt) {
		errorResponse(c, http.StatusBadRequest, "starts_at must be before expires_at")
		return
	}

	currentUser := getCurrentUser(c)
	grant := &store.Grant{
		UserID:              req.UserID,
		DatabaseID:          req.DatabaseID,
		AccessLevel:         req.AccessLevel,
		GrantedBy:           currentUser.UID,
		StartsAt:            req.StartsAt,
		ExpiresAt:           req.ExpiresAt,
		MaxQueryCounts:      req.MaxQueryCounts,
		MaxBytesTransferred: req.MaxBytesTransferred,
	}

	result, err := s.store.CreateGrant(c.Request.Context(), grant)
	if err != nil {
		s.logger.Error("failed to create grant", "error", err)
		errorResponse(c, http.StatusInternalServerError, "failed to create grant")
		return
	}

	// Log audit event
	details, _ := json.Marshal(map[string]interface{}{
		"grant_uid":    result.UID,
		"user_id":      result.UserID,
		"database_id":  result.DatabaseID,
		"access_level": result.AccessLevel,
		"starts_at":    result.StartsAt,
		"expires_at":   result.ExpiresAt,
	})
	_ = s.store.LogAuditEvent(c.Request.Context(), &store.AuditEvent{
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
		s.logger.Error("failed to list grants", "error", err)
		errorResponse(c, http.StatusInternalServerError, "failed to list grants")
		return
	}

	successResponse(c, gin.H{"grants": grants})
}

// handleGetGrant retrieves a specific grant based on user role
func (s *Server) handleGetGrant(c *gin.Context) {
	uid, err := parseUIDParam(c)
	if err != nil {
		errorResponse(c, http.StatusBadRequest, "invalid grant UID")
		return
	}

	currentUser := getCurrentUser(c)

	grant, err := s.store.GetGrantByUID(c.Request.Context(), uid)
	if err != nil {
		s.logger.Error("failed to get grant", "error", err)
		errorResponse(c, http.StatusNotFound, "grant not found")
		return
	}

	// Connector can only see their own grants
	if !currentUser.IsAdmin() && !currentUser.IsViewer() {
		if grant.UserID != currentUser.UID {
			errorResponse(c, http.StatusForbidden, "no access to this grant")
			return
		}
	}

	successResponse(c, grant)
}

// handleRevokeGrant revokes a grant
func (s *Server) handleRevokeGrant(c *gin.Context) {
	uid, err := parseUIDParam(c)
	if err != nil {
		errorResponse(c, http.StatusBadRequest, "invalid grant UID")
		return
	}

	currentUser := getCurrentUser(c)
	if err := s.store.RevokeGrant(c.Request.Context(), uid, currentUser.UID); err != nil {
		s.logger.Error("failed to revoke grant", "error", err)
		errorResponse(c, http.StatusInternalServerError, "failed to revoke grant")
		return
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
