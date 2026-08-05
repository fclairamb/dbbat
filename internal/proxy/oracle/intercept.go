package oracle

import (
	"context"
	"encoding/binary"
	"encoding/json"
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
func (s *session) handleJDBCExec(ttcPayload []byte) {
	result, err := decodeExecSQL(ttcPayload)
	if err != nil {
		s.logger.DebugContext(s.ctx, "failed to decode JDBC exec", slog.Any("error", err))
		return
	}

	s.logger.InfoContext(s.ctx, "query intercepted",
		slog.String("sql", truncateSQL(result.SQL, 200)),
		slog.String("source", "jdbc"),
	)

	// Complete previous query if still pending (sets duration)
	s.flushPendingQuery()

	// Track as pending query and persist immediately
	cursor := &trackedCursor{
		sql:      result.SQL,
		parsedAt: time.Now(),
	}
	s.tracker.pendingQuery = &pendingOracleQuery{
		cursor:    cursor,
		startTime: time.Now(),
	}
	s.persistQueryRecord()
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
func (s *session) handleOFETCH(ttcPayload []byte) {
	result, err := decodeOFETCH(ttcPayload)
	if err != nil {
		s.logger.WarnContext(s.ctx, "failed to decode OFETCH", slog.Any("error", err))
		return
	}

	cursor, ok := s.tracker.cursors[result.CursorID]
	if !ok {
		s.logger.DebugContext(s.ctx, "OFETCH for unknown cursor", slog.Uint64("cursor_id", uint64(result.CursorID)))
		return
	}

	// If no pending query, start one for the fetch (re-execution of cursor)
	if s.tracker.pendingQuery == nil {
		s.tracker.pendingQuery = &pendingOracleQuery{
			cursor:    cursor,
			startTime: time.Now(),
		}
	}
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

	if s.grant.MaxQueryCounts != nil && s.grant.QueryCount >= *s.grant.MaxQueryCounts {
		return ErrQueryLimitExceed
	}

	if s.grant.MaxBytesTransferred != nil && s.grant.BytesTransferred >= *s.grant.MaxBytesTransferred {
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
