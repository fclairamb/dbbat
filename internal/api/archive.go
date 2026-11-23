package api

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/fclairamb/dbbat/internal/archive"
	"github.com/fclairamb/dbbat/internal/store"
)

// Rows locations of a connection.
const (
	rowsLocationStore   = "store"
	rowsLocationArchive = "archive"
	rowsLocationDeleted = "deleted"
)

func rowsLocation(conn *store.Connection) string {
	switch {
	case conn.ArchiveDeletedAt != nil:
		return rowsLocationDeleted
	case conn.ArchivedAt != nil:
		return rowsLocationArchive
	default:
		return rowsLocationStore
	}
}

// writeArchiveError answers a failed archive read with a 502 that says which
// failure it was, never an empty list.
func (s *Server) writeArchiveError(c *gin.Context, err error) {
	var msg string

	switch {
	case errors.Is(err, archive.ErrNotConfigured):
		msg = "the archive is not configured (DBB_ARCHIVE_URL is unset): archived rows cannot be read"
	case errors.Is(err, archive.ErrObjectMissing):
		msg = "archive object missing: " + err.Error()
	case errors.Is(err, archive.ErrHashMismatch):
		msg = "archive object corrupted: " + err.Error()
	case errors.Is(err, store.ErrArchiveSealMismatch):
		msg = "archive reference tampered with: " + err.Error()
	default:
		msg = "archive read failed: " + err.Error()
	}

	s.logger.WarnContext(c.Request.Context(), "archive read failed", "error", err)
	writeError(c, http.StatusBadGateway, ErrCodeInternalError, msg)
}

// archivedQueryRows serves GET /queries/:uid/rows from the archive when the
// statement's connection is archived. handled is false when the rows are in
// PostgreSQL and the caller should read them from there. Same parameters, same
// JSON and same cursor semantics (an offset into the statement's rows) as
// Store.GetQueryRows.
func (s *Server) archivedQueryRows(c *gin.Context, queryUID uuid.UUID, cursor string, limit int) (*store.QueryRowsResult, bool) {
	ctx := c.Request.Context()

	query, err := s.store.GetQuery(ctx, queryUID)
	if err != nil {
		return nil, false // the PostgreSQL path reports not-found the usual way
	}

	ref, err := s.store.GetConnectionArchive(ctx, query.ConnectionID)
	if err != nil {
		if errors.Is(err, store.ErrArchiveNotFound) || errors.Is(err, store.ErrConnectionNotFound) {
			return nil, false
		}

		if errors.Is(err, store.ErrArchiveSealMismatch) {
			s.writeArchiveError(c, err)

			return nil, true
		}

		writeInternalError(c, s.logger, err, "failed to read the connection archive")

		return nil, true
	}

	var offset int64

	if cursor != "" {
		raw, err := base64.StdEncoding.DecodeString(cursor)
		if err != nil {
			writeError(c, http.StatusBadRequest, ErrCodeValidationError, "invalid cursor")

			return nil, true
		}

		var cur store.QueryRowsCursor
		if err := json.Unmarshal(raw, &cur); err != nil {
			writeError(c, http.StatusBadRequest, ErrCodeValidationError, "invalid cursor")

			return nil, true
		}

		offset = cur.Offset
	}

	if s.archiver == nil {
		s.writeArchiveError(c, archive.ErrNotConfigured)

		return nil, true
	}

	page, err := archive.ReadQueryRows(ctx, s.archiver.Bucket(),
		archive.Object{Key: ref.Key, Size: ref.Bytes, SHA256: ref.SHA256}, queryUID, offset, limit+1)
	if err != nil {
		s.writeArchiveError(c, err)

		return nil, true
	}

	result := &store.QueryRowsResult{Rows: make([]store.QueryRow, 0, limit), TotalRows: page.Total}

	var size int64

	for i, r := range page.Rows {
		if i >= limit {
			result.HasMore = true

			break
		}

		if size+int64(len(r.RowData)) > store.MaxQueryRowsDataSize && len(result.Rows) > 0 {
			result.HasMore = true

			break
		}

		result.Rows = append(result.Rows, store.QueryRow{
			RowNumber:    int(r.RowNumber),
			RowData:      json.RawMessage(r.RowData),
			RowSizeBytes: r.RowSizeBytes,
		})
		size += int64(len(r.RowData))
	}

	if result.HasMore {
		raw, _ := json.Marshal(store.QueryRowsCursor{Offset: offset + int64(len(result.Rows))})
		result.NextCursor = base64.StdEncoding.EncodeToString(raw)
	}

	successResponse(c, result)

	return result, true
}

// handleGetConnectionArchiveRows downloads the raw Parquet object holding a
// connection's archived rows, like the dump download, hash checked. Admin-only.
func (s *Server) handleGetConnectionArchiveRows(c *gin.Context) {
	uid, err := parseUIDParam(c)
	if err != nil {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, "invalid connection UID")

		return
	}

	ref, err := s.store.GetConnectionArchive(c.Request.Context(), uid)
	if err != nil {
		if errors.Is(err, store.ErrArchiveNotFound) || errors.Is(err, store.ErrConnectionNotFound) {
			writeError(c, http.StatusNotFound, ErrCodeNotFound, "this connection has no archive")

			return
		}

		if errors.Is(err, store.ErrArchiveSealMismatch) {
			s.writeArchiveError(c, err)

			return
		}

		writeInternalError(c, s.logger, err, "failed to read the connection archive")

		return
	}

	if s.archiver == nil {
		s.writeArchiveError(c, archive.ErrNotConfigured)

		return
	}

	data, err := archive.Download(c.Request.Context(), s.archiver.Bucket(),
		archive.Object{Key: ref.Key, Size: ref.Bytes, SHA256: ref.SHA256})
	if err != nil {
		s.writeArchiveError(c, err)

		return
	}

	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="%s-rows.parquet"`, uid))
	c.Data(http.StatusOK, "application/vnd.apache.parquet", data)
}
