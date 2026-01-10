package api

import (
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
	ID        uuid.UUID  `json:"id"`
	Name      string     `json:"name"`
	Key       string     `json:"key"` // Only returned once!
	KeyPrefix string     `json:"key_prefix"`
	ExpiresAt *time.Time `json:"expires_at"`
	CreatedAt time.Time  `json:"created_at"`
}

// handleCreateAPIKey creates a new API key for the authenticated user
// Requires Web Session or Basic Auth (API keys cannot create other API keys)
func (s *Server) handleCreateAPIKey(c *gin.Context) {
	var req CreateAPIKeyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		errorResponse(c, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}

	currentUser := getCurrentUser(c)

	// Create API key
	apiKey, plainKey, err := s.store.CreateAPIKey(c.Request.Context(), currentUser.UID, req.Name, req.ExpiresAt)
	if err != nil {
		s.logger.Error("failed to create API key", "error", err)
		errorResponse(c, http.StatusInternalServerError, "failed to create API key")
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

	// Return the full key (only time it's shown)
	c.JSON(http.StatusCreated, CreateAPIKeyResponse{
		ID:        apiKey.ID,
		Name:      apiKey.Name,
		Key:       plainKey,
		KeyPrefix: apiKey.KeyPrefix,
		ExpiresAt: apiKey.ExpiresAt,
		CreatedAt: apiKey.CreatedAt,
	})
}

// handleListAPIKeys lists API keys
// Non-admin users see only their own keys
// Admin users see all keys (can filter by user_id query param)
// Web session keys are excluded from the list (they are internal)
func (s *Server) handleListAPIKeys(c *gin.Context) {
	currentUser := getCurrentUser(c)

	// Only show regular API keys, not web sessions
	apiKeyType := store.KeyTypeAPI
	filter := store.APIKeyFilter{
		IncludeAll: c.Query("include_all") == "true",
		KeyType:    &apiKeyType,
	}

	// Non-admins can only see their own keys
	if !currentUser.IsAdmin() {
		filter.UserID = &currentUser.UID
	} else {
		// Admins can filter by user_id
		if userIDStr := c.Query("user_id"); userIDStr != "" {
			userID, err := uuid.Parse(userIDStr)
			if err != nil {
				errorResponse(c, http.StatusBadRequest, "invalid user_id")
				return
			}
			filter.UserID = &userID
		}
	}

	keys, err := s.store.ListAPIKeys(c.Request.Context(), filter)
	if err != nil {
		s.logger.Error("failed to list API keys", "error", err)
		errorResponse(c, http.StatusInternalServerError, "failed to list API keys")
		return
	}

	successResponse(c, gin.H{"keys": keys})
}

// handleGetAPIKey retrieves a specific API key
func (s *Server) handleGetAPIKey(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		errorResponse(c, http.StatusBadRequest, "invalid API key ID")
		return
	}

	apiKey, err := s.store.GetAPIKeyByID(c.Request.Context(), id)
	if err != nil {
		s.logger.Error("failed to get API key", "error", err)
		errorResponse(c, http.StatusNotFound, "API key not found")
		return
	}

	// Non-admins can only see their own keys
	currentUser := getCurrentUser(c)
	if !currentUser.IsAdmin() && apiKey.UserID != currentUser.UID {
		errorResponse(c, http.StatusForbidden, "access denied")
		return
	}

	successResponse(c, apiKey)
}

// handleRevokeAPIKey revokes an API key
// Requires Web Session or Basic Auth (API keys cannot revoke API keys)
func (s *Server) handleRevokeAPIKey(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		errorResponse(c, http.StatusBadRequest, "invalid API key ID")
		return
	}

	currentUser := getCurrentUser(c)

	// Get the key first to check permissions and for audit logging
	apiKey, err := s.store.GetAPIKeyByID(c.Request.Context(), id)
	if err != nil {
		s.logger.Error("failed to get API key", "error", err)
		errorResponse(c, http.StatusNotFound, "API key not found")
		return
	}

	// Non-admins can only revoke their own keys
	if !currentUser.IsAdmin() && apiKey.UserID != currentUser.UID {
		errorResponse(c, http.StatusForbidden, "access denied")
		return
	}

	if err := s.store.RevokeAPIKey(c.Request.Context(), id, currentUser.UID); err != nil {
		s.logger.Error("failed to revoke API key", "error", err)
		errorResponse(c, http.StatusInternalServerError, "failed to revoke API key")
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
