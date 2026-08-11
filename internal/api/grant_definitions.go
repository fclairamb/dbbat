package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// validateDefinitionScope checks that every scoped user group and server group
// uid actually exists, so an admin can't silently create a definition that is
// scoped to nothing (which would fail closed and look like a bug).
func (s *Server) validateDefinitionScope(ctx context.Context, req *CreateGrantDefinitionRequest) string {
	for _, groupUID := range req.UserGroupUIDs {
		if _, err := s.store.GetUserGroup(ctx, groupUID); err != nil {
			return "user group does not exist: " + groupUID.String()
		}
	}

	for _, groupUID := range req.ApproverUserGroupUIDs {
		if _, err := s.store.GetUserGroup(ctx, groupUID); err != nil {
			return "approver group does not exist: " + groupUID.String()
		}
	}

	for _, groupUID := range req.ServerGroupUIDs {
		if _, err := s.store.GetServerGroup(ctx, groupUID); err != nil {
			return "server group does not exist: " + groupUID.String()
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
	// UserGroupUIDs restricts the definition to members of these user groups.
	// Empty/omitted = every user, which is how every pre-scoping definition
	// keeps behaving.
	UserGroupUIDs []uuid.UUID `json:"user_group_uids"`
	// ServerGroupUIDs restricts the definition to the databases currently
	// belonging to these server groups. Empty/omitted = every database.
	ServerGroupUIDs []uuid.UUID `json:"server_group_uids"`
	// ApprovalPatterns are RE2 patterns that suspend a matching statement
	// until a second human approves it. Empty/omitted = no approval gating.
	// Compiled here so a bad pattern is a 400 rather than a runtime surprise
	// on the proxy hot path.
	ApprovalPatterns []string `json:"approval_patterns"`
	// SampleQueries are representative SQL statements saved alongside the
	// patterns to validate them against — a test bench for pattern
	// authoring. See POST /grant-definitions/validate-patterns.
	SampleQueries []string `json:"sample_queries"`
	// ApproverUserGroupUIDs lists the user groups whose members may resolve
	// those holds, in addition to admins. Empty/omitted = admins only.
	ApproverUserGroupUIDs []uuid.UUID `json:"approver_user_group_uids"`

	// RetiredDatabaseUIDs catches the removed per-database scope. It is not a
	// compatibility shim: server groups are a different entity, so there is
	// nothing to fold it onto, and silently dropping a scope restriction on
	// the floor would fail *open*. Its only job is to make the request a 400
	// that names the replacement — see validateDefinitionRequest.
	RetiredDatabaseUIDs []uuid.UUID `json:"database_uids"`

	// RetiredGroupUIDs / RetiredApproverGroupUIDs catch the pre-rename
	// spellings of UserGroupUIDs / ApproverUserGroupUIDs. Server groups made a
	// bare "group" ambiguous, so both fields were renamed; the old spellings
	// are refused rather than folded onto their replacements — silently
	// ignoring a scope restriction fails *open*. See errRetiredGroupUIDs /
	// errRetiredApproverGroupUIDs.
	RetiredGroupUIDs         []uuid.UUID `json:"group_uids"`
	RetiredApproverGroupUIDs []uuid.UUID `json:"approver_group_uids"`
}

// UpdateGrantDefinitionRequest is the JSON body for PATCH
// /grant-definitions/:uid. Unlike CreateGrantDefinitionRequest, every field
// is optional and a field absent from the body is left untouched on the
// existing definition rather than reset to its zero value. This is what
// makes a targeted PATCH — like the auto-approve toggle, which only means
// to flip one field — safe: it used to reuse CreateGrantDefinitionRequest
// as a full-replace body, so any field the caller didn't round-trip (most
// notably approval_patterns and approver_user_group_uids) got silently wiped.
//
// Slice fields (controls, user_group_uids, server_group_uids, approval_patterns,
// approver_user_group_uids) distinguish "absent" from "explicitly cleared":
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
	UserGroupUIDs            []uuid.UUID `json:"user_group_uids"`
	ServerGroupUIDs          []uuid.UUID `json:"server_group_uids"`
	ApprovalPatterns         []string    `json:"approval_patterns"`
	SampleQueries            []string    `json:"sample_queries"`
	ApproverUserGroupUIDs    []uuid.UUID `json:"approver_user_group_uids"`

	// RetiredDatabaseUIDs is refused rather than folded; see
	// CreateGrantDefinitionRequest.RetiredDatabaseUIDs.
	RetiredDatabaseUIDs []uuid.UUID `json:"database_uids"`

	// RetiredGroupUIDs / RetiredApproverGroupUIDs are refused rather than
	// folded; see CreateGrantDefinitionRequest.RetiredGroupUIDs.
	RetiredGroupUIDs         []uuid.UUID `json:"group_uids"`
	RetiredApproverGroupUIDs []uuid.UUID `json:"approver_group_uids"`
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

	if req.UserGroupUIDs != nil {
		def.UserGroupUIDs = req.UserGroupUIDs
	}

	if req.ServerGroupUIDs != nil {
		def.ServerGroupUIDs = req.ServerGroupUIDs
	}

	if req.ApprovalPatterns != nil {
		def.ApprovalPatterns = normalizeStrings(req.ApprovalPatterns)
	}

	if req.SampleQueries != nil {
		def.SampleQueries = normalizeStrings(req.SampleQueries)
	}

	if req.ApproverUserGroupUIDs != nil {
		def.ApproverUserGroupUIDs = normalizeUUIDs(req.ApproverUserGroupUIDs)
	}
}

// errRetiredDatabaseUIDs is the message both write handlers answer with when a
// body still carries the removed per-database scope. Refusing is deliberate:
// accepting and ignoring the field would drop a scope restriction silently,
// which fails *open*.
const errRetiredDatabaseUIDs = "database_uids is no longer supported; " +
	"scope the definition with server_group_uids instead"

// errRetiredGroupUIDs and errRetiredApproverGroupUIDs are the messages both
// write handlers answer with when a body still carries the pre-rename
// spellings of user_group_uids / approver_user_group_uids. Refusing rather
// than folding is deliberate: silently ignoring a scope restriction fails
// *open*.
const errRetiredGroupUIDs = "group_uids is no longer supported; " +
	"use user_group_uids instead"

const errRetiredApproverGroupUIDs = "approver_group_uids is no longer supported; " +
	"use approver_user_group_uids instead"

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

	if err := store.ValidateSampleQueries(req.SampleQueries); err != nil {
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

	if req.RetiredDatabaseUIDs != nil {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, errRetiredDatabaseUIDs)

		return
	}

	if req.RetiredGroupUIDs != nil {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, errRetiredGroupUIDs)

		return
	}

	if req.RetiredApproverGroupUIDs != nil {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, errRetiredApproverGroupUIDs)

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
		Name:                  req.Name,
		Slug:                  req.Slug,
		Description:           req.Description,
		DurationSeconds:       req.DurationSeconds,
		Controls:              req.Controls,
		MaxQueryCounts:        req.MaxQueryCounts,
		MaxBytesTransferred:   req.MaxBytesTransferred,
		Priority:              req.Priority,
		AutoApprove:           req.AutoApprove,
		UserGroupUIDs:         req.UserGroupUIDs,
		ServerGroupUIDs:       req.ServerGroupUIDs,
		ApprovalPatterns:      normalizeStrings(req.ApprovalPatterns),
		SampleQueries:         normalizeStrings(req.SampleQueries),
		ApproverUserGroupUIDs: normalizeUUIDs(req.ApproverUserGroupUIDs),
		CreatedBy:             currentUser.UID,
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
		"user_group_uids":      created.UserGroupUIDs,
		"server_group_uids":    created.ServerGroupUIDs,
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
			if defs[i].AppliesToUserGroups(groupUIDs) {
				visible = append(visible, defs[i])
			}
		}

		defs = visible
	}

	ptrs := make([]*store.GrantDefinition, len(defs))
	for i := range defs {
		ptrs[i] = &defs[i]
	}

	if err := s.attachScopedDatabaseUIDs(c.Request.Context(), ptrs); err != nil {
		writeInternalError(c, s.logger, err, "failed to resolve scoped database uids")

		return
	}

	successResponse(c, gin.H{"grant_definitions": defs})
}

// handleGetGrantDefinition — any authenticated user. The path param accepts
// either the definition's uid or its slug.
func (s *Server) handleGetGrantDefinition(c *gin.Context) {
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

	if !currentUser.IsAdmin() {
		visible, err := s.nonAdminMayViewGrantDefinition(ctx, currentUser, def)
		if err != nil {
			writeInternalError(c, s.logger, err, "failed to check grant definition visibility")

			return
		}

		if !visible {
			writeError(c, http.StatusNotFound, ErrCodeNotFound, "grant definition not found")

			return
		}
	}

	if err := s.attachScopedDatabaseUIDs(ctx, []*store.GrantDefinition{def}); err != nil {
		writeInternalError(c, s.logger, err, "failed to resolve scoped database uids")

		return
	}

	successResponse(c, def)
}

// nonAdminMayViewGrantDefinition decides whether a non-admin caller may read
// def directly by uid.
//
// A caller who holds a grant issued from this exact version always may:
// GET /grants already embeds the same definition unfiltered
// (store.attachDefinitions), whatever def's is_active or archived_at state,
// so a direct GET by uid must not be more restrictive than what that grant
// response already shows. This is what keeps an archived version's shape
// renderable for the grant pinned to it.
//
// Otherwise def must be the live, active, in-scope row for its lineage —
// exactly what the listing endpoint shows — since a direct GET by uid must
// not be a way to browse past that: not to an archived version, not to a
// deactivated one, not to one outside the caller's group scope.
func (s *Server) nonAdminMayViewGrantDefinition(
	ctx context.Context, currentUser *store.User, def *store.GrantDefinition,
) (bool, error) {
	owns, err := s.store.UserHasGrantForDefinition(ctx, currentUser.UID, def.UID)
	if err != nil {
		return false, fmt.Errorf("check grant ownership: %w", err)
	}

	if owns {
		return true, nil
	}

	if def.ArchivedAt != nil || !def.IsActive {
		return false, nil
	}

	groupUIDs, err := s.store.ListUserGroupUIDs(ctx, currentUser.UID)
	if err != nil {
		return false, fmt.Errorf("list user groups: %w", err)
	}

	return def.AppliesToUserGroups(groupUIDs), nil
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

	if req.RetiredDatabaseUIDs != nil {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, errRetiredDatabaseUIDs)

		return
	}

	if req.RetiredGroupUIDs != nil {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, errRetiredGroupUIDs)

		return
	}

	if req.RetiredApproverGroupUIDs != nil {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, errRetiredApproverGroupUIDs)

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
		Name:                  def.Name,
		Slug:                  def.Slug,
		Description:           def.Description,
		DurationSeconds:       def.DurationSeconds,
		Controls:              def.Controls,
		MaxQueryCounts:        def.MaxQueryCounts,
		MaxBytesTransferred:   def.MaxBytesTransferred,
		Priority:              def.Priority,
		AutoApprove:           def.AutoApprove,
		UserGroupUIDs:         def.UserGroupUIDs,
		ServerGroupUIDs:       def.ServerGroupUIDs,
		ApprovalPatterns:      def.ApprovalPatterns,
		SampleQueries:         def.SampleQueries,
		ApproverUserGroupUIDs: def.ApproverUserGroupUIDs,
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
		"user_group_uids":               updated.UserGroupUIDs,
		"server_group_uids":             updated.ServerGroupUIDs,
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

// attachScopedDatabaseUIDs fills GrantDefinition.ScopedDatabaseUIDs on every
// definition in defs, in place. Whatever server groups the definitions
// reference — shared or not, however many definitions are passed — their
// current membership is resolved with exactly one batched store query, never
// one query per definition. See GrantDefinition.ScopedDatabaseUIDs for what
// the field represents and why it exists.
func (s *Server) attachScopedDatabaseUIDs(ctx context.Context, defs []*store.GrantDefinition) error {
	groupSet := make(map[uuid.UUID]struct{})

	for _, def := range defs {
		for _, g := range def.ServerGroupUIDs {
			groupSet[g] = struct{}{}
		}
	}

	if len(groupSet) == 0 {
		return nil
	}

	groupUIDs := make([]uuid.UUID, 0, len(groupSet))
	for g := range groupSet {
		groupUIDs = append(groupUIDs, g)
	}

	membersByGroup, err := s.store.ListServerGroupMemberUIDsByGroups(ctx, groupUIDs)
	if err != nil {
		return fmt.Errorf("resolve scoped database uids: %w", err)
	}

	for _, def := range defs {
		def.ScopedDatabaseUIDs = scopedDatabaseUIDsForDefinition(def, membersByGroup)
	}

	return nil
}

// scopedDatabaseUIDsForDefinition computes a single definition's
// scoped_database_uids from a pre-fetched group -> members map. Pure
// function (no I/O) so attachScopedDatabaseUIDs stays the only place that
// queries.
func scopedDatabaseUIDsForDefinition(
	def *store.GrantDefinition, membersByGroup map[uuid.UUID][]uuid.UUID,
) *[]uuid.UUID {
	if len(def.ServerGroupUIDs) == 0 {
		return nil // unscoped: every database, represented by omitting the field
	}

	seen := make(map[uuid.UUID]struct{})
	union := make([]uuid.UUID, 0)

	for _, groupUID := range def.ServerGroupUIDs {
		for _, serverUID := range membersByGroup[groupUID] {
			if _, ok := seen[serverUID]; ok {
				continue
			}

			seen[serverUID] = struct{}{}

			union = append(union, serverUID)
		}
	}

	return &union
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
