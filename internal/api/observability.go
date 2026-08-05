package api

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/fclairamb/dbbat/internal/dump"
	"github.com/fclairamb/dbbat/internal/store"
)

// handleListConnections lists connections based on user role
func (s *Server) handleListConnections(c *gin.Context) {
	currentUser := getCurrentUser(c)
	filter := store.ConnectionFilter{}

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

	if before := c.Query("before"); before != "" {
		if uid, err := uuid.Parse(before); err == nil {
			filter.BeforeUID = &uid
		}
	}

	if limit := c.Query("limit"); limit != "" {
		if val, err := strconv.Atoi(limit); err == nil {
			filter.Limit = val
		}
	} else {
		filter.Limit = 100 // Default limit
	}

	if offset := c.Query("offset"); offset != "" {
		if val, err := strconv.Atoi(offset); err == nil {
			filter.Offset = val
		}
	}

	// Connector can only see their own connections
	if !currentUser.IsAdmin() && !currentUser.IsViewer() {
		filter.UserID = &currentUser.UID
	}

	connections, err := s.store.ListConnections(c.Request.Context(), filter)
	if err != nil {
		writeInternalError(c, s.logger, err, "failed to list connections")
		return
	}

	successResponse(c, gin.H{"connections": connections})
}

// handleGetConnection retrieves a single connection based on user role.
// Connectors may only fetch their own connections; a connection belonging to
// another user is reported as 404 (not 403) so its existence isn't leaked,
// matching handleListConnections' filtering behavior.
func (s *Server) handleGetConnection(c *gin.Context) {
	uid, err := parseUIDParam(c)
	if err != nil {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, "invalid connection UID")
		return
	}

	currentUser := getCurrentUser(c)

	conn, err := s.store.GetConnectionByUID(c.Request.Context(), uid)
	if err != nil {
		writeError(c, http.StatusNotFound, ErrCodeNotFound, "connection not found")
		return
	}

	// Connector can only see their own connections. Report 404, not 403, so
	// connectors can't learn that a connection they don't own exists.
	if !currentUser.IsAdmin() && !currentUser.IsViewer() && conn.UserID != currentUser.UID {
		writeError(c, http.StatusNotFound, ErrCodeNotFound, "connection not found")
		return
	}

	successResponse(c, connectionDetailResponse{
		Connection: conn,
		Dump:       s.dumpMetadata(c, uid),
	})
}

// connectionDetailResponse decorates a connection with capture metadata, for
// GET /connections/{uid} only. The list endpoint (handleListConnections)
// keeps returning bare store.Connection rows: statting a capture — local or
// remote — for every row of a paginated list is a needless fan-out that a
// single detail fetch doesn't pay.
type connectionDetailResponse struct {
	*store.Connection

	Dump DumpMetadata `json:"dump"`
}

// DumpMetadata reports whether a session capture is available for download
// and, if so, how large it is — enough for the UI to label the download
// action and warn before pulling a multi-megabyte file.
type DumpMetadata struct {
	Available bool  `json:"available"`
	SizeBytes int64 `json:"size_bytes"`
}

// handleListQueries lists queries with optional filters
func (s *Server) handleListQueries(c *gin.Context) {
	filter := store.QueryFilter{}

	// Parse query parameters
	if connectionID := c.Query("connection_id"); connectionID != "" {
		if uid, err := uuid.Parse(connectionID); err == nil {
			filter.ConnectionID = &uid
		}
	}

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

	if startTime := c.Query("start_time"); startTime != "" {
		if t, err := time.Parse(time.RFC3339, startTime); err == nil {
			filter.StartTime = &t
		}
	}

	if endTime := c.Query("end_time"); endTime != "" {
		if t, err := time.Parse(time.RFC3339, endTime); err == nil {
			filter.EndTime = &t
		}
	}

	if before := c.Query("before"); before != "" {
		if uid, err := uuid.Parse(before); err == nil {
			filter.BeforeUID = &uid
		}
	}

	if limit := c.Query("limit"); limit != "" {
		if val, err := strconv.Atoi(limit); err == nil {
			filter.Limit = val
		}
	} else {
		filter.Limit = 100 // Default limit
	}

	if offset := c.Query("offset"); offset != "" {
		if val, err := strconv.Atoi(offset); err == nil {
			filter.Offset = val
		}
	}

	queries, err := s.store.ListQueries(c.Request.Context(), filter)
	if err != nil {
		writeInternalError(c, s.logger, err, "failed to list queries")
		return
	}

	successResponse(c, gin.H{"queries": queries})
}

// handleGetQuery retrieves a query without its result rows
func (s *Server) handleGetQuery(c *gin.Context) {
	uid, err := parseUIDParam(c)
	if err != nil {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, "invalid query UID")
		return
	}

	query, err := s.store.GetQuery(c.Request.Context(), uid)
	if err != nil {
		writeError(c, http.StatusNotFound, ErrCodeNotFound, "query not found")
		return
	}

	successResponse(c, query)
}

// handleListAudit lists audit events with optional filters
func (s *Server) handleListAudit(c *gin.Context) {
	filter := store.AuditFilter{}

	// Parse query parameters
	if eventType := c.Query("event_type"); eventType != "" {
		filter.EventType = &eventType
	}

	if userID := c.Query("user_id"); userID != "" {
		if uid, err := uuid.Parse(userID); err == nil {
			filter.UserID = &uid
		}
	}

	if performedBy := c.Query("performed_by"); performedBy != "" {
		if uid, err := uuid.Parse(performedBy); err == nil {
			filter.PerformedBy = &uid
		}
	}

	if startTime := c.Query("start_time"); startTime != "" {
		if t, err := time.Parse(time.RFC3339, startTime); err == nil {
			filter.StartTime = &t
		}
	}

	if endTime := c.Query("end_time"); endTime != "" {
		if t, err := time.Parse(time.RFC3339, endTime); err == nil {
			filter.EndTime = &t
		}
	}

	if before := c.Query("before"); before != "" {
		if uid, err := uuid.Parse(before); err == nil {
			filter.BeforeUID = &uid
		}
	}

	if limit := c.Query("limit"); limit != "" {
		if val, err := strconv.Atoi(limit); err == nil {
			filter.Limit = val
		}
	} else {
		filter.Limit = 100 // Default limit
	}

	if offset := c.Query("offset"); offset != "" {
		if val, err := strconv.Atoi(offset); err == nil {
			filter.Offset = val
		}
	}

	events, err := s.store.ListAuditEvents(c.Request.Context(), filter)
	if err != nil {
		writeInternalError(c, s.logger, err, "failed to list audit events")
		return
	}

	successResponse(c, gin.H{"audit_events": events})
}

// handleGetQueryRows retrieves paginated rows for a specific query
func (s *Server) handleGetQueryRows(c *gin.Context) {
	uid, err := parseUIDParam(c)
	if err != nil {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, "invalid query UID")
		return
	}

	// Parse cursor parameter
	cursor := c.Query("cursor")

	// Parse limit parameter
	limit := store.DefaultQueryRowsLimit
	if limitStr := c.Query("limit"); limitStr != "" {
		val, err := strconv.Atoi(limitStr)
		if err != nil || val < 1 {
			writeError(c, http.StatusBadRequest, ErrCodeValidationError, "invalid limit")
			return
		}
		if val > store.MaxQueryRowsLimit {
			writeError(c, http.StatusBadRequest, ErrCodeValidationError, "invalid limit")
			return
		}
		limit = val
	}

	result, err := s.store.GetQueryRows(c.Request.Context(), uid, cursor, limit)
	if err != nil {
		if errors.Is(err, store.ErrQueryNotFound) {
			writeError(c, http.StatusNotFound, ErrCodeNotFound, "query not found")
			return
		}
		if errors.Is(err, store.ErrInvalidCursor) {
			writeError(c, http.StatusBadRequest, ErrCodeValidationError, "invalid cursor")
			return
		}
		writeInternalError(c, s.logger, err, "failed to get query rows")
		return
	}

	successResponse(c, result)
}

const (
	dumpFileExt         = dump.FileExt
	dumpFileContentType = dump.ContentType
)

// dumpsEnabled reports whether this process captures sessions at all. The
// spool directory is the switch even when uploads are configured: captures are
// always written locally first.
func (s *Server) dumpsEnabled() bool {
	return s.config != nil && s.config.Dump.Dir != ""
}

// localDumpPath is where a capture sits while it is still in the spool.
func (s *Server) localDumpPath(uid uuid.UUID) string {
	return filepath.Join(s.config.Dump.Dir, uid.String()+dumpFileExt)
}

// remoteDumpKey returns the blob key of an uploaded capture, or "" when there
// is none. A missing connection row is reported as no capture: the caller is
// asking for a capture, not for the row.
func (s *Server) remoteDumpKey(c *gin.Context, uid uuid.UUID) string {
	if s.dumpStorage == nil {
		return ""
	}

	conn, err := s.store.GetConnectionByUID(c.Request.Context(), uid)
	if err != nil {
		return ""
	}

	return conn.DumpKey
}

// dumpLocator resolves what is known about a connection's capture — a stat
// of the local spool file and a lookup of the remote key — in one place, so
// the three call sites that care (the detail metadata, the download and the
// delete handlers) agree on how a capture is found instead of each re-joining
// the path and re-statting by hand.
//
// It does not itself decide "local wins" or "delete both": that policy stays
// with each caller, since download and delete want different answers to
// "what do I do when both are present" (serve the nearer one vs. remove
// every copy).
type dumpLocator struct {
	localPath string
	localInfo os.FileInfo // nil when no local spool file
	remoteKey string      // "" when no object is recorded (or no storage configured)
}

// resolveDump locates uid's capture: stats the local spool file and looks up
// the remote key recorded on the connection row, if any. Callers must check
// s.dumpsEnabled() themselves first — this always looks, so its zero value
// isn't a reliable "dumps are disabled" signal.
func (s *Server) resolveDump(c *gin.Context, uid uuid.UUID) dumpLocator {
	loc := dumpLocator{localPath: s.localDumpPath(uid)}

	if info, err := os.Stat(loc.localPath); err == nil {
		loc.localInfo = info
	}

	loc.remoteKey = s.remoteDumpKey(c, uid)

	return loc
}

// available reports whether a capture exists anywhere, local or remote.
func (l dumpLocator) available() bool {
	return l.localInfo != nil || l.remoteKey != ""
}

// dumpMetadata reports whether uid has a downloadable capture and its size,
// for the connection detail response. A remote-only capture costs one extra
// Stat round trip to the bucket; that is acceptable for a single detail
// fetch even though it would not be for a paginated list.
func (s *Server) dumpMetadata(c *gin.Context, uid uuid.UUID) DumpMetadata {
	if !s.dumpsEnabled() {
		return DumpMetadata{}
	}

	loc := s.resolveDump(c, uid)

	if loc.localInfo != nil {
		return DumpMetadata{Available: true, SizeBytes: loc.localInfo.Size()}
	}

	if loc.remoteKey != "" {
		if size, err := s.dumpStorage.Stat(c.Request.Context(), loc.remoteKey); err == nil {
			return DumpMetadata{Available: true, SizeBytes: size}
		}
	}

	return DumpMetadata{}
}

// handleGetConnectionDump downloads the raw pcapng session capture for a
// connection.
//
// Local spool first, uploaded object second. That order matches the lifecycle:
// a capture lives in the spool until it is uploaded, and the key is only
// recorded once the object is in place, so at most one of the two is ever
// authoritative — and the local copy avoids a network round trip for the
// sessions this replica served itself.
func (s *Server) handleGetConnectionDump(c *gin.Context) {
	uid, err := parseUIDParam(c)
	if err != nil {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, "invalid connection UID")
		return
	}

	if !s.dumpsEnabled() {
		writeError(c, http.StatusNotFound, ErrCodeNotFound, "dumps are not enabled")
		return
	}

	loc := s.resolveDump(c, uid)

	if loc.localInfo != nil {
		c.Header("Content-Type", dumpFileContentType)
		c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="%s%s"`, uid, dumpFileExt))
		c.File(loc.localPath)

		return
	}

	if loc.remoteKey == "" {
		writeError(c, http.StatusNotFound, ErrCodeNotFound, "no dump available for this connection")
		return
	}

	body, err := s.dumpStorage.Open(c.Request.Context(), loc.remoteKey)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeError(c, http.StatusNotFound, ErrCodeNotFound, "no dump available for this connection")
			return
		}

		writeInternalError(c, s.logger, err, "failed to read dump")

		return
	}

	defer func() { _ = body.Close() }()

	c.Header("Content-Type", dumpFileContentType)
	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="%s%s"`, uid, dumpFileExt))
	c.Status(http.StatusOK)

	if _, err := io.Copy(c.Writer, body); err != nil {
		// Headers are already on the wire; all that is left is to say so.
		s.logger.ErrorContext(c.Request.Context(), "failed to stream dump",
			slog.String("connection_uid", uid.String()), slog.Any("error", err))
	}
}

// handleDeleteConnectionDump deletes the capture of a connection, wherever it
// currently lives — the local spool, blob storage, or both while an upload is
// in flight.
func (s *Server) handleDeleteConnectionDump(c *gin.Context) {
	uid, err := parseUIDParam(c)
	if err != nil {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, "invalid connection UID")
		return
	}

	if !s.dumpsEnabled() {
		writeError(c, http.StatusNotFound, ErrCodeNotFound, "dumps are not enabled")
		return
	}

	loc := s.resolveDump(c, uid)
	if !loc.available() {
		writeError(c, http.StatusNotFound, ErrCodeNotFound, "no dump available for this connection")
		return
	}

	if loc.localInfo != nil {
		if err := os.Remove(loc.localPath); err != nil && !os.IsNotExist(err) {
			writeInternalError(c, s.logger, err, "failed to delete dump")
			return
		}
	}

	if loc.remoteKey != "" {
		if err := s.dumpStorage.Delete(c.Request.Context(), loc.remoteKey); err != nil {
			writeInternalError(c, s.logger, err, "failed to delete dump")
			return
		}

		if err := s.store.ClearConnectionDumpKey(c.Request.Context(), uid); err != nil {
			writeInternalError(c, s.logger, err, "failed to delete dump")
			return
		}
	}

	c.Status(http.StatusNoContent)
}
