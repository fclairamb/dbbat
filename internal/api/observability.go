package api

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

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

const dumpFileExt = ".dbbat-dump"

// handleGetConnectionDump downloads the raw TNS dump for a connection.
func (s *Server) handleGetConnectionDump(c *gin.Context) {
	uid, err := parseUIDParam(c)
	if err != nil {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, "invalid connection UID")
		return
	}

	dumpDir := ""
	if s.config != nil {
		dumpDir = s.config.Dump.Dir
	}

	if dumpDir == "" {
		writeError(c, http.StatusNotFound, ErrCodeNotFound, "dumps are not enabled")
		return
	}

	dumpPath := filepath.Join(dumpDir, uid.String()+dumpFileExt)
	if _, err := os.Stat(dumpPath); os.IsNotExist(err) {
		writeError(c, http.StatusNotFound, ErrCodeNotFound, "no dump available for this connection")
		return
	}

	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="%s%s"`, uid, dumpFileExt))
	c.File(dumpPath)
}

// handleDeleteConnectionDump deletes the dump file for a connection.
func (s *Server) handleDeleteConnectionDump(c *gin.Context) {
	uid, err := parseUIDParam(c)
	if err != nil {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, "invalid connection UID")
		return
	}

	dumpDir := ""
	if s.config != nil {
		dumpDir = s.config.Dump.Dir
	}

	if dumpDir == "" {
		writeError(c, http.StatusNotFound, ErrCodeNotFound, "dumps are not enabled")
		return
	}

	dumpPath := filepath.Join(dumpDir, uid.String()+dumpFileExt)
	if _, err := os.Stat(dumpPath); os.IsNotExist(err) {
		writeError(c, http.StatusNotFound, ErrCodeNotFound, "no dump available for this connection")
		return
	}

	if err := os.Remove(dumpPath); err != nil {
		writeInternalError(c, s.logger, err, "failed to delete dump")
		return
	}

	c.Status(http.StatusNoContent)
}
