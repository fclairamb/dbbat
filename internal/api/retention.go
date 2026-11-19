package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/fclairamb/dbbat/internal/config"
	"github.com/fclairamb/dbbat/internal/store"
)

// SetRetentionWaker installs the function that wakes the retention sweeper. It
// is called after every retention policy edit (instance, server group, server,
// group membership), so a lowered window applies at the next pass instead of
// the next hour. Nil is fine: nothing wakes.
func (s *Server) SetRetentionWaker(wake func()) {
	s.retentionWake = wake
}

func (s *Server) wakeRetention() {
	if s.retentionWake != nil {
		s.retentionWake()
	}
}

// archiveEnabled reports whether an archive bucket is configured.
func (s *Server) archiveEnabled() bool {
	return s.config != nil && s.config.ArchiveURL != ""
}

// validateRetentionSeconds checks one level's two windows after an edit:
// neither is negative, an archive window needs an archive bucket, and when both
// are positive the archive window is shorter than the delete window. Only the
// values that are Set in the request are checked against the stored ones, which
// the caller passes as current.
func (s *Server) validateRetentionSeconds(archive, del store.OptionalSeconds, curArchive, curDelete *int64) string {
	for name, v := range map[string]store.OptionalSeconds{"archive_after_seconds": archive, "delete_after_seconds": del} {
		if v.Set && v.Value != nil && *v.Value < 0 {
			return name + " must not be negative"
		}
	}

	if archive.Set && archive.Value != nil && *archive.Value > 0 && !s.archiveEnabled() {
		return "archive_after_seconds needs an archive bucket: set DBB_ARCHIVE_URL"
	}

	a, d := curArchive, curDelete
	if archive.Set {
		a = archive.Value
	}

	if del.Set {
		d = del.Value
	}

	if a != nil && d != nil && *a > 0 && *d > 0 && *a >= *d {
		return "archive_after_seconds must be shorter than delete_after_seconds"
	}

	return ""
}

// effectiveRetention resolves the windows of one server with the live policy.
func (s *Server) effectiveRetention(c *gin.Context, serverUID uuid.UUID) (*store.ResolvedRetention, error) {
	ctx := c.Request.Context()

	instance, err := s.store.InstanceRetention(ctx)
	if err != nil {
		return nil, err
	}

	if !s.archiveEnabled() {
		instance.Archive = store.RetentionWindow{}
	}

	resolved, err := s.store.ResolveRetentionByServers(ctx, []uuid.UUID{serverUID}, instance)
	if err != nil {
		return nil, err
	}

	r := resolved[serverUID]

	if !s.archiveEnabled() {
		r.Archive = store.RetentionWindow{}
	}

	return &r, nil
}

// instanceRetentionInfo is the retention part of GET /instance.
type instanceRetentionInfo struct {
	// Raw are the retention.queries.* parameters, Go duration strings, empty
	// when unset (the environment applies).
	ArchiveAfter string `json:"archive_after"`
	DeleteAfter  string `json:"delete_after"`
}

// instanceResolvedRetention is the resolved instance level, with its source.
type instanceResolvedRetention struct {
	ArchiveAfterSeconds int64  `json:"archive_after_seconds"`
	ArchiveAfterSource  string `json:"archive_after_source"`
	DeleteAfterSeconds  int64  `json:"delete_after_seconds"`
	DeleteAfterSource   string `json:"delete_after_source"`
	// ArchiveEnabled is true when DBB_ARCHIVE_URL is set.
	ArchiveEnabled bool `json:"archive_enabled"`
}

func (s *Server) resolveInstanceRetention(raw store.RetentionParams) instanceResolvedRetention {
	var env config.RetentionWindows

	if s.config != nil {
		env = s.config.RetentionWindows()
	}

	r := store.ResolveInstanceRetention(raw, env)

	out := instanceResolvedRetention{
		ArchiveAfterSeconds: r.Archive.Seconds,
		ArchiveAfterSource:  r.Archive.InstanceSource,
		DeleteAfterSeconds:  r.Delete.Seconds,
		DeleteAfterSource:   r.Delete.InstanceSource,
		ArchiveEnabled:      s.archiveEnabled(),
	}

	if !out.ArchiveEnabled {
		out.ArchiveAfterSeconds = 0
		out.ArchiveAfterSource = ""
	}

	return out
}

// updateInstanceRetentionRequest is the body for PUT /instance/retention.
type updateInstanceRetentionRequest struct {
	// ArchiveAfter and DeleteAfter are Go durations ("36h", "8760h"). Empty
	// clears the parameter, which falls back to the environment; "0" means
	// never.
	ArchiveAfter string `json:"archive_after"`
	DeleteAfter  string `json:"delete_after"`
}

// handleUpdateInstanceRetention writes the retention.queries.* parameters.
// Admin-only. 400 on a malformed or negative value, on archive >= delete, and
// on an archive window while no archive bucket is configured.
func (s *Server) handleUpdateInstanceRetention(c *gin.Context) {
	var req updateInstanceRetentionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, "invalid request: "+err.Error())

		return
	}

	var archive, del time.Duration

	if req.ArchiveAfter != "" {
		d, err := store.ParseRetentionParam(req.ArchiveAfter)
		if err != nil {
			writeError(c, http.StatusBadRequest, ErrCodeValidationError, "archive_after: "+err.Error())

			return
		}

		archive = d

		if archive > 0 && !s.archiveEnabled() {
			writeError(c, http.StatusBadRequest, ErrCodeValidationError,
				"archive_after needs an archive bucket: set DBB_ARCHIVE_URL")

			return
		}
	}

	if req.DeleteAfter != "" {
		d, err := store.ParseRetentionParam(req.DeleteAfter)
		if err != nil {
			writeError(c, http.StatusBadRequest, ErrCodeValidationError, "delete_after: "+err.Error())

			return
		}

		del = d
	}

	if archive > 0 && del > 0 && archive >= del {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError,
			"archive_after must be shorter than delete_after")

		return
	}

	ctx := c.Request.Context()

	if err := s.store.SetRetentionParams(ctx, store.RetentionParams{
		ArchiveAfter: req.ArchiveAfter, DeleteAfter: req.DeleteAfter,
	}); err != nil {
		writeInternalError(c, s.logger, err, "failed to update instance retention")

		return
	}

	s.auditRetention(c, "instance.retention_updated", map[string]any{
		"archive_after": req.ArchiveAfter, "delete_after": req.DeleteAfter,
	})
	s.wakeRetention()

	c.Status(http.StatusNoContent)
}

func (s *Server) auditRetention(c *gin.Context, eventType string, details map[string]any) {
	currentUser := getCurrentUser(c)

	raw, err := json.Marshal(details)
	if err != nil {
		s.logger.WarnContext(c.Request.Context(), "failed to encode retention audit details",
			"error", fmt.Sprint(err))

		return
	}

	_ = s.store.LogAuditEvent(c.Request.Context(), &store.AuditEvent{
		EventType:   eventType,
		PerformedBy: &currentUser.UID,
		Details:     raw,
	})
}

// retentionEditAllowed validates the retention fields of a server edit against
// the stored values and writes the 400 itself.
func (s *Server) retentionEditAllowed(c *gin.Context, archive, del store.OptionalSeconds, current *store.Server) bool {
	if msg := s.validateRetentionSeconds(archive, del, current.ArchiveAfterSeconds, current.DeleteAfterSeconds); msg != "" {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, msg)

		return false
	}

	return true
}

// wakeRetentionIfEdited wakes the sweeper when a request touched a window:
// policy edits apply to data already stored.
func (s *Server) wakeRetentionIfEdited(archive, del store.OptionalSeconds) {
	if archive.Set || del.Set {
		s.wakeRetention()
	}
}

// applyGroupRetention validates and stores the windows of a server group edit,
// mirroring them onto the in-memory group, and writes the error itself.
func (s *Server) applyGroupRetention(c *gin.Context, group *store.ServerGroup, archive, del store.OptionalSeconds) bool {
	ctx := c.Request.Context()

	if msg := s.validateRetentionSeconds(archive, del, group.ArchiveAfterSeconds, group.DeleteAfterSeconds); msg != "" {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, msg)

		return false
	}

	if err := s.store.SetServerGroupRetention(ctx, group.UID, archive, del); err != nil {
		writeInternalError(c, s.logger, err, "failed to set server group retention")

		return false
	}

	if archive.Set {
		group.ArchiveAfterSeconds = archive.Value
	}

	if del.Set {
		group.DeleteAfterSeconds = del.Value
	}

	return true
}
