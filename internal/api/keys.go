package api

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/fclairamb/dbbat/internal/store"
)

// CreateAPIKeyRequest represents the request to create an API key
type CreateAPIKeyRequest struct {
	Name      string     `json:"name" binding:"required"`
	ExpiresAt *time.Time `json:"expires_at"`
}

// CreateAPIKeyResponse represents the response when creating an API key
type CreateAPIKeyResponse struct {
	ID                   uuid.UUID        `json:"id"`
	Name                 string           `json:"name"`
	Key                  string           `json:"key"` // Only returned once!
	KeyPrefix            string           `json:"key_prefix"`
	ExpiresAt            *time.Time       `json:"expires_at"`
	CreatedAt            time.Time        `json:"created_at"`
	Connections          []ConnectionInfo `json:"connections"`
	ConnectionsTruncated bool             `json:"connections_truncated"`
}

const maxConnectionsInResponse = 50

// handleCreateAPIKey creates a new API key for the authenticated user
// Requires Web Session or Basic Auth (API keys cannot create other API keys)
func (s *Server) handleCreateAPIKey(c *gin.Context) {
	var req CreateAPIKeyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, "invalid request: "+err.Error())
		return
	}

	currentUser := getCurrentUser(c)

	// Create API key
	apiKey, plainKey, err := s.store.CreateAPIKey(c.Request.Context(), currentUser.UID, req.Name, req.ExpiresAt, s.encryptionKey)
	if err != nil {
		writeInternalError(c, s.logger, err, "failed to create API key")
		return
	}

	// Log audit event
	details, _ := json.Marshal(map[string]interface{}{
		"key_name":   apiKey.Name,
		"key_prefix": apiKey.KeyPrefix,
		"user_id":    currentUser.UID,
		"expires_at": apiKey.ExpiresAt,
	})
	_ = s.store.LogAuditEvent(c.Request.Context(), &store.AuditEvent{
		EventType:   "api_key.created",
		UserID:      &currentUser.UID,
		PerformedBy: &currentUser.UID,
		Details:     details,
	})

	connections, truncated := s.buildConnectionsForUser(c.Request.Context(), currentUser, plainKey)

	// Return the full key (only time it's shown)
	c.JSON(http.StatusCreated, CreateAPIKeyResponse{
		ID:                   apiKey.ID,
		Name:                 apiKey.Name,
		Key:                  plainKey,
		KeyPrefix:            apiKey.KeyPrefix,
		ExpiresAt:            apiKey.ExpiresAt,
		CreatedAt:            apiKey.CreatedAt,
		Connections:          connections,
		ConnectionsTruncated: truncated,
	})
}

// handleListAPIKeys lists API keys
// Default for every caller (including admins): only the caller's own keys.
// Admin-only overrides of that default:
//   - all_users=true returns every user's keys
//   - user_id=<uuid> returns one specific user's keys (wins over all_users if
//     both are given)
//
// Non-admins can never see another user's keys: both query params are simply
// ignored for them.
// Web session keys are excluded from the list (they are internal)
func (s *Server) handleListAPIKeys(c *gin.Context) {
	currentUser := getCurrentUser(c)

	// Only show regular API keys, not web sessions
	apiKeyType := store.KeyTypeAPI
	filter := store.APIKeyFilter{
		IncludeAll: c.Query("include_all") == "true",
		KeyType:    &apiKeyType,
		UserID:     &currentUser.UID, // default: own keys only, for everyone
	}

	if currentUser.IsAdmin() {
		if c.Query("all_users") == "true" {
			filter.UserID = nil
		}
		if userIDStr := c.Query("user_id"); userIDStr != "" {
			userID, err := uuid.Parse(userIDStr)
			if err != nil {
				writeError(c, http.StatusBadRequest, ErrCodeValidationError, "invalid user_id")
				return
			}
			filter.UserID = &userID
		}
	}

	keys, err := s.store.ListAPIKeys(c.Request.Context(), filter)
	if err != nil {
		writeInternalError(c, s.logger, err, "failed to list API keys")
		return
	}

	successResponse(c, gin.H{"keys": keys})
}

// handleGetAPIKey retrieves a specific API key
func (s *Server) handleGetAPIKey(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, "invalid API key ID")
		return
	}

	apiKey, err := s.store.GetAPIKeyByID(c.Request.Context(), id)
	if err != nil {
		writeError(c, http.StatusNotFound, ErrCodeNotFound, "API key not found")
		return
	}

	// Non-admins can only see their own keys
	currentUser := getCurrentUser(c)
	if !currentUser.IsAdmin() && apiKey.UserID != currentUser.UID {
		writeError(c, http.StatusForbidden, ErrCodeForbidden, "access denied")
		return
	}

	successResponse(c, apiKey)
}

// handleRevokeAPIKey revokes an API key
// Requires Web Session or Basic Auth (API keys cannot revoke API keys)
func (s *Server) handleRevokeAPIKey(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, "invalid API key ID")
		return
	}

	currentUser := getCurrentUser(c)

	// Get the key first to check permissions and for audit logging
	apiKey, err := s.store.GetAPIKeyByID(c.Request.Context(), id)
	if err != nil {
		writeError(c, http.StatusNotFound, ErrCodeNotFound, "API key not found")
		return
	}

	// Non-admins can only revoke their own keys
	if !currentUser.IsAdmin() && apiKey.UserID != currentUser.UID {
		writeError(c, http.StatusForbidden, ErrCodeForbidden, "access denied")
		return
	}

	if err := s.store.RevokeAPIKey(c.Request.Context(), id, currentUser.UID); err != nil {
		writeInternalError(c, s.logger, err, "failed to revoke API key")
		return
	}

	// Log audit event
	details, _ := json.Marshal(map[string]interface{}{
		"key_name":   apiKey.Name,
		"key_prefix": apiKey.KeyPrefix,
		"revoked_by": currentUser.UID,
	})
	_ = s.store.LogAuditEvent(c.Request.Context(), &store.AuditEvent{
		EventType:   "api_key.revoked",
		UserID:      &apiKey.UserID,
		PerformedBy: &currentUser.UID,
		Details:     details,
	})

	c.Status(http.StatusNoContent)
}

// buildConnectionsForUser builds connection URLs for all databases the user has active grants on.
func (s *Server) buildConnectionsForUser(ctx context.Context, user *store.User, apiKey string) ([]ConnectionInfo, bool) {
	if s.config == nil {
		return []ConnectionInfo{}, false
	}

	pe, err := s.store.GetPublicEndpoints(ctx)
	if err != nil {
		return []ConnectionInfo{}, false
	}
	endpoints := store.ResolvePublicEndpoints(pe, s.config)

	grants, err := s.store.ListGrants(ctx, store.GrantFilter{UserID: &user.UID, ActiveOnly: true})
	if err != nil {
		return []ConnectionInfo{}, false
	}

	// Deduplicate database UIDs
	seen := make(map[uuid.UUID]bool)
	var dbUIDs []uuid.UUID
	for _, g := range grants {
		if !seen[g.DatabaseID] {
			seen[g.DatabaseID] = true
			dbUIDs = append(dbUIDs, g.DatabaseID)
		}
	}

	var connections []ConnectionInfo
	for _, dbUID := range dbUIDs {
		db, err := s.store.GetDatabaseByUID(ctx, dbUID)
		if err != nil {
			continue
		}
		info, ok := BuildConnectionURL(db, user, endpoints, apiKey)
		if ok {
			connections = append(connections, info)
		}
	}

	if connections == nil {
		connections = []ConnectionInfo{}
	}

	if len(connections) > maxConnectionsInResponse {
		return connections[:maxConnectionsInResponse], true
	}
	return connections, false
}
