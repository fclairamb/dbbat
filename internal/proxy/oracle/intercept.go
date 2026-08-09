package oracle

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/fclairamb/dbbat/internal/proxy/shared"
	"github.com/fclairamb/dbbat/internal/store"
)

// trackedCursor tracks a parsed cursor and its SQL.
type trackedCursor struct {
	cursorID   uint16
	sql        string
	bindValues []string
	parsedAt   time.Time
	columns    []columnDef // Column definitions from first response (for multi-fetch)
}

// oracleQueryTracker manages per-session cursor state and pending queries.
type oracleQueryTracker struct {
	cursors      map[uint16]*trackedCursor
	pendingQuery *pendingOracleQuery
}

// pendingOracleQuery tracks a query in progress (between OALL8/OFETCH and response).
type pendingOracleQuery struct {
	cursor         *trackedCursor
	startTime      time.Time
	capturedBytes  int64
	rowNumber      int
	truncated      bool
	queryUID       uuid.UUID // Set after query record is created in DB
	queryPersisted bool      // True after query record is created
	lastRow        []string  // Last captured row values (for continuation packet duplicate tracking)

	// rowSink batches this query's captured rows through the shared writer.
	// Created by persistQueryRecord, i.e. only once the parent queries row
	// exists — query_rows.query_id is a foreign key.
	rowSink *shared.QuerySink
}

// newOracleQueryTracker creates a new query tracker.
func newOracleQueryTracker() *oracleQueryTracker {
	return &oracleQueryTracker{
		cursors: make(map[uint16]*trackedCursor),
	}
}

// handleOALL8 intercepts an OALL8 message: decodes SQL, checks access controls,
// and begins tracking the query. Returns an error if the query should be blocked.
func (s *session) handleOALL8(ttcPayload []byte) error {
	result, err := decodeOALL8(ttcPayload)
	if err != nil {
		// A well-formed OALL8 with no SQL text is a re-execution of a cursor
		// the client already parsed, not a broken frame: it is gated against
		// the SQL that cursor holds rather than forwarded.
		var noSQL *OALL8NoSQLError
		if errors.As(err, &noSQL) {
			return s.handleCursorReexec(noSQL.CursorID)
		}

		s.logger.WarnContext(s.ctx, "failed to decode OALL8", slog.Any("error", err))
		// Don't block on decode failure — let it pass through
		return nil
	}

	s.logger.DebugContext(s.ctx, "intercepted OALL8",
		slog.String("sql", truncateSQL(result.SQL, 200)),
		slog.Uint64("cursor_id", uint64(result.CursorID)),
		slog.Int("bind_count", len(result.BindValues)),
	)

	// Access control check
	if s.grant != nil {
		if err := shared.ValidateOracleQuery(result.SQL, s.grant); err != nil {
			s.logger.WarnContext(s.ctx, "query blocked by access control",
				slog.String("sql", truncateSQL(result.SQL, 200)),
				slog.Any("error", err),
			)

			return err
		}
	}

	// Complete previous query if still pending (sets duration)
	s.flushPendingQuery()

	// Approval hold — after the static validators, before anything reaches
	// upstream.
	approvalUID, herr := s.holdIfNeeded(result.SQL)
	if herr != nil {
		return herr
	}

	// Track the cursor
	cursor := &trackedCursor{
		cursorID:   result.CursorID,
		sql:        result.SQL,
		bindValues: result.BindValues,
		parsedAt:   time.Now(),
	}
	s.tracker.cursors[result.CursorID] = cursor

	// Start pending query and persist immediately
	s.tracker.pendingQuery = &pendingOracleQuery{
		cursor:    cursor,
		startTime: time.Now(),
	}

	// An approval hold already inserted the row; reuse it rather than writing
	// a second one for the same statement.
	if approvalUID != uuid.Nil {
		s.tracker.pendingQuery.queryUID = approvalUID
		s.tracker.pendingQuery.queryPersisted = true
	} else {
		s.persistQueryRecord()
	}

	return nil
}

// handleCursorReexec gates an execution that carries no statement of its own —
// a SQL-less OALL8, or an OFETCH that starts a fresh pending query — against
// the SQL the named cursor was parsed with.
//
// Without this, an approval decision and every statement-shaped control applied
// to the *parse* only: a client that parsed once and re-executed the same
// cursor got one hold and then a free run for the rest of the session.
func (s *session) handleCursorReexec(cursorID uint16) error {
	cursor, ok := s.tracker.cursors[cursorID]
	if !ok {
		return s.refuseUnknownCursor(cursorID)
	}

	return s.regateCursor(cursor)
}

// refuseUnknownCursor decides what to do with a re-execution naming a cursor
// dbbat never saw parsed. The statement it would run is unknown, so:
//
//   - under a grant carrying statement-shaped controls, it fails closed — a
//     restrictive grant must not be bypassable by an execution the proxy cannot
//     identify (the shape the SQL Server proxy already chose, see
//     ErrUnknownPreparedStatement);
//   - under a grant carrying none of them, it is forwarded with a WARN. An
//     untracked cursor is not by itself an attack — dbbat may have attached
//     mid-session, or the entry may be gone — and refusing there would break
//     permissive sessions for no security gain.
//
// This asymmetry is deliberate and documented in docs/approvals.md.
func (s *session) refuseUnknownCursor(cursorID uint16) error {
	if !s.hasStatementControls() {
		s.logger.WarnContext(s.ctx,
			"forwarding a re-execution of an untracked cursor: the grant carries no statement-shaped control",
			slog.Uint64("cursor_id", uint64(cursorID)),
		)

		return nil
	}

	s.logger.WarnContext(s.ctx,
		"refused a re-execution of an untracked cursor under a restrictive grant",
		slog.Uint64("cursor_id", uint64(cursorID)),
	)

	return ErrUnknownCursor
}

// hasStatementControls reports whether the session's grant carries controls
// that are evaluated per statement — the ones a re-execution could otherwise
// evade. Deliberately three: approval patterns, read_only, block_ddl.
// block_copy is left out because an Oracle COPY is a client-side SQL*Plus
// command, never a server statement, so it cannot ride a re-executed cursor.
//
// Note the pattern set is read off the grant regardless of
// DBB_APPROVAL_ENABLED: a grant carrying patterns while approvals are globally
// off still counts as restrictive here. Intentional — it errs fail-closed, and
// the alternative would make an untracked cursor's fate depend on a global
// switch the grant's author cannot see.
func (s *session) hasStatementControls() bool {
	if s.grant == nil {
		return false
	}

	return len(s.grant.ApprovalPatterns()) > 0 || s.grant.IsReadOnly() || s.grant.ShouldBlockDDL()
}

// regateCursor re-runs the statement gate — static controls, then the approval
// hold — against the SQL a tracked cursor holds, and starts a fresh pending
// query for this execution. Same order as the SQL-carrying path, so a
// re-execution is enforced exactly like the parse that created the cursor.
func (s *session) regateCursor(cursor *trackedCursor) error {
	sql := cursor.sql

	s.logger.DebugContext(s.ctx, "intercepted cursor re-execution",
		slog.String("sql", truncateSQL(sql, 200)),
		slog.Uint64("cursor_id", uint64(cursor.cursorID)),
	)

	// Access control check
	if s.grant != nil {
		if err := shared.ValidateOracleQuery(sql, s.grant); err != nil {
			s.logger.WarnContext(s.ctx, "cursor re-execution blocked by access control",
				slog.String("sql", truncateSQL(sql, 200)),
				slog.Any("error", err),
			)

			return err
		}
	}

	// Complete previous query if still pending (sets duration)
	s.flushPendingQuery()

	// Approval hold — after the static validators, before anything reaches
	// upstream.
	approvalUID, herr := s.holdIfNeeded(sql)
	if herr != nil {
		return herr
	}

	s.tracker.pendingQuery = &pendingOracleQuery{
		cursor:    cursor,
		startTime: time.Now(),
	}

	// An approval hold already inserted the row; reuse it rather than writing
	// a second one for the same statement.
	if approvalUID != uuid.Nil {
		s.tracker.pendingQuery.queryUID = approvalUID
		s.tracker.pendingQuery.queryPersisted = true
	} else {
		s.persistQueryRecord()
	}

	return nil
}

// handlePiggybackReexec gates a piggyback cursor re-execution — func 0x03,
// sub-op 0x4e (SELECT) or 0x04 (everything else). This is the frame every
// modern thin client actually sends to re-run a statement it already parsed;
// the SQL-less OALL8 handleCursorReexec was written for is its legacy
// equivalent, and no tested client emits that one (see docs/oracle.md).
//
// A cursor dbbat knows is re-gated exactly like the parse that created it. One
// it does not know is **forwarded**, not refused — deliberately, and unlike the
// SQL-less OALL8:
//
//   - dbbat learns these cursor ids by reading them off the server's response
//     (learnCursorID), so an unknown id here usually means dbbat attached
//     mid-session or missed one response, not that a client is evading anything;
//   - and this frame is what a plain `cur.execute()` loop sends, so failing it
//     closed would break ordinary read-only sessions on every second execution.
//
// Closing that half is filed in
// specs/todos/2026-08-09-oracle-piggyback-reexec-unknown-cursor.md.
func (s *session) handlePiggybackReexec(ttcPayload []byte) error {
	cursorID, err := decodeCursorReexec(ttcPayload)
	if err != nil {
		s.logger.DebugContext(s.ctx, "failed to decode piggyback cursor re-execution", slog.Any("error", err))

		return nil
	}

	cursor, ok := s.tracker.cursors[cursorID]
	if !ok {
		s.logger.WarnContext(s.ctx,
			"forwarding a piggyback re-execution of an untracked cursor: its statement is unknown",
			slog.Uint64("cursor_id", uint64(cursorID)),
		)

		return nil
	}

	return s.regateCursor(cursor)
}

// learnCursorID records the cursor id the server assigned to the statement
// currently in flight, so a later re-execution naming that id resolves to its
// SQL.
//
// Only the legacy OALL8 carries the cursor id on the request; the piggyback and
// JDBC exec paths leave dbbat to read it off the response. Learning is a
// one-shot per statement — a cursor that already has an id is never re-read —
// which is what keeps the anchored scan in findCursorIDInResponse from being
// re-run against row-stream bytes for the rest of a fetch.
func (s *session) learnCursorID(ttcPayload []byte) {
	pending := s.tracker.pendingQuery
	if pending == nil || pending.cursor == nil || pending.cursor.cursorID != 0 {
		return
	}

	cursorID, ok := findCursorIDInResponse(ttcPayload)
	if !ok {
		return
	}

	pending.cursor.cursorID = cursorID
	s.tracker.cursors[cursorID] = pending.cursor

	s.logger.DebugContext(s.ctx, "learned server-assigned cursor id",
		slog.Uint64("cursor_id", uint64(cursorID)),
		slog.String("sql", truncateSQL(pending.cursor.sql, 200)),
	)
}

// flushPendingQuery completes any outstanding query that hasn't been finalized.
// Called before starting a new query to ensure the previous one is persisted.
func (s *session) flushPendingQuery() {
	if s.tracker.pendingQuery != nil {
		s.completeQuery(nil, nil)
	}
}

// handlePiggybackExec intercepts a v315+ piggyback execute-with-SQL message.
func (s *session) handlePiggybackExec(ttcPayload []byte) error {
	result, err := decodePiggybackExecSQL(ttcPayload)
	if err != nil {
		s.logger.DebugContext(s.ctx, "failed to decode piggyback exec", slog.Any("error", err))
		return nil // Don't block on decode failure
	}

	s.logger.InfoContext(s.ctx, "query intercepted",
		slog.String("sql", truncateSQL(result.SQL, 200)),
	)

	// Access control check
	if s.grant != nil {
		if err := shared.ValidateOracleQuery(result.SQL, s.grant); err != nil {
			s.logger.WarnContext(s.ctx, "query blocked by access control",
				slog.String("sql", truncateSQL(result.SQL, 200)),
				slog.Any("error", err),
			)
			return err
		}
	}

	// Complete previous query if still pending (sets duration)
	s.flushPendingQuery()

	approvalUID, herr := s.holdIfNeeded(result.SQL)
	if herr != nil {
		return herr
	}

	// Track as pending query and persist immediately
	cursor := &trackedCursor{
		sql:        result.SQL,
		bindValues: result.BindValues,
		parsedAt:   time.Now(),
	}
	s.tracker.pendingQuery = &pendingOracleQuery{
		cursor:    cursor,
		startTime: time.Now(),
	}

	if approvalUID != uuid.Nil {
		s.tracker.pendingQuery.queryUID = approvalUID
		s.tracker.pendingQuery.queryPersisted = true
	} else {
		s.persistQueryRecord()
	}

	return nil
}

// handleJDBCExec intercepts a JDBC execute-with-SQL message (func=0x11, sub=0x69).
//
// This is a statement-carrying op like OALL8 and the v315+ piggyback exec, so it
// runs the same gate in the same order: static validators, then the approval
// hold, and only then is the row recorded and the packet allowed upstream. It
// used to record the query and nothing else, which made the JDBC thin driver's
// exec path a way around `read_only`, `block_ddl` and every approval pattern.
//
// Returns a non-nil error when the statement must not be forwarded; the caller
// answers the client with a TTC error instead.
func (s *session) handleJDBCExec(ttcPayload []byte) error {
	result, err := decodeExecSQL(ttcPayload)
	if err != nil {
		s.logger.DebugContext(s.ctx, "failed to decode JDBC exec", slog.Any("error", err))
		// Don't block on decode failure — let it pass through, as OALL8 does.
		// See the Oracle caveat in docs/approvals.md.
		return nil
	}

	// The gate matches on the normalized text, so the text recorded and shown in
	// /queries is the same one the patterns ran against.
	sql := shared.NormalizeSQL(result.SQL)

	s.logger.InfoContext(s.ctx, "query intercepted",
		slog.String("sql", truncateSQL(sql, 200)),
		slog.String("source", "jdbc"),
	)

	// Access control check
	if s.grant != nil {
		if err := shared.ValidateOracleQuery(sql, s.grant); err != nil {
			s.logger.WarnContext(s.ctx, "query blocked by access control",
				slog.String("sql", truncateSQL(sql, 200)),
				slog.Any("error", err),
			)

			return err
		}
	}

	// Complete previous query if still pending (sets duration)
	s.flushPendingQuery()

	// Approval hold — after the static validators, before anything reaches
	// upstream.
	approvalUID, herr := s.holdIfNeeded(sql)
	if herr != nil {
		return herr
	}

	// Track as pending query and persist immediately
	cursor := &trackedCursor{
		sql:      sql,
		parsedAt: time.Now(),
	}
	s.tracker.pendingQuery = &pendingOracleQuery{
		cursor:    cursor,
		startTime: time.Now(),
	}

	// An approval hold already inserted the row; reuse it rather than writing
	// a second one for the same statement.
	if approvalUID != uuid.Nil {
		s.tracker.pendingQuery.queryUID = approvalUID
		s.tracker.pendingQuery.queryPersisted = true
	} else {
		s.persistQueryRecord()
	}

	return nil
}

// handleQueryResultV2 processes a v315+ QueryResult (func=0x10) response.
// Extracts column names and row values, stores them as query results.
// For large result sets, rows arrive across multiple response packets.
// We accumulate rows until we see ORA-01403 (no data found) which signals end of data.
func (s *session) handleQueryResultV2(ttcPayload []byte) {
	result := decodeQueryResultV2(ttcPayload)
	if result == nil {
		return
	}

	// Store column definitions from the first response (they only appear once).
	// The type code (when known) feeds type-aware value decoding of continuation
	// packets, which carry no describe of their own.
	if s.tracker.pendingQuery != nil && s.tracker.pendingQuery.cursor != nil && len(result.Columns) > 0 {
		columns := make([]columnDef, len(result.Columns))
		for i, name := range result.Columns {
			columns[i] = columnDef{Name: name}
			if i < len(result.ColumnTypes) {
				columns[i].TypeCode = uint8(result.ColumnTypes[i])
			}
		}

		s.tracker.pendingQuery.cursor.columns = columns
	}

	// Capture rows from the QueryResult response.
	if s.tracker.pendingQuery != nil && s.tracker.pendingQuery.cursor != nil {
		columns := s.tracker.pendingQuery.cursor.columns
		for _, row := range result.Rows {
			values := make([]interface{}, len(row))
			for i, v := range row {
				values[i] = v
			}

			s.captureRow(columns, values)
		}

		// Store last row for continuation packet duplicate tracking
		if len(result.Rows) > 0 {
			s.tracker.pendingQuery.lastRow = result.Rows[len(result.Rows)-1]
		}
	}

	// Only complete the query when we see the end-of-data marker (ORA-01403)
	if result.NoData {
		s.completeQuery(nil, nil)
	}
}

// handleOFETCH intercepts an OFETCH message: links the fetch to its cursor.
//
// Returns a non-nil error when the fetch must not be forwarded; the caller
// answers the client with a TTC error instead.
func (s *session) handleOFETCH(ttcPayload []byte) error {
	result, err := decodeOFETCH(ttcPayload)
	if err != nil {
		s.logger.WarnContext(s.ctx, "failed to decode OFETCH", slog.Any("error", err))

		return nil
	}

	// A fetch that CONTINUES a query already in flight is simply more rows of a
	// statement that has already been through the gate. It is deliberately left
	// alone: re-validating here would risk parking a client mid-result-set,
	// which the hold must never do.
	if s.tracker.pendingQuery != nil {
		return nil
	}

	cursor, ok := s.tracker.cursors[result.CursorID]
	if !ok {
		s.logger.DebugContext(s.ctx, "OFETCH for unknown cursor", slog.Uint64("cursor_id", uint64(result.CursorID)))

		return nil
	}

	// No query in flight: this fetch re-executes the cursor and starts its own
	// pending query, which is persisted as a distinct row in /queries. Anything
	// recorded as its own query is gated as its own query, or the audit trail
	// and the enforcement disagree.
	return s.regateCursor(cursor)
}

// handleOCLOSE cleans up cursor tracking when a cursor is closed.
func (s *session) handleOCLOSE(cursorID uint16) {
	delete(s.tracker.cursors, cursorID)
}

// captureRow hands a single captured row to the shared batching writer.
//
// The submit is non-blocking: if the writer's queue is full the row is dropped
// and the capture is marked degraded, because this runs inline with forwarding
// rows to the client and a slow moment in dbbat's own storage must never
// become a stall on the customer's query. Rows are never accumulated in
// memory here — the writer's bounded queue is the only buffer.
func (s *session) captureRow(columns []columnDef, values []interface{}) {
	pending := s.tracker.pendingQuery
	if pending == nil || pending.truncated {
		return
	}

	if !s.queryStorage.StoreResults {
		return
	}

	// Ensure the query record exists in the database: query_rows.query_id is
	// a foreign key, so the parent has to be there before any row flushes.
	if !pending.queryPersisted {
		s.persistQueryRecord()
	}

	if pending.rowSink == nil {
		return // No query record — nowhere to hang the rows
	}

	rowData := make(map[string]interface{})
	rowSize := int64(0)

	for i, col := range columns {
		var val interface{}
		if i < len(values) {
			val = values[i]
		}

		if val != nil {
			valStr := fmt.Sprintf("%v", val)
			rowSize += int64(len(valStr))
		}

		rowData[col.Name] = val
	}

	// Check limits
	if pending.rowNumber >= s.queryStorage.MaxResultRows ||
		pending.capturedBytes+rowSize > s.queryStorage.MaxResultBytes {
		pending.truncated = true

		return
	}

	jsonData, err := json.Marshal(rowData)
	if err != nil {
		return
	}

	pending.rowNumber++
	pending.capturedBytes += rowSize

	// Queue the row for the next batch. Row numbering is a producer-side
	// running counter (pending.rowNumber), so it stays correct across flush
	// boundaries.
	pending.rowSink.Add(store.QueryRow{
		RowNumber:    pending.rowNumber,
		RowData:      jsonData,
		RowSizeBytes: rowSize,
	})
}

// persistQueryRecord creates the query record in the database before streaming rows.
func (s *session) persistQueryRecord() {
	pending := s.tracker.pendingQuery
	if pending == nil || pending.queryPersisted || pending.cursor == nil || s.store == nil {
		return
	}

	pending.queryPersisted = true

	query := &store.Query{
		ConnectionID: s.connectionUID,
		SQLText:      pending.cursor.sql,
		ExecutedAt:   pending.startTime,
		Parameters:   formatOracleBinds(pending.cursor.bindValues),
	}

	created, err := s.store.CreateQuery(s.ctx, query)
	if err != nil {
		s.logger.ErrorContext(s.ctx, "failed to create query record", slog.Any("error", err))
		return
	}

	pending.queryUID = created.UID
	pending.rowSink = s.rowWriter.NewSinkFor(created.UID)

	s.stream.Query(created.UID, query)
}

// queryCompletionStore is the slice of the store that query completion writes
// through: the create-on-completion path, the update-after-streaming path, and
// the connection byte counter. It exists so the completion path can be driven
// by a unit test — the error text a completed query records is the property
// this package's mid-fetch handling hinges on.
type queryCompletionStore interface {
	CreateQuery(ctx context.Context, query *store.Query) (*store.Query, error)
	UpdateQueryCompletion(
		ctx context.Context,
		uid uuid.UUID,
		durationMs *float64,
		rowsAffected *int64,
		queryError *string,
		resultsTruncated bool,
		resultsDropped bool,
	) error
	IncrementConnectionStats(ctx context.Context, uid uuid.UUID, bytes int64) error
}

// completeQuery finalizes a query record with duration and updates connection stats.
// Rows have already been streamed to the database during capture.
//
// bytes_transferred is computed from the wire-level CountingConn around the
// client socket — this captures TNS framing, AUTH/OALL8/OFETCH payloads and
// error packets, fixing the previous undercount that summed only response
// payload lengths.
func (s *session) completeQuery(rowsAffected *int64, queryError *string) {
	pending := s.tracker.pendingQuery
	if pending == nil || pending.cursor == nil {
		return
	}

	s.tracker.pendingQuery = nil

	// Last gate before the store: never persist an "error" that is not readable
	// text. See shared.SanitizeQueryError.
	queryError = shared.SanitizeQueryError(s.ctx, s.logger, queryError)

	total := s.cumulativeClientBytes()
	bytesTransferred := total - s.lastBytesSnapshot
	s.lastBytesSnapshot = total

	duration := float64(time.Since(pending.startTime).Milliseconds())

	// Use captured row count as rows_affected if not provided by the caller.
	if rowsAffected == nil && pending.rowNumber > 0 {
		rc := int64(pending.rowNumber)
		rowsAffected = &rc
	}

	// If the query record was already created (rows were streamed), update it.
	// Otherwise, create it now (no-result queries like DML).
	if pending.queryPersisted && pending.queryUID != uuid.Nil {
		// Update with duration, error, rows affected. results_truncated is only
		// known now: the row was inserted before any row was streamed.
		if s.completionStore != nil {
			go s.finalizeQuery(pending.queryUID, pending.rowSink, &duration, rowsAffected, queryError, pending.truncated, bytesTransferred)
		}
	} else if s.completionStore != nil {
		// Create the query record (no rows to stream)
		query := &store.Query{
			ConnectionID:     s.connectionUID,
			SQLText:          pending.cursor.sql,
			ExecutedAt:       pending.startTime,
			DurationMs:       &duration,
			RowsAffected:     rowsAffected,
			Error:            queryError,
			Parameters:       formatOracleBinds(pending.cursor.bindValues),
			ResultsTruncated: pending.truncated,
		}

		go func() {
			if _, err := s.completionStore.CreateQuery(s.ctx, query); err != nil {
				s.logger.ErrorContext(s.ctx, "failed to log query", slog.Any("error", err))
			}

			if err := s.completionStore.IncrementConnectionStats(s.ctx, s.connectionUID, bytesTransferred); err != nil {
				s.logger.ErrorContext(s.ctx, "failed to increment connection stats", slog.Any("error", err))
			}
		}()
	}

	// Update local grant state for in-session quota checks
	if s.grant != nil {
		s.grant.QueryCount++
		s.grant.BytesTransferred += bytesTransferred
	}
}

// finalizeQuery updates a query record with completion data (duration, error,
// whether row capture stopped on a storage limit, and whether the row writer
// had to drop rows).
//
// It runs in its own goroutine, which is what makes the flush barrier
// affordable: the tail of the capture must land before the query reads as
// complete, or the UI shows a finished query with rows still arriving.
func (s *session) finalizeQuery(
	queryUID uuid.UUID,
	rowSink *shared.QuerySink,
	duration *float64,
	rowsAffected *int64,
	queryError *string,
	resultsTruncated bool,
	bytesTransferred int64,
) {
	rowSink.Flush(s.ctx)

	if err := s.completionStore.UpdateQueryCompletion(
		s.ctx, queryUID, duration, rowsAffected, queryError, resultsTruncated, rowSink.Dropped(),
	); err != nil {
		s.logger.ErrorContext(s.ctx, "failed to finalize query", slog.Any("error", err))
	}

	if err := s.completionStore.IncrementConnectionStats(s.ctx, s.connectionUID, bytesTransferred); err != nil {
		s.logger.ErrorContext(s.ctx, "failed to increment connection stats", slog.Any("error", err))
	}
}

// writeTTCError sends a TTC error response to the client.
// This creates a response that Oracle clients (sqlplus, JDBC) can parse.
// Format: TNS Data packet with TTC Response function code + error info.
func (s *session) writeTTCError(oraErrorCode int, message string) error {
	// Build TTC error response payload:
	// [data flags: 2 bytes] [func code: 1 byte = 0x08 Response]
	// [sequence number: 1 byte] [error code: 4 bytes BE]
	// [cursor ID: 2 bytes] [row count: 4 bytes]
	// [error flag: 2 bytes] [error message length: 2 bytes] [error message]
	errMsg := fmt.Sprintf("ORA-%05d: %s", oraErrorCode, message)
	buf := make([]byte, 0, 18+len(errMsg))

	// Data flags
	buf = append(buf, 0x00, 0x00)

	// TTC function code: Response
	buf = append(buf, byte(TTCFuncResponse))

	// Sequence number
	buf = append(buf, 0x01)

	// Error code (4 bytes, big-endian)
	errCode := make([]byte, 4)
	binary.BigEndian.PutUint32(errCode, uint32(oraErrorCode))
	buf = append(buf, errCode...)

	// Cursor ID (2 bytes) — 0 for error
	buf = append(buf, 0x00, 0x00)

	// Row count (4 bytes) — 0 for error
	buf = append(buf, 0x00, 0x00, 0x00, 0x00)

	// Error flag (2 bytes) — non-zero indicates error
	buf = append(buf, 0x00, 0x01)

	// Error message: ORA-NNNNN: message
	msgLen := make([]byte, 2)
	binary.BigEndian.PutUint16(msgLen, uint16(len(errMsg)))
	buf = append(buf, msgLen...)
	buf = append(buf, []byte(errMsg)...)

	pkt := &TNSPacket{
		Type:    TNSPacketTypeData,
		Payload: buf,
	}

	return writeTNSPacket(s.clientConn, pkt)
}

// sendOracleError sends an ORA-01031 (insufficient privileges) error to the client.
func (s *session) sendOracleError(queryErr error) error {
	return s.writeTTCError(1031, queryErr.Error())
}

// formatOracleBinds formats bind values for storage.
func formatOracleBinds(values []string) *store.QueryParameters {
	if len(values) == 0 {
		return nil
	}

	return &store.QueryParameters{
		Values: values,
	}
}

// truncateSQL truncates SQL for logging purposes.
func truncateSQL(sql string, maxLen int) string {
	if len(sql) <= maxLen {
		return sql
	}

	return sql[:maxLen] + "..."
}

// parseResponseRowsAffected attempts to extract rows affected from a TTC Response.
func parseResponseRowsAffected(ttcPayload []byte) *int64 {
	if len(ttcPayload) < 10 {
		return nil
	}

	// Row count at offset 8 (func + seq + errcode + cursor)
	rowCount := int64(binary.BigEndian.Uint32(ttcPayload[8:12]))
	if rowCount > 0 {
		return &rowCount
	}

	return nil
}

// decodeCursorIDFromOCLOSE extracts cursor ID from an OCLOSE payload.
// OCLOSE layout: func(1) + cursorID(2)
func decodeCursorIDFromOCLOSE(ttcPayload []byte) (uint16, error) {
	if len(ttcPayload) < 3 {
		return 0, fmt.Errorf("%w: OCLOSE needs at least 3 bytes", ErrTTCPayloadTooShort)
	}

	return binary.BigEndian.Uint16(ttcPayload[1:3]), nil
}

// checkQuotas checks whether quota limits have been reached. It also enforces
// the grant's expiry between commands: expiry is otherwise only validated at
// connect time, so without this a session opened just before expiry could keep
// issuing queries indefinitely.
func (s *session) checkQuotas() error {
	// A grant revoked mid-session invalidates every subsequent command, even
	// before the watchdog force-closes the connection.
	if s.revocation.Revoked() {
		return shared.ErrGrantRevoked
	}

	if s.grant == nil {
		return nil
	}

	if !s.grant.ExpiresAt.IsZero() && !time.Now().Before(s.grant.ExpiresAt) {
		return shared.ErrGrantExpired
	}

	if maxQueries := s.grant.MaxQueryCounts(); maxQueries != nil && s.grant.QueryCount >= *maxQueries {
		return ErrQueryLimitExceed
	}

	if maxBytes := s.grant.MaxBytesTransferred(); maxBytes != nil && s.grant.BytesTransferred >= *maxBytes {
		return ErrDataLimitExceed
	}

	return nil
}

// holdIfNeeded runs the approval gate for one statement, after the static
// validators and before anything is forwarded upstream. Returns the uid of the
// pending row the gate created (uuid.Nil when no hold happened).
func (s *session) holdIfNeeded(sql string) (uuid.UUID, error) {
	if !s.approvalGate.Active() {
		return uuid.Nil, nil
	}

	pattern, matched := s.approvalGate.Match(sql)
	if !matched {
		return uuid.Nil, nil
	}

	var gone <-chan struct{}

	if s.watched != nil {
		gone = s.watched.Park()
		defer s.watched.Unpark()
	}

	return s.approvalGate.Hold(s.ctx, shared.HoldRequest{
		SQL:        sql,
		Pattern:    pattern,
		StartedAt:  time.Now(),
		ClientGone: gone,
		Guard:      s.guard,
		OnPending:  s.setHeldQuery,
	})
}

// setHeldQuery records the currently parked statement so an out-of-band
// cancellation path can end it.
func (s *session) setHeldQuery(uid uuid.UUID) {
	s.heldMu.Lock()
	s.heldQueryUID = uid
	s.heldMu.Unlock()
}
