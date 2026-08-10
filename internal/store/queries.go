package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
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
		// COPY metadata, set by the protocols that recognize a bulk transfer
		// (PostgreSQL today). Nil for every other statement.
		CopyFormat:    query.CopyFormat,
		CopyDirection: query.CopyDirection,
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

	// Truncated to what timestamptz can store: this exact value is what the
	// chain MAC covers and what verification reads back.
	result.ExecutedAt = normalizeStoredTime(result.ExecutedAt)

	if !s.ChainEnabled() {
		if _, err := s.db.NewInsert().Model(result).Exec(ctx); err != nil {
			return nil, fmt.Errorf("failed to create query: %w", err)
		}

		return result, nil
	}

	if err := s.appendQueryChain(ctx, result); err != nil {
		return nil, err
	}

	return result, nil
}

// appendQueryChain inserts one sealed query row, extending the chain of the
// connection it belongs to.
//
// The chain is per connection rather than global on purpose: retention
// (DBB_QUERY_STORAGE_RETENTION) deletes whole connections, which would sever a
// single global chain on its first sweep. Per connection, a reaped connection
// takes its own chain with it and every surviving connection still verifies.
//
// Ordering comes from an explicit chain_seq, not from uid: UUIDv7 orders by
// millisecond, and two statements in the same millisecond have no defined
// order, which a chain cannot tolerate.
// A stale cached head — this process's view of the chain after a peer replica
// appended to the same connection — collides with the unique index and is
// retried against a head re-read under the lock. See appendAuditChain.
func (s *Store) appendQueryChain(ctx context.Context, query *Query) error {
	state := s.queryChains.get(query.ConnectionID)

	state.mu.Lock()
	defer state.mu.Unlock()

	var err error

	for attempt := range chainAppendAttempts {
		if attempt > 0 {
			state.invalidate()

			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
		}

		if err = s.appendQueryChainOnce(ctx, state, query); err == nil {
			return nil
		}
	}

	return err
}

func (s *Store) appendQueryChainOnce(ctx context.Context, state *chainState, query *Query) error {
	err := s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(?, ?)",
			queryChainAdvisoryLockClass, queryChainLockID(query.ConnectionID)); err != nil {
			return fmt.Errorf("failed to take the query chain lock: %w", err)
		}

		if !state.loaded {
			seq, mac, err := readQueryChainHead(ctx, tx, query.ConnectionID)
			if err != nil {
				return err
			}

			state.seq = seq
			state.mac = mac
			state.loaded = true
		}

		seq := state.seq + 1

		prevMAC := state.mac
		if prevMAC == nil {
			prevMAC = s.queryGenesisMAC(query.ConnectionID)
		}

		payload, err := queryChainPayload(
			seq, query.UID, query.ConnectionID, query.SQLText,
			queryParametersJSON(query.Parameters), query.ExecutedAt, prevMAC,
		)
		if err != nil {
			return err
		}

		query.ChainSeq = &seq
		query.PrevMAC = prevMAC
		query.MAC = s.chainMAC(payload)

		if _, err := tx.NewInsert().Model(query).Exec(ctx); err != nil {
			return fmt.Errorf("failed to create query: %w", err)
		}

		return nil
	})
	if err != nil {
		state.invalidate()

		return err
	}

	state.seq = *query.ChainSeq
	state.mac = query.MAC

	return nil
}

// queryParametersJSON renders bound parameters the way they are stored, so the
// MAC covers the same document verification will read back out of the jsonb
// column. A nil parameter set stays nil, which the canonical payload records as
// absent rather than as an empty document.
func queryParametersJSON(params *QueryParameters) json.RawMessage {
	if params == nil {
		return nil
	}

	encoded, err := json.Marshal(params)
	if err != nil {
		// QueryParameters is a plain value type; marshaling it cannot fail.
		// Falling back to nil here would silently make the row's MAC cover
		// less than the row, so record something that cannot be confused with
		// a real parameter set instead.
		return json.RawMessage(`{"__unmarshalable__":true}`)
	}

	return encoded
}

// readQueryChainHead returns the highest chain_seq on a connection and its MAC,
// or (0, nil) when nothing on it has been chained yet.
func readQueryChainHead(ctx context.Context, db bun.IDB, connectionUID uuid.UUID) (int64, []byte, error) {
	var row struct {
		ChainSeq int64  `bun:"chain_seq"`
		MAC      []byte `bun:"mac"`
	}

	err := db.NewSelect().
		Model((*Query)(nil)).
		Column("chain_seq", "mac").
		Where("connection_id = ?", connectionUID).
		Where("chain_seq IS NOT NULL").
		Order("chain_seq DESC").
		Limit(1).
		Scan(ctx, &row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil, nil
		}

		return 0, nil, fmt.Errorf("failed to read the query chain head: %w", err)
	}

	return row.ChainSeq, row.MAC, nil
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
//
// When chaining is on the batch is sealed as it lands: it is split per query,
// each slice extends that query's own row chain, and the whole thing still goes
// in as one INSERT — see appendRowChains for why that split does not cost a
// round trip per query.
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

	if !s.ChainEnabled() {
		if _, err := s.db.NewInsert().Model(&resultRows).Exec(ctx); err != nil {
			return fmt.Errorf("failed to store query rows: %w", err)
		}

		return nil
	}

	return s.appendRowChains(ctx, resultRows)
}

// rowChainGroup is one query's slice of a batch: the indexes of its rows, in
// capture order, plus the cached head of the chain they extend.
type rowChainGroup struct {
	queryUID uuid.UUID
	rows     []int
	state    *chainState
}

// rowChainHead is a chain's head as it stands after a batch: the last stored
// row's number and MAC, and how many rows the chain now holds. The count is not
// the row number — a capture with gaps has fewer rows than its last row_number.
type rowChainHead struct {
	seq   int64
	mac   []byte
	count int64
}

// appendRowChains seals a batch of captured rows and inserts it.
//
// This is the hottest write path in the store, so the per-query split is
// deliberately *not* allowed to become a round trip per query. One transaction
// covers the whole batch: one statement takes every chain lock it needs, one
// statement loads whatever heads are not already cached, and the INSERT stays a
// single bulk INSERT spanning all the queries in the batch. What batching pays
// for — amortizing the round trip across ~1000 rows — is preserved; what it
// costs is the transaction and those two extra statements per *batch*.
//
// Locks are taken in query-uid order (rowChainGroups sorts), which is the same
// order for every caller, so two overlapping batches cannot deadlock.
func (s *Store) appendRowChains(ctx context.Context, rows []QueryRowModel) error {
	groups := s.rowChainGroups(rows)

	for i := range groups {
		groups[i].state.mu.Lock()
	}

	defer func() {
		for i := range groups {
			groups[i].state.mu.Unlock()
		}
	}()

	var err error

	for attempt := range chainAppendAttempts {
		if attempt > 0 {
			for i := range groups {
				groups[i].state.invalidate()
			}

			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
		}

		if err = s.appendRowChainsOnce(ctx, groups, rows); err == nil {
			return nil
		}
	}

	return err
}

// rowChainGroups splits a batch by parent query, keeping each query's rows in
// the order they were captured and ordering the groups by query uid.
func (s *Store) rowChainGroups(rows []QueryRowModel) []rowChainGroup {
	byQuery := make(map[uuid.UUID][]int, 4)
	order := make([]uuid.UUID, 0, 4)

	for i := range rows {
		queryUID := rows[i].QueryID

		if _, seen := byQuery[queryUID]; !seen {
			order = append(order, queryUID)
		}

		byQuery[queryUID] = append(byQuery[queryUID], i)
	}

	sort.Slice(order, func(i, j int) bool {
		return bytes.Compare(order[i][:], order[j][:]) < 0
	})

	groups := make([]rowChainGroup, 0, len(order))
	for _, queryUID := range order {
		groups = append(groups, rowChainGroup{
			queryUID: queryUID,
			rows:     byQuery[queryUID],
			state:    s.rowChains.get(queryUID),
		})
	}

	return groups
}

func (s *Store) appendRowChainsOnce(ctx context.Context, groups []rowChainGroup, rows []QueryRowModel) error {
	heads := make([]rowChainHead, len(groups))

	err := s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if err := lockRowChains(ctx, tx, groups); err != nil {
			return err
		}

		if err := loadColdRowChainHeads(ctx, tx, groups); err != nil {
			return err
		}

		for i := range groups {
			head, err := s.sealRowGroup(&groups[i], rows)
			if err != nil {
				return err
			}

			heads[i] = head
		}

		if _, err := tx.NewInsert().Model(&rows).Exec(ctx); err != nil {
			return fmt.Errorf("failed to store query rows: %w", err)
		}

		return nil
	})
	if err != nil {
		for i := range groups {
			groups[i].state.invalidate()
		}

		return err
	}

	for i := range groups {
		state := groups[i].state
		state.loaded = true
		state.seq = heads[i].seq
		state.mac = heads[i].mac
		state.count = heads[i].count
	}

	return nil
}

// sealRowGroup MACs one query's slice of the batch, linking each row to the one
// before it and the first to the head the chain already had (or to the query's
// genesis MAC when the capture starts here).
func (s *Store) sealRowGroup(group *rowChainGroup, rows []QueryRowModel) (rowChainHead, error) {
	prevMAC := group.state.mac
	if prevMAC == nil {
		prevMAC = s.rowGenesisMAC(group.queryUID)
	}

	for _, idx := range group.rows {
		row := &rows[idx]

		payload, err := rowChainPayload(
			row.QueryID, row.UID, row.RowNumber, row.RowData, row.RowSizeBytes, prevMAC,
		)
		if err != nil {
			return rowChainHead{}, err
		}

		row.PrevMAC = prevMAC
		row.MAC = s.chainMAC(payload)
		prevMAC = row.MAC
	}

	last := rows[group.rows[len(group.rows)-1]]

	return rowChainHead{
		seq:   int64(last.RowNumber),
		mac:   last.MAC,
		count: group.state.count + int64(len(group.rows)),
	}, nil
}

// lockRowChains takes every chain lock the batch needs in one statement. The
// alternative — one round trip per query in the batch — is exactly the cost
// this write path cannot afford.
func lockRowChains(ctx context.Context, tx bun.Tx, groups []rowChainGroup) error {
	var sb strings.Builder

	args := make([]any, 0, 2*len(groups))

	sb.WriteString("SELECT ")

	for i := range groups {
		if i > 0 {
			sb.WriteString(", ")
		}

		sb.WriteString("pg_advisory_xact_lock(?, ?)")

		args = append(args, rowChainAdvisoryLockClass, rowChainLockID(groups[i].queryUID))
	}

	if _, err := tx.ExecContext(ctx, sb.String(), args...); err != nil {
		return fmt.Errorf("failed to take the row chain locks: %w", err)
	}

	return nil
}

// loadColdRowChainHeads fills in the heads this process does not have cached,
// for every group at once.
func loadColdRowChainHeads(ctx context.Context, tx bun.IDB, groups []rowChainGroup) error {
	cold := make([]uuid.UUID, 0, len(groups))

	for i := range groups {
		if !groups[i].state.loaded {
			cold = append(cold, groups[i].queryUID)
		}
	}

	if len(cold) == 0 {
		return nil
	}

	heads, err := readRowChainHeads(ctx, tx, cold)
	if err != nil {
		return err
	}

	for i := range groups {
		state := groups[i].state
		if state.loaded {
			continue
		}

		// A query with nothing chained yet gets the zero head, which
		// sealRowGroup turns into the genesis link.
		head := heads[groups[i].queryUID]
		state.seq = head.seq
		state.mac = head.mac
		state.count = head.count
		state.loaded = true
	}

	return nil
}

// readRowChainHeads returns, per query, the last stored row's number and MAC
// plus how many rows the chain holds. Queries with nothing chained are absent
// from the map.
func readRowChainHeads(ctx context.Context, db bun.IDB, queryUIDs []uuid.UUID) (map[uuid.UUID]rowChainHead, error) {
	var rows []struct {
		QueryID uuid.UUID `bun:"query_id"`
		LastRow int64     `bun:"last_row"`
		MAC     []byte    `bun:"mac"`
		Count   int64     `bun:"cnt"`
	}

	err := db.NewSelect().
		Model((*QueryRowModel)(nil)).
		ColumnExpr("qr.query_id").
		ColumnExpr("MAX(qr.row_number) AS last_row").
		ColumnExpr("COUNT(*) AS cnt").
		ColumnExpr("(ARRAY_AGG(qr.mac ORDER BY qr.row_number DESC, qr.uid DESC))[1] AS mac").
		Where("qr.mac IS NOT NULL").
		Where("qr.query_id IN (?)", bun.List(queryUIDs)).
		GroupExpr("qr.query_id").
		Scan(ctx, &rows)
	if err != nil {
		return nil, fmt.Errorf("failed to read the row chain heads: %w", err)
	}

	heads := make(map[uuid.UUID]rowChainHead, len(rows))
	for _, row := range rows {
		heads[row.QueryID] = rowChainHead{seq: row.LastRow, mac: row.MAC, count: row.Count}
	}

	return heads, nil
}

// SealQueryRowChain stamps the final head of a capture's row chain onto its
// query, and forgets the cached head.
//
// It is called at the flush barrier — the point where every captured row is
// durable (or definitively lost) and the query is about to be marked complete —
// so it is the row-chain counterpart of what CloseConnection does for a
// session's statements. Without the stamp, deleting the last captured rows
// would leave a shorter chain that still verified end to end.
//
// A query this process chained nothing for costs nothing: no cached head means
// nothing was captured (or the capture was already sealed), and the answer is
// given without touching the database.
//
// The gap it shares with connections.query_chain_mac: a process that dies
// mid-capture never reaches the barrier, so that capture keeps a NULL stamp and
// only its prefix and interior are protected. A missing stamp is never a break.
func (s *Store) SealQueryRowChain(ctx context.Context, queryUID uuid.UUID) error {
	if !s.ChainEnabled() {
		return nil
	}

	state, ok := s.rowChains.lookup(queryUID)
	if !ok {
		return nil
	}

	state.mu.Lock()
	head := rowChainHead{seq: state.seq, mac: state.mac, count: state.count}
	loaded := state.loaded
	state.mu.Unlock()

	s.rowChains.forget(queryUID)

	if !loaded || head.mac == nil {
		return nil
	}

	// The stamp is a MAC over the head, not the head itself: a verbatim copy
	// could be recomputed by anyone who can read query_rows, which is exactly
	// the attacker this is meant to catch. See rowChainStampMAC.
	_, err := s.db.NewUpdate().
		Model((*Query)(nil)).
		Where("uid = ?", queryUID).
		Set("row_chain_mac = ?", s.rowChainStampMAC(queryUID, head.count, head.mac)).
		Set("row_chain_len = ?", head.count).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("failed to stamp the row chain head: %w", err)
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
			"q.results_truncated, q.results_dropped, q.copy_format, q.copy_direction, " +
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
