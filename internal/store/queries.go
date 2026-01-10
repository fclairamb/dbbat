package store

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"
)

const (
	// MaxQueryRowsLimit is the maximum number of rows that can be returned per request
	MaxQueryRowsLimit = 1000
	// MaxQueryRowsDataSize is the maximum data size (1MB) that can be returned per request
	MaxQueryRowsDataSize = 1024 * 1024
	// DefaultQueryRowsLimit is the default number of rows returned if not specified
	DefaultQueryRowsLimit = 100
)

// QueryRowsResult contains paginated query rows
type QueryRowsResult struct {
	Rows       []QueryRow `json:"rows"`
	NextCursor string     `json:"next_cursor,omitempty"`
	HasMore    bool       `json:"has_more"`
	TotalRows  int64      `json:"total_rows"`
}

// QueryRowsCursor represents the pagination cursor state
type QueryRowsCursor struct {
	Offset int64 `json:"offset"`
}

// CreateQuery creates a new query record
func (s *Store) CreateQuery(ctx context.Context, query *Query) (*Query, error) {
	result := &Query{
		UID:          newUIDv7(), // Generate UUIDv7 for time-ordered inserts
		ConnectionID: query.ConnectionID,
		SQLText:      query.SQLText,
		Parameters:   query.Parameters,
		ExecutedAt:   query.ExecutedAt,
		DurationMs:   query.DurationMs,
		RowsAffected: query.RowsAffected,
		Error:        query.Error,
	}

	if result.ExecutedAt.IsZero() {
		result.ExecutedAt = time.Now()
	}

	_, err := s.db.NewInsert().
		Model(result).
		Exec(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to create query: %w", err)
	}

	return result, nil
}

// StoreQueryRows stores result rows for a query
func (s *Store) StoreQueryRows(ctx context.Context, queryUID uuid.UUID, rows []QueryRow) error {
	if len(rows) == 0 {
		return nil
	}

	// Convert QueryRow to QueryRowModel for bun model
	resultRows := make([]QueryRowModel, len(rows))
	for i, row := range rows {
		resultRows[i] = QueryRowModel{
			UID:          newUIDv7(), // Generate UUIDv7 for each row
			QueryID:      queryUID,
			RowNumber:    row.RowNumber,
			RowData:      row.RowData,
			RowSizeBytes: row.RowSizeBytes,
		}
	}

	_, err := s.db.NewInsert().
		Model(&resultRows).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("failed to store query rows: %w", err)
	}

	return nil
}

// ListQueries retrieves queries with optional filters
func (s *Store) ListQueries(ctx context.Context, filter QueryFilter) ([]Query, error) {
	var queries []Query
	q := s.db.NewSelect().
		Model(&queries).
		ColumnExpr("q.uid, q.connection_id, q.sql_text, q.parameters, q.executed_at, q.duration_ms, q.rows_affected, q.error")

	// Join with connections if filtering by user or database
	if filter.UserID != nil || filter.DatabaseID != nil {
		q = q.Join("JOIN connections c ON q.connection_id = c.uid")
	}

	if filter.ConnectionID != nil {
		q = q.Where("q.connection_id = ?", *filter.ConnectionID)
	}

	if filter.UserID != nil {
		q = q.Where("c.user_id = ?", *filter.UserID)
	}

	if filter.DatabaseID != nil {
		q = q.Where("c.database_id = ?", *filter.DatabaseID)
	}

	if filter.StartTime != nil {
		q = q.Where("q.executed_at >= ?", *filter.StartTime)
	}

	if filter.EndTime != nil {
		q = q.Where("q.executed_at <= ?", *filter.EndTime)
	}

	q = q.Order("q.executed_at DESC")

	if filter.Limit > 0 {
		q = q.Limit(filter.Limit)
	}

	if filter.Offset > 0 {
		q = q.Offset(filter.Offset)
	}

	err := q.Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list queries: %w", err)
	}

	if queries == nil {
		queries = []Query{}
	}
	return queries, nil
}

// GetQueryWithRows retrieves a query with its result rows
func (s *Store) GetQueryWithRows(ctx context.Context, uid uuid.UUID) (*QueryWithRows, error) {
	result := &QueryWithRows{}

	// Get query
	err := s.db.NewSelect().
		Model(&result.Query).
		Where("uid = ?", uid).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrQueryNotFound
		}
		return nil, fmt.Errorf("failed to get query: %w", err)
	}

	// Get rows
	var resultRows []QueryRowModel
	err = s.db.NewSelect().
		Model(&resultRows).
		Where("query_id = ?", uid).
		Order("row_number ASC").
		Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get query rows: %w", err)
	}

	// Convert to QueryRow
	result.Rows = make([]QueryRow, len(resultRows))
	for i, row := range resultRows {
		result.Rows[i] = QueryRow{
			RowNumber:    row.RowNumber,
			RowData:      row.RowData,
			RowSizeBytes: row.RowSizeBytes,
		}
	}

	return result, nil
}

// Ensure Query type is compatible with bun (used for table aliasing in queries)
var _ bun.BeforeAppendModelHook = (*Query)(nil)

// BeforeAppendModel implements bun.BeforeAppendModelHook for Query
func (q *Query) BeforeAppendModel(_ context.Context, _ bun.Query) error {
	// This hook allows us to use 'q' as an alias in queries
	return nil
}

// GetQueryRows retrieves paginated rows for a query with cursor-based pagination
func (s *Store) GetQueryRows(ctx context.Context, queryUID uuid.UUID, cursor string, limit int) (*QueryRowsResult, error) {
	// Validate limit
	if limit <= 0 {
		limit = DefaultQueryRowsLimit
	}
	if limit > MaxQueryRowsLimit {
		limit = MaxQueryRowsLimit
	}

	// Decode cursor if provided
	var offset int64
	if cursor != "" {
		cursorData, err := base64.StdEncoding.DecodeString(cursor)
		if err != nil {
			return nil, ErrInvalidCursor
		}
		var cursorObj QueryRowsCursor
		if err := json.Unmarshal(cursorData, &cursorObj); err != nil {
			return nil, ErrInvalidCursor
		}
		offset = cursorObj.Offset
	}

	// Verify the query exists
	exists, err := s.db.NewSelect().
		Model((*Query)(nil)).
		Where("uid = ?", queryUID).
		Exists(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to check query existence: %w", err)
	}
	if !exists {
		return nil, ErrQueryNotFound
	}

	// Get total row count for this query
	totalRows, err := s.db.NewSelect().
		Model((*QueryRowModel)(nil)).
		Where("query_id = ?", queryUID).
		Count(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to count query rows: %w", err)
	}

	// Query rows with offset and a buffer for checking hasMore
	var resultRows []QueryRowModel
	err = s.db.NewSelect().
		Model(&resultRows).
		Where("query_id = ?", queryUID).
		Order("row_number ASC").
		Offset(int(offset)).
		Limit(limit + 1). // Fetch one extra to check if there are more
		Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get query rows: %w", err)
	}

	// Build result with data size limit enforcement
	result := &QueryRowsResult{
		Rows:      make([]QueryRow, 0, limit),
		TotalRows: int64(totalRows),
	}

	var currentDataSize int64
	for i, row := range resultRows {
		// Stop if we've already collected enough rows
		if i >= limit {
			result.HasMore = true
			break
		}

		// Check data size limit
		rowSize := int64(len(row.RowData))
		if currentDataSize+rowSize > MaxQueryRowsDataSize && len(result.Rows) > 0 {
			// Stop before exceeding data size limit (but always include at least one row)
			result.HasMore = true
			break
		}

		result.Rows = append(result.Rows, QueryRow{
			RowNumber:    row.RowNumber,
			RowData:      row.RowData,
			RowSizeBytes: row.RowSizeBytes,
		})
		currentDataSize += rowSize
	}

	// Generate next cursor if there are more rows
	if result.HasMore {
		nextOffset := offset + int64(len(result.Rows))
		nextCursor := QueryRowsCursor{Offset: nextOffset}
		cursorJSON, err := json.Marshal(nextCursor)
		if err != nil {
			return nil, fmt.Errorf("failed to encode cursor: %w", err)
		}
		result.NextCursor = base64.StdEncoding.EncodeToString(cursorJSON)
	}

	return result, nil
}

// GetQuery retrieves a query by UID without rows
func (s *Store) GetQuery(ctx context.Context, uid uuid.UUID) (*Query, error) {
	result := &Query{}
	err := s.db.NewSelect().
		Model(result).
		Where("uid = ?", uid).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrQueryNotFound
		}
		return nil, fmt.Errorf("failed to get query: %w", err)
	}
	return result, nil
}
