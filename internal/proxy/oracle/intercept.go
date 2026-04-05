package oracle

import (
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
	s.persistQueryRecord()

	return nil
}

// flushPendingQuery completes any outstanding query that hasn't been finalized.
// Called before starting a new query to ensure the previous one is persisted.
func (s *session) flushPendingQuery() {
	if s.tracker.pendingQuery != nil {
		s.completeQuery(nil, nil, 0)
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
func (s *session) handleQueryResultV2(ttcPayload []byte, bytesTransferred int64) {
	result := decodeQueryResultV2(ttcPayload)
	if result == nil {
		return
	}

	// Store column definitions from the first response (they only appear once)
	if s.tracker.pendingQuery != nil && s.tracker.pendingQuery.cursor != nil && len(result.Columns) > 0 {
		columns := make([]columnDef, len(result.Columns))
		for i, name := range result.Columns {
			columns[i] = columnDef{Name: name}
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
		s.completeQuery(nil, nil, bytesTransferred)
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

// captureRow captures a single row and streams it to the database immediately.
// No rows are buffered in memory.
func (s *session) captureRow(columns []columnDef, values []interface{}) {
	pending := s.tracker.pendingQuery
	if pending == nil || pending.truncated || s.store == nil {
		return
	}

	if !s.queryStorage.StoreResults {
		return
	}

	// Ensure the query record exists in the database
	if !pending.queryPersisted {
		s.persistQueryRecord()
	}

	if pending.queryUID == uuid.Nil {
		return // Query record creation failed
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

	// Stream row directly to database
	row := store.QueryRow{
		RowNumber:    pending.rowNumber,
		RowData:      jsonData,
		RowSizeBytes: rowSize,
	}

	if err := s.store.StoreQueryRows(s.ctx, pending.queryUID, []store.QueryRow{row}); err != nil {
		s.logger.WarnContext(s.ctx, "failed to stream row", slog.Any("error", err))
	}
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
	}

	created, err := s.store.CreateQuery(s.ctx, query)
	if err != nil {
		s.logger.ErrorContext(s.ctx, "failed to create query record", slog.Any("error", err))
		return
	}

	pending.queryUID = created.UID
}

// completeQuery finalizes a query record with duration and updates connection stats.
// Rows have already been streamed to the database during capture.
func (s *session) completeQuery(rowsAffected *int64, queryError *string, bytesTransferred int64) {
	pending := s.tracker.pendingQuery
	if pending == nil || pending.cursor == nil {
		return
	}

	s.tracker.pendingQuery = nil

	duration := float64(time.Since(pending.startTime).Milliseconds())

	// Use captured row count as rows_affected if not provided by the caller.
	if rowsAffected == nil && pending.rowNumber > 0 {
		rc := int64(pending.rowNumber)
		rowsAffected = &rc
	}

	// If the query record was already created (rows were streamed), update it.
	// Otherwise, create it now (no-result queries like DML).
	if pending.queryPersisted && pending.queryUID != uuid.Nil {
		// Update with duration, error, rows affected
		go s.finalizeQuery(pending.queryUID, &duration, rowsAffected, queryError, bytesTransferred)
	} else if s.store != nil {
		// Create the query record (no rows to stream)
		query := &store.Query{
			ConnectionID: s.connectionUID,
			SQLText:      pending.cursor.sql,
			ExecutedAt:   pending.startTime,
			DurationMs:   &duration,
			RowsAffected: rowsAffected,
			Error:        queryError,
		}

		go func() {
			if _, err := s.store.CreateQuery(s.ctx, query); err != nil {
				s.logger.ErrorContext(s.ctx, "failed to log query", slog.Any("error", err))
			}

			if err := s.store.IncrementConnectionStats(s.ctx, s.connectionUID, bytesTransferred); err != nil {
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

// finalizeQuery updates a query record with completion data (duration, error).
func (s *session) finalizeQuery(queryUID uuid.UUID, duration *float64, rowsAffected *int64, queryError *string, bytesTransferred int64) {
	if err := s.store.UpdateQueryCompletion(s.ctx, queryUID, duration, rowsAffected, queryError); err != nil {
		s.logger.ErrorContext(s.ctx, "failed to finalize query", slog.Any("error", err))
	}

	if err := s.store.IncrementConnectionStats(s.ctx, s.connectionUID, bytesTransferred); err != nil {
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

// parseResponseError attempts to extract an error message from a TTC Response payload.
// Returns the error string if present, or nil if no error.
func parseResponseError(ttcPayload []byte) *string {
	// Minimum response: func(1) + seq(1) + errcode(4) + cursor(2) + rowcount(4) + errflag(2) = 14
	if len(ttcPayload) < 14 {
		return nil
	}

	// Error code at offset 2 (after func code + sequence)
	errCode := binary.BigEndian.Uint32(ttcPayload[2:6])
	if errCode == 0 {
		return nil
	}

	// Error flag at offset 12
	errFlag := binary.BigEndian.Uint16(ttcPayload[12:14])
	if errFlag == 0 {
		return nil
	}

	// Error message length at offset 14
	if len(ttcPayload) < 16 {
		errStr := fmt.Sprintf("ORA-%05d", errCode)
		return &errStr
	}

	msgLen := binary.BigEndian.Uint16(ttcPayload[14:16])

	if len(ttcPayload) >= 16+int(msgLen) && msgLen > 0 {
		errStr := string(ttcPayload[16 : 16+msgLen])
		return &errStr
	}

	errStr := fmt.Sprintf("ORA-%05d", errCode)

	return &errStr
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

// checkQuotas checks whether quota limits have been reached.
func (s *session) checkQuotas() error {
	if s.grant == nil {
		return nil
	}

	if s.grant.MaxQueryCounts != nil && s.grant.QueryCount >= *s.grant.MaxQueryCounts {
		return ErrQueryLimitExceed
	}

	if s.grant.MaxBytesTransferred != nil && s.grant.BytesTransferred >= *s.grant.MaxBytesTransferred {
		return ErrDataLimitExceed
	}

	return nil
}
