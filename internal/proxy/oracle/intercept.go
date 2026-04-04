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

	// Track the cursor
	cursor := &trackedCursor{
		cursorID:   result.CursorID,
		sql:        result.SQL,
		bindValues: result.BindValues,
		parsedAt:   time.Now(),
	}
	s.tracker.cursors[result.CursorID] = cursor

	// Start pending query
	s.tracker.pendingQuery = &pendingOracleQuery{
		cursor:    cursor,
		startTime: time.Now(),
	}

	return nil
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

	// Track as pending query
	cursor := &trackedCursor{
		sql:      result.SQL,
		parsedAt: time.Now(),
	}
	s.tracker.pendingQuery = &pendingOracleQuery{
		cursor:    cursor,
		startTime: time.Now(),
	}

	return nil
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

	// Capture rows using stored column definitions
	if s.tracker.pendingQuery != nil && s.tracker.pendingQuery.cursor != nil {
		columns := s.tracker.pendingQuery.cursor.columns
		for _, row := range result.Rows {
			values := make([]interface{}, len(row))
			for i, v := range row {
				values[i] = v
			}

			s.captureRow(columns, values)
		}
	}

	// Only complete the query when we see the end-of-data marker (ORA-01403)
	if result.NoData {
		s.completeQuery(nil, nil, bytesTransferred)
	}
}

// captureRowsFromContinuation extracts row data from continuation packets
// in multi-packet result sets. Continuation packets (func=0x06) contain:
// [func(1)] [header(~12 bytes)] [row data: len+val pairs separated by 0x07]
//
// The row data uses the same length-prefixed format as QueryResult but
// without column definitions or the 0x06 0x22 marker.
func (s *session) captureRowsFromContinuation(payload []byte, columns []columnDef) { //nolint:gocognit,cyclop // TTC binary parsing
	numCols := len(columns)
	if numCols == 0 || len(payload) < 15 {
		return
	}

	// Skip the TNS data flags (already stripped in extractTTCPayload),
	// then the func code + header. The header varies in length.
	// Scan for the first valid row by trying different start offsets.
	for startOffset := 10; startOffset < 25 && startOffset < len(payload)-numCols; startOffset++ {
		// Try to read one row at this offset
		offset := startOffset
		row := make([]string, 0, numCols)
		valid := true

		for col := 0; col < numCols; col++ {
			if offset >= len(payload) {
				valid = false
				break
			}

			valLen := int(payload[offset])
			offset++

			if valLen == 0 {
				row = append(row, "")
				continue
			}

			if valLen > 4000 || offset+valLen > len(payload) {
				valid = false
				break
			}

			valBytes := payload[offset : offset+valLen]
			offset += valLen
			decoded := decodeOracleRawValue(valBytes)

			// Validate: first column of first row should look reasonable
			if col == 0 && len(row) == 0 && !isReasonableValue(decoded) {
				valid = false
				break
			}

			row = append(row, decoded)
		}

		if !valid || len(row) != numCols {
			continue
		}

		// Found valid row data! Capture this row and continue scanning.
		s.captureRowValues(columns, row)

		// Read remaining rows
		endOfData := len(payload)
		if idx := findBytes(payload, []byte("ORA-01403")); idx >= 0 {
			endOfData = idx
		}

		for offset < endOfData {
			if payload[offset] == 0x08 {
				break // End of rows
			}

			if payload[offset] == 0x07 {
				offset++ // Row separator
			}

			row = make([]string, 0, numCols)
			valid = true

			for col := 0; col < numCols; col++ {
				if offset >= endOfData {
					valid = false
					break
				}

				valLen := int(payload[offset])
				offset++

				if valLen == 0 {
					row = append(row, "")
					continue
				}

				if valLen > 4000 || offset+valLen > endOfData {
					valid = false
					break
				}

				row = append(row, decodeOracleRawValue(payload[offset:offset+valLen]))
				offset += valLen
			}

			if !valid || len(row) != numCols {
				break
			}

			s.captureRowValues(columns, row)
		}

		return // Done
	}
}

// captureRowValues converts string values to interface{} and captures the row.
func (s *session) captureRowValues(columns []columnDef, row []string) {
	values := make([]interface{}, len(row))
	for i, v := range row {
		values[i] = v
	}

	s.captureRow(columns, values)
}

// isReasonableValue checks if a decoded value looks like real data (not header bytes).
func isReasonableValue(s string) bool {
	if len(s) == 0 {
		return true // NULL is OK
	}

	// Reject hex-encoded values (likely misinterpreted header bytes)
	if len(s) > 4 && len(s)%2 == 0 {
		allHex := true
		for _, c := range s {
			isHexChar := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')
			if !isHexChar {
				allHex = false
				break
			}
		}

		if allHex {
			return false
		}
	}

	return true
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
	if pending == nil || pending.queryPersisted || pending.cursor == nil {
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
