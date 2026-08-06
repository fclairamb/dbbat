package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/fclairamb/dbbat/internal/store"
)

// slugMaxLength bounds how long a grant definition slug may be — long enough
// for a descriptive handle, short enough to stay comfortable in a CLI
// invocation or a URL path segment.
const slugMaxLength = 64

// slugPattern is the accepted shape for a grant definition slug: lowercase
// alphanumeric segments separated by single hyphens. No leading/trailing/
// doubled hyphens, no uppercase, no underscores or spaces — the same rules
// as most URL-safe slug conventions, chosen so the slug is always safe to
// drop into a path segment or a shell argument unquoted.
var slugPattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// resolveGrantDefinition looks up a grant definition by either its uid or
// its slug. A syntactically UUID-shaped identifier is looked up as a uid
// only — slugs can never be UUID-shaped (validateDefinitionRequest rejects
// them), so there is no ambiguity to fall back through: a UUID-shaped
// param that misses as a uid is simply not found. Anything else is looked
// up as a slug.
func (s *Server) resolveGrantDefinition(ctx context.Context, idOrSlug string) (*store.GrantDefinition, error) {
	if uid, err := uuid.Parse(idOrSlug); err == nil {
		return s.store.GetGrantDefinition(ctx, uid)
	}

	return s.store.GetGrantDefinitionBySlug(ctx, idOrSlug)
}

// validateDefinitionScope checks that every scoped group and database uid
// actually exists, so an admin can't silently create a definition that is
// scoped to nothing (which would fail closed and look like a bug).
func (s *Server) validateDefinitionScope(ctx context.Context, req *CreateGrantDefinitionRequest) string {
	for _, groupUID := range req.GroupUIDs {
		if _, err := s.store.GetUserGroup(ctx, groupUID); err != nil {
			return "user group does not exist: " + groupUID.String()
		}
	}

	for _, groupUID := range req.ApproverGroupUIDs {
		if _, err := s.store.GetUserGroup(ctx, groupUID); err != nil {
			return "approver group does not exist: " + groupUID.String()
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
	Name string `json:"name" binding:"required"`
	// Slug is a stable, human-typeable, machine-friendly identifier for this
	// definition — mandatory at the API level. The server never generates
	// one; that's the frontend's job (derive-from-name until the operator
	// edits it manually), which keeps the API contract explicit for CLI and
	// agent callers.
	Slug                string   `json:"slug" binding:"required"`
	Description         string   `json:"description"`
	DurationSeconds     int64    `json:"duration_seconds" binding:"required"`
	Controls            []string `json:"controls"`
	MaxQueryCounts      *int64   `json:"max_query_counts"`
	MaxBytesTransferred *int64   `json:"max_bytes_transferred"`
	// Priority, when supplied, is stamped verbatim on every grant
	// materialized from this definition instead of the tier its controls
	// would earn. null/omitted — the normal case — leaves it auto.
	Priority *int16 `json:"priority"`
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
	// ApprovalPatterns are RE2 patterns that suspend a matching statement
	// until a second human approves it. Empty/omitted = no approval gating.
	// Compiled here so a bad pattern is a 400 rather than a runtime surprise
	// on the proxy hot path.
	ApprovalPatterns []string `json:"approval_patterns"`
	// ApproverGroupUIDs lists groups whose members may resolve those holds,
	// in addition to admins. Empty/omitted = admins only.
	ApproverGroupUIDs []uuid.UUID `json:"approver_group_uids"`
}

// UpdateGrantDefinitionRequest is the JSON body for PATCH
// /grant-definitions/:uid. Unlike CreateGrantDefinitionRequest, every field
// is optional and a field absent from the body is left untouched on the
// existing definition rather than reset to its zero value. This is what
// makes a targeted PATCH — like the auto-approve toggle, which only means
// to flip one field — safe: it used to reuse CreateGrantDefinitionRequest
// as a full-replace body, so any field the caller didn't round-trip (most
// notably approval_patterns and approver_group_uids) got silently wiped.
//
// Slice fields (controls, group_uids, database_uids, approval_patterns,
// approver_group_uids) distinguish "absent" from "explicitly cleared":
// encoding/json leaves a slice field nil when its key is missing (or its
// value is JSON null), but allocates a non-nil empty slice for a present
// `[]`. Only a non-nil slice is applied, so an admin can still empty one of
// these lists via PATCH without sending anything else.
//
// max_query_counts, max_bytes_transferred and priority are themselves
// nullable in the domain (null means "no limit" / "auto"), so a plain
// pointer can't tell "absent" from "explicit null" — both decode to a nil
// pointer. Each gets a companion Clear* flag instead, mirroring
// UpdateDatabaseRequest.ClearViaUID.
type UpdateGrantDefinitionRequest struct {
	Name                     *string     `json:"name"`
	Slug                     *string     `json:"slug"`
	Description              *string     `json:"description"`
	DurationSeconds          *int64      `json:"duration_seconds"`
	Controls                 []string    `json:"controls"`
	MaxQueryCounts           *int64      `json:"max_query_counts"`
	ClearMaxQueryCounts      bool        `json:"clear_max_query_counts"`
	MaxBytesTransferred      *int64      `json:"max_bytes_transferred"`
	ClearMaxBytesTransferred bool        `json:"clear_max_bytes_transferred"`
	Priority                 *int16      `json:"priority"`
	ClearPriority            bool        `json:"clear_priority"`
	AutoApprove              *bool       `json:"auto_approve"`
	GroupUIDs                []uuid.UUID `json:"group_uids"`
	DatabaseUIDs             []uuid.UUID `json:"database_uids"`
	ApprovalPatterns         []string    `json:"approval_patterns"`
	ApproverGroupUIDs        []uuid.UUID `json:"approver_group_uids"`
}

// applyGrantDefinitionUpdate copies every field present in req onto def,
// leaving fields absent from the request body untouched. See
// UpdateGrantDefinitionRequest for how presence is detected per field kind.
func applyGrantDefinitionUpdate(def *store.GrantDefinition, req *UpdateGrantDefinitionRequest) {
	if req.Name != nil {
		def.Name = *req.Name
	}

	if req.Slug != nil {
		def.Slug = *req.Slug
	}

	if req.Description != nil {
		def.Description = *req.Description
	}

	if req.DurationSeconds != nil {
		def.DurationSeconds = *req.DurationSeconds
	}

	if req.Controls != nil {
		def.Controls = req.Controls
	}

	switch {
	case req.ClearMaxQueryCounts:
		def.MaxQueryCounts = nil
	case req.MaxQueryCounts != nil:
		def.MaxQueryCounts = req.MaxQueryCounts
	}

	switch {
	case req.ClearMaxBytesTransferred:
		def.MaxBytesTransferred = nil
	case req.MaxBytesTransferred != nil:
		def.MaxBytesTransferred = req.MaxBytesTransferred
	}

	switch {
	case req.ClearPriority:
		def.Priority = nil
	case req.Priority != nil:
		def.Priority = req.Priority
	}

	if req.AutoApprove != nil {
		def.AutoApprove = *req.AutoApprove
	}

	if req.GroupUIDs != nil {
		def.GroupUIDs = req.GroupUIDs
	}

	if req.DatabaseUIDs != nil {
		def.DatabaseUIDs = req.DatabaseUIDs
	}

	if req.ApprovalPatterns != nil {
		def.ApprovalPatterns = normalizeStrings(req.ApprovalPatterns)
	}

	if req.ApproverGroupUIDs != nil {
		def.ApproverGroupUIDs = normalizeUUIDs(req.ApproverGroupUIDs)
	}
}

func validateDefinitionRequest(req *CreateGrantDefinitionRequest) string {
	if req.Name == "" {
		return "name is required"
	}

	if len(req.Name) > 64 {
		return "name must be at most 64 characters"
	}

	if req.Slug == "" {
		return "slug is required"
	}

	if len(req.Slug) > slugMaxLength {
		return "slug must be at most 64 characters"
	}

	if !slugPattern.MatchString(req.Slug) {
		return "slug must be lowercase alphanumeric segments separated by hyphens (e.g. read-only-1h)"
	}

	// A UUID is not a valid slug: it defeats the whole point (a
	// human-typeable handle instead of the uid) and slugPattern's own shape
	// — lowercase hex segments joined by hyphens — happens to accept both
	// the canonical hyphenated form and, since hyphens are optional
	// separators, the bare 32-hex-digit form too. uuid.Parse recognizes
	// both, so it's the check, not a hand-rolled regex.
	if _, err := uuid.Parse(req.Slug); err == nil {
		return "slug must not be a UUID"
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

	if err := store.ValidateApprovalPatterns(req.ApprovalPatterns); err != nil {
		return err.Error()
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
		Slug:                req.Slug,
		Description:         req.Description,
		DurationSeconds:     req.DurationSeconds,
		Controls:            req.Controls,
		MaxQueryCounts:      req.MaxQueryCounts,
		MaxBytesTransferred: req.MaxBytesTransferred,
		Priority:            req.Priority,
		AutoApprove:         req.AutoApprove,
		GroupUIDs:           req.GroupUIDs,
		DatabaseUIDs:        req.DatabaseUIDs,
		ApprovalPatterns:    normalizeStrings(req.ApprovalPatterns),
		ApproverGroupUIDs:   normalizeUUIDs(req.ApproverGroupUIDs),
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
		if errors.Is(err, store.ErrGrantDefinitionSlugDuplicate) {
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
		"priority":             created.Priority,
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

// handleGetGrantDefinition — any authenticated user. The path param accepts
// either the definition's uid or its slug.
func (s *Server) handleGetGrantDefinition(c *gin.Context) {
	def, err := s.resolveGrantDefinition(c.Request.Context(), c.Param("uid"))
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

// handleUpdateGrantDefinition — admin-only. The path param accepts either
// the definition's uid or its slug. PATCH is a genuine partial update: only
// fields present in the body are changed (see UpdateGrantDefinitionRequest),
// so a caller like the auto-approve toggle can flip one field without
// round-tripping — and without risk of wiping — the rest of the definition.
func (s *Server) handleUpdateGrantDefinition(c *gin.Context) {
	var req UpdateGrantDefinitionRequest

	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, "invalid request: "+err.Error())

		return
	}

	def, err := s.resolveGrantDefinition(c.Request.Context(), c.Param("uid"))
	if err != nil {
		if errors.Is(err, store.ErrGrantDefinitionNotFound) {
			writeError(c, http.StatusNotFound, ErrCodeNotFound, "grant definition not found")

			return
		}

		writeInternalError(c, s.logger, err, "failed to get grant definition")

		return
	}

	applyGrantDefinitionUpdate(def, &req)

	// Validate the merged, post-update state — same rules a full create/
	// replace enforces — by reusing validateDefinitionRequest/
	// validateDefinitionScope against a request built from the merged
	// definition. This keeps validation logic in one place regardless of
	// which fields the PATCH actually touched.
	merged := &CreateGrantDefinitionRequest{
		Name:                def.Name,
		Slug:                def.Slug,
		Description:         def.Description,
		DurationSeconds:     def.DurationSeconds,
		Controls:            def.Controls,
		MaxQueryCounts:      def.MaxQueryCounts,
		MaxBytesTransferred: def.MaxBytesTransferred,
		Priority:            def.Priority,
		AutoApprove:         def.AutoApprove,
		GroupUIDs:           def.GroupUIDs,
		DatabaseUIDs:        def.DatabaseUIDs,
		ApprovalPatterns:    def.ApprovalPatterns,
		ApproverGroupUIDs:   def.ApproverGroupUIDs,
	}

	if msg := validateDefinitionRequest(merged); msg != "" {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, msg)

		return
	}

	if msg := s.validateDefinitionScope(c.Request.Context(), merged); msg != "" {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, msg)

		return
	}

	previousUID := def.UID

	updated, err := s.store.UpdateGrantDefinition(c.Request.Context(), def)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrGrantDefinitionSlugDuplicate),
			errors.Is(err, store.ErrGrantDefinitionDuplicate):
			writeError(c, http.StatusConflict, ErrCodeDuplicateName, err.Error())
		case errors.Is(err, store.ErrGrantDefinitionArchived):
			writeError(c, http.StatusConflict, ErrCodeConflict,
				"this is a superseded version of the definition; edit the current one")
		case errors.Is(err, store.ErrGrantDefinitionNotFound):
			writeError(c, http.StatusNotFound, ErrCodeNotFound, "grant definition not found")
		default:
			writeInternalError(c, s.logger, err, "failed to update grant definition")
		}

		return
	}

	currentUser := getCurrentUser(c)

	// An edit archives the row it started from and inserts a successor, so the
	// audit trail records both uids: the version that stopped being current
	// and the one that replaced it. Grants issued from the previous version
	// keep behaving exactly as they did.
	details, _ := json.Marshal(map[string]any{
		"grant_definition_uid":          updated.UID,
		"previous_grant_definition_uid": previousUID,
		"lineage_uid":                   updated.LineageUID,
		"versioned":                     updated.UID != previousUID,
		"name":                          updated.Name,
		"auto_approve":                  updated.AutoApprove,
		"group_uids":                    updated.GroupUIDs,
		"database_uids":                 updated.DatabaseUIDs,
	})

	_ = s.store.LogAuditEvent(c.Request.Context(), &store.AuditEvent{
		EventType:   "grant_definition.updated",
		PerformedBy: &currentUser.UID,
		Details:     details,
	})

	successResponse(c, updated)
}

// handleDeactivateGrantDefinition — admin-only. The path param accepts either
// the definition's uid or its slug.
//
// Default behavior is deactivation, which withdraws the definition across its
// whole version lineage and **fails closed**: every grant issued from any of
// its versions stops authorizing new connections. The response reports how
// many grants that affected.
//
// `?hard=true` asks for a real deletion instead, which is refused with a 409
// as soon as anything references the definition — a grant's shape lives on its
// definition, so deleting one out from under a grant is not a thing that can
// be allowed to happen.
func (s *Server) handleDeactivateGrantDefinition(c *gin.Context) {
	ctx := c.Request.Context()

	def, err := s.resolveGrantDefinition(ctx, c.Param("uid"))
	if err != nil {
		if errors.Is(err, store.ErrGrantDefinitionNotFound) {
			writeError(c, http.StatusNotFound, ErrCodeNotFound, "grant definition not found")

			return
		}

		writeInternalError(c, s.logger, err, "failed to get grant definition")

		return
	}

	currentUser := getCurrentUser(c)

	if c.Query("hard") == "true" {
		s.hardDeleteGrantDefinition(c, def, currentUser)

		return
	}

	_, activeGrants, err := s.store.CountGrantsForLineage(ctx, def.UID)
	if err != nil {
		writeInternalError(c, s.logger, err, "failed to count grants for grant definition")

		return
	}

	if err := s.store.DeactivateGrantDefinition(ctx, def.UID); err != nil {
		if errors.Is(err, store.ErrGrantDefinitionNotFound) {
			writeError(c, http.StatusNotFound, ErrCodeNotFound, "grant definition not found")

			return
		}

		writeInternalError(c, s.logger, err, "failed to deactivate grant definition")

		return
	}

	details, _ := json.Marshal(map[string]any{
		"grant_definition_uid": def.UID,
		"lineage_uid":          def.LineageUID,
		"affected_grants":      activeGrants,
	})

	_ = s.store.LogAuditEvent(ctx, &store.AuditEvent{
		EventType:   "grant_definition.deactivated",
		PerformedBy: &currentUser.UID,
		Details:     details,
	})

	successResponse(c, gin.H{
		"message":         "grant definition deactivated",
		"affected_grants": activeGrants,
	})
}

// hardDeleteGrantDefinition removes a definition and its whole version
// history, and only ever succeeds for one that was never used.
func (s *Server) hardDeleteGrantDefinition(c *gin.Context, def *store.GrantDefinition, currentUser *store.User) {
	ctx := c.Request.Context()

	err := s.store.DeleteGrantDefinition(ctx, def.UID)

	var inUse *store.GrantDefinitionInUseError

	switch {
	case err == nil:
	case errors.As(err, &inUse):
		writeError(c, http.StatusConflict, ErrCodeConflict, inUse.Error()+
			"; deactivate it instead of deleting it")

		return
	case errors.Is(err, store.ErrGrantDefinitionNotFound):
		writeError(c, http.StatusNotFound, ErrCodeNotFound, "grant definition not found")

		return
	default:
		writeInternalError(c, s.logger, err, "failed to delete grant definition")

		return
	}

	details, _ := json.Marshal(map[string]any{
		"grant_definition_uid": def.UID,
		"lineage_uid":          def.LineageUID,
	})

	_ = s.store.LogAuditEvent(ctx, &store.AuditEvent{
		EventType:   "grant_definition.deleted",
		PerformedBy: &currentUser.UID,
		Details:     details,
	})

	successResponse(c, gin.H{"message": "grant definition deleted"})
}

// normalizeStrings turns a nil slice into an empty one so bun writes '{}'
// rather than NULL into a NOT NULL array column.
func normalizeStrings(in []string) []string {
	if in == nil {
		return []string{}
	}

	return in
}

// normalizeUUIDs is normalizeStrings for uuid slices.
func normalizeUUIDs(in []uuid.UUID) []uuid.UUID {
	if in == nil {
		return []uuid.UUID{}
	}

	return in
}
