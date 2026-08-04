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
		// Set by protocols that insert the row after the result set has been
		// read, so they already know capture hit a limit.
		ResultsTruncated: query.ResultsTruncated,
		// Same for a capture that lost rows because the batched row writer
		// fell behind — known only once the rows have been handed over.
		ResultsDropped: query.ResultsDropped,
		// Carried through so a caller that already knows the approval outcome
		// (an approved hold logged by a protocol that inserts on completion)
		// doesn't lose it.
		ApprovalStatus:   query.ApprovalStatus,
		ApprovalPattern:  query.ApprovalPattern,
		ResolvedBy:       query.ResolvedBy,
		ResolvedAt:       query.ResolvedAt,
		ResolutionReason: query.ResolutionReason,
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

// StoreQueryRows stores captured result rows in a single bulk INSERT.
//
// Each row carries its own QueryID rather than the whole slice sharing one, so
// a batch may span several queries — and therefore several concurrent
// sessions. That is what lets a single process-wide writer amortize the
// round-trip across a busy proxy instead of issuing one INSERT per query.
//
// Every referenced query must already exist: query_rows.query_id is a foreign
// key, so the caller is responsible for creating the parent row first.
func (s *Store) StoreQueryRows(ctx context.Context, rows []PendingQueryRow) error {
	if len(rows) == 0 {
		return nil
	}

	// Convert to QueryRowModel for bun model
	resultRows := make([]QueryRowModel, len(rows))
	for i, row := range rows {
		resultRows[i] = QueryRowModel{
			UID:          newUIDv7(), // Generate UUIDv7 for each row
			QueryID:      row.QueryID,
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

// UpdateQueryCompletion updates a query with duration, rows affected, and error.
//
// resultsTruncated and resultsDropped are written unconditionally (unlike the
// pointer arguments, which are only written when set): protocols that persist
// the row before the result set is read only learn about a capture limit — or
// about rows the writer had to drop — here.
func (s *Store) UpdateQueryCompletion(
	ctx context.Context,
	uid uuid.UUID,
	durationMs *float64,
	rowsAffected *int64,
	queryError *string,
	resultsTruncated bool,
	resultsDropped bool,
) error {
	q := s.db.NewUpdate().
		Model((*Query)(nil)).
		Where("uid = ?", uid).
		Set("results_truncated = ?", resultsTruncated).
		Set("results_dropped = ?", resultsDropped)

	if durationMs != nil {
		q = q.Set("duration_ms = ?", *durationMs)
	}

	if rowsAffected != nil {
		q = q.Set("rows_affected = ?", *rowsAffected)
	}

	if queryError != nil {
		q = q.Set("error = ?", *queryError)
	}

	_, err := q.Exec(ctx)

	return err
}

// ListQueries retrieves queries with optional filters
func (s *Store) ListQueries(ctx context.Context, filter QueryFilter) ([]Query, error) {
	var queries []Query
	q := s.db.NewSelect().
		Model(&queries).
		ColumnExpr("q.uid, q.connection_id, q.sql_text, q.parameters, q.executed_at, q.duration_ms, q.rows_affected, q.error, " +
			"q.results_truncated, q.results_dropped, " +
			"q.approval_status, q.approval_pattern, q.resolved_by, q.resolved_at, q.resolution_reason, c.user_id, c.database_id").
		Join("JOIN connections c ON q.connection_id = c.uid")

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

	if filter.BeforeUID != nil {
		q = q.Where("q.uid < ?", *filter.BeforeUID)
	}

	q = q.Order("q.uid DESC")

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

// RetentionBatchSize bounds a single DELETE statement issued by the retention
// sweep. The sweep loops until nothing old is left, so this only caps how much
// one statement locks and how much WAL it writes at once — which matters on the
// very first run against a store that has been accumulating forever.
const RetentionBatchSize = 1000

// RetentionSweepResult reports what a retention sweep removed.
//
// Queries counts only queries deleted directly. Queries belonging to a reaped
// connection go away through the connection's ON DELETE CASCADE and are counted
// under Connections instead — as are all of their query_rows.
type RetentionSweepResult struct {
	// Connections is the number of closed connection records deleted.
	Connections int64
	// Queries is the number of query records deleted directly (i.e. queries on
	// connections that survived the sweep).
	Queries int64
}

// CleanupOldQueryRows deletes query history older than olderThan, together with
// the result rows captured for it. A zero or negative duration is a no-op:
// retention is opt-in, and the default is to keep history forever.
//
// The delete is driven from the parent rows, not from query_rows: both
// query_rows.query_id -> queries.uid and queries.connection_id ->
// connections.uid are ON DELETE CASCADE, so removing a query removes its rows
// and removing a connection removes its queries and their rows.
//
// Two sweeps run, in this order:
//
//  1. connections closed before the cutoff — cascading to their queries and
//     rows. This keeps the UI consistent: a closed connection never survives as
//     an empty shell whose queries have all been reaped.
//  2. queries executed before the cutoff that are still attached to a
//     connection the first sweep left alone (an open, long-lived session).
//
// Connections that are still open (disconnected_at IS NULL) are never reaped,
// however old they are: the session may still be live, and deleting its record
// would break the foreign key for the next query it logs. Such a connection can
// therefore outlive all of its queries and show up in the UI with none left —
// its `queries` counter is a lifetime counter, not a count of retained rows.
func (s *Store) CleanupOldQueryRows(ctx context.Context, olderThan time.Duration) (RetentionSweepResult, error) {
	var result RetentionSweepResult

	if olderThan <= 0 {
		return result, nil
	}

	cutoff := time.Now().Add(-olderThan)

	connections, err := s.deleteInBatches(ctx,
		`DELETE FROM connections WHERE uid IN (
			SELECT uid FROM connections
			WHERE disconnected_at IS NOT NULL AND disconnected_at < ?
			LIMIT ?
		)`, cutoff)
	result.Connections = connections

	if err != nil {
		return result, fmt.Errorf("failed to delete old connections: %w", err)
	}

	queries, err := s.deleteInBatches(ctx,
		`DELETE FROM queries WHERE uid IN (
			SELECT uid FROM queries
			WHERE executed_at < ?
			LIMIT ?
		)`, cutoff)
	result.Queries = queries

	if err != nil {
		return result, fmt.Errorf("failed to delete old queries: %w", err)
	}

	return result, nil
}

// deleteInBatches runs a LIMIT-ed delete statement repeatedly until it stops
// finding rows. The statement takes the cutoff timestamp and the batch size, in
// that order. Each iteration removes the rows it matched, so the loop strictly
// makes progress and terminates.
func (s *Store) deleteInBatches(ctx context.Context, query string, cutoff time.Time) (int64, error) {
	var total int64

	for {
		res, err := s.db.ExecContext(ctx, query, cutoff, RetentionBatchSize)
		if err != nil {
			return total, err
		}

		affected, err := res.RowsAffected()
		if err != nil {
			return total, err
		}

		total += affected

		if affected < RetentionBatchSize {
			return total, nil
		}

		// Yield between batches so a large first sweep doesn't monopolise a
		// connection, and so shutdown is observed promptly.
		if err := ctx.Err(); err != nil {
			return total, err
		}
	}
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
