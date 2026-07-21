package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/fclairamb/dbbat/internal/store"
)

// validateDefinitionScope checks that every scoped group and database uid
// actually exists, so an admin can't silently create a definition that is
// scoped to nothing (which would fail closed and look like a bug).
func (s *Server) validateDefinitionScope(ctx context.Context, req *CreateGrantDefinitionRequest) string {
	for _, groupUID := range req.GroupUIDs {
		if _, err := s.store.GetUserGroup(ctx, groupUID); err != nil {
			return "user group does not exist: " + groupUID.String()
		}
	}

	for _, dbUID := range req.DatabaseUIDs {
		target, err := s.store.GetServerByUID(ctx, dbUID)
		if err != nil {
			return "database does not exist: " + dbUID.String()
		}

		if target.IsSSH() {
			return "cannot scope a grant definition to an ssh server: " + dbUID.String()
		}
	}

	return ""
}

// CreateGrantDefinitionRequest is the JSON body for POST /grant-definitions.
type CreateGrantDefinitionRequest struct {
	Name                string   `json:"name" binding:"required"`
	Description         string   `json:"description"`
	DurationSeconds     int64    `json:"duration_seconds" binding:"required"`
	Controls            []string `json:"controls"`
	MaxQueryCounts      *int64   `json:"max_query_counts"`
	MaxBytesTransferred *int64   `json:"max_bytes_transferred"`
	// AutoApprove, when true, makes grant requests against this definition
	// skip the pending/admin-approval step and materialize the grant
	// instantly.
	AutoApprove bool `json:"auto_approve"`
	// GroupUIDs restricts the definition to members of these user groups.
	// Empty/omitted = every user, which is how every pre-scoping definition
	// keeps behaving.
	GroupUIDs []uuid.UUID `json:"group_uids"`
	// DatabaseUIDs restricts the definition to these databases.
	// Empty/omitted = every database.
	DatabaseUIDs []uuid.UUID `json:"database_uids"`
}

// UpdateGrantDefinitionRequest is the JSON body for PATCH
// /grant-definitions/:uid. The shape mirrors the create request — partial
// updates aren't worth the extra complexity for this small surface.
type UpdateGrantDefinitionRequest = CreateGrantDefinitionRequest

func validateDefinitionRequest(req *CreateGrantDefinitionRequest) string {
	if req.Name == "" {
		return "name is required"
	}

	if len(req.Name) > 64 {
		return "name must be at most 64 characters"
	}

	if req.DurationSeconds <= 0 {
		return "duration_seconds must be > 0"
	}

	const maxDuration = int64(30 * 24 * 3600) // 30 days
	if req.DurationSeconds > maxDuration {
		return "duration_seconds must be at most 30 days (2592000)"
	}

	for _, control := range req.Controls {
		valid := false

		for _, vc := range store.ValidControls {
			if control == vc {
				valid = true

				break
			}
		}

		if !valid {
			return "invalid control: " + control
		}
	}

	if req.MaxQueryCounts != nil && *req.MaxQueryCounts <= 0 {
		return "max_query_counts must be > 0 or omitted"
	}

	if req.MaxBytesTransferred != nil && *req.MaxBytesTransferred <= 0 {
		return "max_bytes_transferred must be > 0 or omitted"
	}

	return ""
}

// handleCreateGrantDefinition — admin-only.
func (s *Server) handleCreateGrantDefinition(c *gin.Context) {
	var req CreateGrantDefinitionRequest

	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, "invalid request: "+err.Error())

		return
	}

	if msg := validateDefinitionRequest(&req); msg != "" {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, msg)

		return
	}

	if msg := s.validateDefinitionScope(c.Request.Context(), &req); msg != "" {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, msg)

		return
	}

	currentUser := getCurrentUser(c)

	def := &store.GrantDefinition{
		Name:                req.Name,
		Description:         req.Description,
		DurationSeconds:     req.DurationSeconds,
		Controls:            req.Controls,
		MaxQueryCounts:      req.MaxQueryCounts,
		MaxBytesTransferred: req.MaxBytesTransferred,
		AutoApprove:         req.AutoApprove,
		GroupUIDs:           req.GroupUIDs,
		DatabaseUIDs:        req.DatabaseUIDs,
		CreatedBy:           currentUser.UID,
	}

	created, err := s.store.CreateGrantDefinition(c.Request.Context(), def)
	if err != nil {
		// A name collision against an existing active definition surfaces as
		// a unique-violation, mapped to a typed sentinel by the store; return
		// a 409 so the client can react rather than an opaque 500.
		if errors.Is(err, store.ErrGrantDefinitionDuplicate) {
			writeError(c, http.StatusConflict, ErrCodeDuplicateName, err.Error())

			return
		}
		writeInternalError(c, s.logger, err, "failed to create grant definition")

		return
	}

	details, _ := json.Marshal(map[string]any{
		"grant_definition_uid": created.UID,
		"name":                 created.Name,
		"duration_seconds":     created.DurationSeconds,
		"controls":             created.Controls,
		"auto_approve":         created.AutoApprove,
		"group_uids":           created.GroupUIDs,
		"database_uids":        created.DatabaseUIDs,
	})

	_ = s.store.LogAuditEvent(c.Request.Context(), &store.AuditEvent{
		EventType:   "grant_definition.created",
		PerformedBy: &currentUser.UID,
		Details:     details,
	})

	successResponse(c, created)
}

// handleListGrantDefinitions — any authenticated user. Non-admins see only
// active definitions; admins see all unless they pass active=true to filter.
func (s *Server) handleListGrantDefinitions(c *gin.Context) {
	currentUser := getCurrentUser(c)

	filter := store.GrantDefinitionFilter{}

	switch {
	case !currentUser.IsAdmin():
		filter.ActiveOnly = true
	case c.Query("active_only") == "true":
		filter.ActiveOnly = true
	}

	defs, err := s.store.ListGrantDefinitions(c.Request.Context(), filter)
	if err != nil {
		writeInternalError(c, s.logger, err, "failed to list grant definitions")

		return
	}

	// Admins manage every definition, so their listing stays unfiltered.
	// Everyone else only sees the definitions their groups put in scope —
	// invisible rather than greyed out.
	if !currentUser.IsAdmin() {
		groupUIDs, err := s.store.ListUserGroupUIDs(c.Request.Context(), currentUser.UID)
		if err != nil {
			writeInternalError(c, s.logger, err, "failed to list user groups")

			return
		}

		visible := make([]store.GrantDefinition, 0, len(defs))

		for i := range defs {
			if defs[i].AppliesToGroups(groupUIDs) {
				visible = append(visible, defs[i])
			}
		}

		defs = visible
	}

	successResponse(c, gin.H{"grant_definitions": defs})
}

// handleGetGrantDefinition — any authenticated user.
func (s *Server) handleGetGrantDefinition(c *gin.Context) {
	uid, err := parseUIDParam(c)
	if err != nil {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, "invalid grant definition UID")

		return
	}

	def, err := s.store.GetGrantDefinition(c.Request.Context(), uid)
	if err != nil {
		if errors.Is(err, store.ErrGrantDefinitionNotFound) {
			writeError(c, http.StatusNotFound, ErrCodeNotFound, "grant definition not found")

			return
		}

		writeInternalError(c, s.logger, err, "failed to get grant definition")

		return
	}

	currentUser := getCurrentUser(c)

	if !currentUser.IsAdmin() {
		if !def.IsActive {
			// Hide deactivated definitions from non-admins entirely; the
			// listing endpoint already filters them, so a direct GET should
			// match.
			writeError(c, http.StatusNotFound, ErrCodeNotFound, "grant definition not found")

			return
		}

		groupUIDs, err := s.store.ListUserGroupUIDs(c.Request.Context(), currentUser.UID)
		if err != nil {
			writeInternalError(c, s.logger, err, "failed to list user groups")

			return
		}

		// Out-of-scope definitions are invisible in the listing; a direct GET
		// must not be a way around that.
		if !def.AppliesToGroups(groupUIDs) {
			writeError(c, http.StatusNotFound, ErrCodeNotFound, "grant definition not found")

			return
		}
	}

	successResponse(c, def)
}

// handleUpdateGrantDefinition — admin-only.
func (s *Server) handleUpdateGrantDefinition(c *gin.Context) {
	uid, err := parseUIDParam(c)
	if err != nil {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, "invalid grant definition UID")

		return
	}

	var req UpdateGrantDefinitionRequest

	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, "invalid request: "+err.Error())

		return
	}

	if msg := validateDefinitionRequest(&req); msg != "" {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, msg)

		return
	}

	if msg := s.validateDefinitionScope(c.Request.Context(), &req); msg != "" {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, msg)

		return
	}

	def, err := s.store.GetGrantDefinition(c.Request.Context(), uid)
	if err != nil {
		if errors.Is(err, store.ErrGrantDefinitionNotFound) {
			writeError(c, http.StatusNotFound, ErrCodeNotFound, "grant definition not found")

			return
		}

		writeInternalError(c, s.logger, err, "failed to get grant definition")

		return
	}

	def.Name = req.Name
	def.Description = req.Description
	def.DurationSeconds = req.DurationSeconds
	def.Controls = req.Controls
	def.MaxQueryCounts = req.MaxQueryCounts
	def.MaxBytesTransferred = req.MaxBytesTransferred
	def.AutoApprove = req.AutoApprove
	def.GroupUIDs = req.GroupUIDs
	def.DatabaseUIDs = req.DatabaseUIDs

	if err := s.store.UpdateGrantDefinition(c.Request.Context(), def); err != nil {
		writeInternalError(c, s.logger, err, "failed to update grant definition")

		return
	}

	currentUser := getCurrentUser(c)

	details, _ := json.Marshal(map[string]any{
		"grant_definition_uid": def.UID,
		"name":                 def.Name,
		"auto_approve":         def.AutoApprove,
		"group_uids":           def.GroupUIDs,
		"database_uids":        def.DatabaseUIDs,
	})

	_ = s.store.LogAuditEvent(c.Request.Context(), &store.AuditEvent{
		EventType:   "grant_definition.updated",
		PerformedBy: &currentUser.UID,
		Details:     details,
	})

	successResponse(c, def)
}

// handleDeactivateGrantDefinition — admin-only soft delete.
func (s *Server) handleDeactivateGrantDefinition(c *gin.Context) {
	uid, err := parseUIDParam(c)
	if err != nil {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, "invalid grant definition UID")

		return
	}

	if err := s.store.DeactivateGrantDefinition(c.Request.Context(), uid); err != nil {
		if errors.Is(err, store.ErrGrantDefinitionNotFound) {
			writeError(c, http.StatusNotFound, ErrCodeNotFound, "grant definition not found")

			return
		}

		writeInternalError(c, s.logger, err, "failed to deactivate grant definition")

		return
	}

	currentUser := getCurrentUser(c)

	details, _ := json.Marshal(map[string]any{
		"grant_definition_uid": uid,
	})

	_ = s.store.LogAuditEvent(c.Request.Context(), &store.AuditEvent{
		EventType:   "grant_definition.deactivated",
		PerformedBy: &currentUser.UID,
		Details:     details,
	})

	successResponse(c, gin.H{"message": "grant definition deactivated"})
}
