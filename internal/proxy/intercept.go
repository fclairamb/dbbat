package proxy

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/fclairamb/dbbat/internal/store"
)

// Write keywords that should be blocked for read-only grants.
var writeKeywords = []string{
	"INSERT", "UPDATE", "DELETE", "DROP", "TRUNCATE",
	"CREATE", "ALTER", "GRANT", "REVOKE",
}

// readOnlyBypassPatterns contains regex patterns that detect attempts to disable read-only mode.
var readOnlyBypassPatterns = []*regexp.Regexp{
	// SET [SESSION] default_transaction_read_only (=|TO) (off|false|0)
	regexp.MustCompile(`(?i)\bSET\s+(?:SESSION\s+)?default_transaction_read_only\s*(?:=|TO)\s*(?:off|false|0)\b`),

	// RESET [SESSION] default_transaction_read_only
	regexp.MustCompile(`(?i)\bRESET\s+(?:SESSION\s+)?default_transaction_read_only\b`),

	// SET [SESSION] AUTHORIZATION (privilege escalation)
	regexp.MustCompile(`(?i)\bSET\s+(?:SESSION\s+)?AUTHORIZATION\b`),

	// SET ROLE (privilege escalation)
	regexp.MustCompile(`(?i)\bSET\s+ROLE\b`),
}

// handleQuery intercepts and logs queries - returns nil if query was handled.
func (s *Session) handleQuery(query *pgproto3.Query) error {
	sqlText := query.String

	// Check quotas before executing query
	if err := s.checkQuotas(); err != nil {
		return err
	}

	// Always block password changes regardless of access level
	if isPasswordChangeQuery(sqlText) {
		return ErrPasswordChangeNotAllowed
	}

	// Block attempts to disable read-only mode
	if s.grant.AccessLevel == "read" && isReadOnlyBypassAttempt(sqlText) {
		return ErrReadOnlyBypassAttempt
	}

	// Check for read-only enforcement (defense-in-depth)
	if s.grant.AccessLevel == "read" && isWriteQuery(sqlText) {
		return ErrWriteNotPermitted
	}

	// Start tracking query for logging
	s.currentQuery = &pendingQuery{
		sql:          sqlText,
		startTime:    time.Now(),
		capturedRows: make([]store.QueryRow, 0), // Initialize for capture
	}

	return nil
}

// handleParse handles Parse messages (prepared statement creation) for Extended Query Protocol.
func (s *Session) handleParse(msg *pgproto3.Parse) error {
	sqlText := msg.Query

	// Always block password changes regardless of access level
	if isPasswordChangeQuery(sqlText) {
		return ErrPasswordChangeNotAllowed
	}

	// Block attempts to disable read-only mode
	if s.grant.AccessLevel == "read" && isReadOnlyBypassAttempt(sqlText) {
		return ErrReadOnlyBypassAttempt
	}

	// Check for read-only enforcement at Parse time (defense-in-depth)
	if s.grant.AccessLevel == "read" && isWriteQuery(sqlText) {
		return ErrWriteNotPermitted
	}

	// Store the prepared statement with type OIDs
	s.extendedState.preparedStatements[msg.Name] = &preparedStatement{
		sql:      sqlText,
		typeOIDs: msg.ParameterOIDs,
	}

	return nil
}

// handleBind handles Bind messages (portal creation) for Extended Query Protocol.
func (s *Session) handleBind(msg *pgproto3.Bind) {
	stmt := s.extendedState.preparedStatements[msg.PreparedStatement]

	var typeOIDs []uint32
	if stmt != nil {
		typeOIDs = stmt.typeOIDs
	}

	// Build parameters structure
	var params *store.QueryParameters
	if len(msg.Parameters) > 0 {
		params = &store.QueryParameters{
			Values:      make([]string, len(msg.Parameters)),
			Raw:         make([]string, len(msg.Parameters)),
			FormatCodes: make([]int16, len(msg.Parameters)),
			TypeOIDs:    typeOIDs,
		}

		for i, param := range msg.Parameters {
			// Determine format code (see PostgreSQL protocol spec)
			formatCode := int16(0)
			if len(msg.ParameterFormatCodes) == 1 {
				formatCode = msg.ParameterFormatCodes[0] // All params same format
			} else if i < len(msg.ParameterFormatCodes) {
				formatCode = msg.ParameterFormatCodes[i]
			}

			params.FormatCodes[i] = formatCode
			params.Raw[i] = base64.StdEncoding.EncodeToString(param)

			if formatCode == 0 {
				// Text format - value is directly usable
				params.Values[i] = string(param)
			} else {
				// Binary format - decode based on type OID
				params.Values[i] = decodeBinaryParameter(param, getTypeOID(typeOIDs, i))
			}
		}
	}

	s.extendedState.portals[msg.DestinationPortal] = &portalState{
		stmtName:   msg.PreparedStatement,
		parameters: params,
	}
}

// handleExecute handles Execute messages (query execution) for Extended Query Protocol.
func (s *Session) handleExecute(msg *pgproto3.Execute) error {
	// Check quotas before executing
	if err := s.checkQuotas(); err != nil {
		return err
	}

	// Look up the portal
	portal := s.extendedState.portals[msg.Portal]
	if portal == nil {
		s.logger.Warn("execute for unknown portal", "portal", msg.Portal)
		return nil
	}

	// Look up the statement
	stmt := s.extendedState.preparedStatements[portal.stmtName]
	sqlText := ""
	if stmt != nil {
		sqlText = stmt.sql
	} else {
		s.logger.Warn("execute for unknown statement", "portal", msg.Portal, "stmt", portal.stmtName)
	}

	// Queue the query for logging (will be popped on CommandComplete)
	query := &pendingQuery{
		sql:          sqlText,
		startTime:    time.Now(),
		parameters:   portal.parameters,
		capturedRows: make([]store.QueryRow, 0), // Initialize for capture
	}
	s.extendedState.pendingQueries = append(s.extendedState.pendingQueries, query)

	return nil
}

// handleClose handles Close messages (cleanup) for Extended Query Protocol.
func (s *Session) handleClose(msg *pgproto3.Close) {
	switch msg.ObjectType {
	case 'S': // Statement
		delete(s.extendedState.preparedStatements, msg.Name)
	case 'P': // Portal
		delete(s.extendedState.portals, msg.Name)
	}
}

// isWriteQuery checks if a query is a write operation.
func isWriteQuery(sql string) bool {
	upper := strings.ToUpper(strings.TrimSpace(sql))

	for _, keyword := range writeKeywords {
		if strings.HasPrefix(upper, keyword) {
			return true
		}
	}

	return false
}

// isReadOnlyBypassAttempt checks if a query attempts to disable read-only mode.
func isReadOnlyBypassAttempt(sql string) bool {
	for _, pattern := range readOnlyBypassPatterns {
		if pattern.MatchString(sql) {
			return true
		}
	}

	return false
}

// isPasswordChangeQuery checks if a query attempts to modify user/role passwords.
// This blocks: ALTER USER/ROLE ... PASSWORD, ALTER USER/ROLE ... ENCRYPTED PASSWORD
func isPasswordChangeQuery(sql string) bool {
	upper := strings.ToUpper(strings.TrimSpace(sql))

	// Check for ALTER USER or ALTER ROLE with PASSWORD
	if (strings.HasPrefix(upper, "ALTER USER") || strings.HasPrefix(upper, "ALTER ROLE")) &&
		strings.Contains(upper, "PASSWORD") {
		return true
	}

	return false
}

// parseRowsAffected extracts the row count from a CommandComplete message tag.
// CommandTag format: "COMMAND [count]"
// Examples: "UPDATE 5", "DELETE 10", "INSERT 0 1", "SELECT 100".
func parseRowsAffected(commandTag string) *int64 {
	parts := strings.Fields(commandTag)
	if len(parts) >= 2 {
		// Try last part first (handles "INSERT 0 1" format)
		if n, err := strconv.ParseInt(parts[len(parts)-1], 10, 64); err == nil {
			return &n
		}
	}

	return nil
}

// logQuery persists the query record to the store.
func (s *Session) logQuery(rowsAffected *int64, queryError *string, bytesTransferred int64) {
	if s.currentQuery == nil {
		return
	}

	duration := float64(time.Since(s.currentQuery.startTime).Milliseconds())

	query := &store.Query{
		ConnectionID: s.connectionUID,
		SQLText:      s.currentQuery.sql,
		Parameters:   s.currentQuery.parameters,
		ExecutedAt:   s.currentQuery.startTime,
		DurationMs:   &duration,
		RowsAffected: rowsAffected,
		Error:        queryError,
	}

	// Set COPY metadata if this was a COPY operation
	if s.copyState != nil {
		query.CopyDirection = &s.copyState.direction
		format := copyFormatToString(s.copyState.format)
		query.CopyFormat = &format
	}

	// Capture rows - either from regular query or COPY operation
	var capturedRows []store.QueryRow
	if s.copyState != nil && !s.copyState.truncated && len(s.copyState.dataChunks) > 0 {
		// Parse COPY data into rows
		capturedRows = s.parseCopyDataToRows()
	} else {
		// Regular query rows
		capturedRows = s.currentQuery.capturedRows
	}

	// Log asynchronously to not block proxy
	go func() {
		createdQuery, err := s.store.CreateQuery(s.ctx, query)
		if err != nil {
			s.logger.Error("failed to log query", "error", err)
			return
		}

		// Store captured result rows
		if len(capturedRows) > 0 {
			// Assign row numbers
			for i := range capturedRows {
				capturedRows[i].RowNumber = i + 1
			}

			if err := s.store.StoreQueryRows(s.ctx, createdQuery.UID, capturedRows); err != nil {
				s.logger.Error("failed to store query rows", "error", err)
			}
		}

		// Update connection stats
		if err := s.store.IncrementConnectionStats(s.ctx, s.connectionUID, bytesTransferred); err != nil {
			s.logger.Error("failed to increment connection stats", "error", err)
		}
	}()

	// Update local grant state for in-session quota checks
	s.grant.QueryCount++
	s.grant.BytesTransferred += bytesTransferred
}

// copyFormatToString converts COPY format byte to string.
func copyFormatToString(format byte) string {
	switch format {
	case 0:
		return "text"
	case 1:
		return "binary"
	default:
		return "unknown"
	}
}

// decodeBinaryParameter decodes a binary-format PostgreSQL parameter.
func decodeBinaryParameter(data []byte, oid uint32) string {
	if len(data) == 0 {
		return ""
	}

	switch oid {
	case 16: // bool
		if data[0] == 1 {
			return "true"
		}
		return "false"

	case 21: // int2
		if len(data) >= 2 {
			return strconv.FormatInt(int64(int16(binary.BigEndian.Uint16(data))), 10)
		}

	case 23: // int4
		if len(data) >= 4 {
			return strconv.FormatInt(int64(int32(binary.BigEndian.Uint32(data))), 10)
		}

	case 20: // int8
		if len(data) >= 8 {
			return strconv.FormatInt(int64(binary.BigEndian.Uint64(data)), 10)
		}

	case 700: // float4
		if len(data) >= 4 {
			bits := binary.BigEndian.Uint32(data)
			return strconv.FormatFloat(float64(math.Float32frombits(bits)), 'g', -1, 32)
		}

	case 701: // float8
		if len(data) >= 8 {
			bits := binary.BigEndian.Uint64(data)
			return strconv.FormatFloat(math.Float64frombits(bits), 'g', -1, 64)
		}

	case 25, 1042, 1043: // text, char, varchar
		return string(data)

	case 17: // bytea
		return base64.StdEncoding.EncodeToString(data)
	}

	// Unknown type - return base64 with type hint
	return fmt.Sprintf("(oid:%d)%s", oid, base64.StdEncoding.EncodeToString(data))
}

// getTypeOID safely retrieves a type OID from a slice.
func getTypeOID(oids []uint32, idx int) uint32 {
	if idx < len(oids) {
		return oids[idx]
	}
	return 0 // Unknown
}

// convertDataRow converts a DataRow to a QueryRow with JSON data.
func (s *Session) convertDataRow(values [][]byte, columnNames []string, columnOIDs []uint32) store.QueryRow {
	rowData := make(map[string]interface{})
	rowSize := int64(0)

	for i, val := range values {
		rowSize += int64(len(val))

		columnName := fmt.Sprintf("col_%d", i)
		if i < len(columnNames) {
			columnName = columnNames[i]
		}

		if val == nil {
			rowData[columnName] = nil
			continue
		}

		// Decode based on type OID (values are in text format by default)
		var oid uint32
		if i < len(columnOIDs) {
			oid = columnOIDs[i]
		}

		rowData[columnName] = decodeColumnValue(val, oid)
	}

	jsonData, err := json.Marshal(rowData)
	if err != nil {
		// Fallback: store error message
		jsonData = []byte(fmt.Sprintf(`{"error":"marshal failed: %s"}`, err.Error()))
	}

	return store.QueryRow{
		RowData:      jsonData,
		RowSizeBytes: rowSize,
	}
}

// captureCopyData captures a COPY data chunk, respecting storage limits.
func (s *Session) captureCopyData(data []byte) {
	if s.copyState == nil || s.copyState.truncated || !s.queryStorage.StoreResults {
		return
	}

	dataSize := int64(len(data))

	// Check if this chunk would exceed limits
	if s.copyState.totalBytes+dataSize > s.queryStorage.MaxResultBytes {
		s.copyState.truncated = true
		s.copyState.dataChunks = nil // Discard captured data
		s.logger.Warn("COPY data capture truncated - byte limit exceeded",
			"total_bytes", s.copyState.totalBytes,
			"max_bytes", s.queryStorage.MaxResultBytes)
		return
	}

	// Store the chunk
	s.copyState.dataChunks = append(s.copyState.dataChunks, append([]byte(nil), data...))
	s.copyState.totalBytes += dataSize
}

// parseCopyColumnNames extracts column names from a COPY query.
// Examples:
// - "COPY table (col1, col2) TO STDOUT" -> ["col1", "col2"]
// - "COPY table TO STDOUT" -> nil (all columns)
func parseCopyColumnNames(sql string) []string {
	// Find the column list between parentheses before TO/FROM
	upper := strings.ToUpper(sql)
	toIdx := strings.Index(upper, " TO ")
	fromIdx := strings.Index(upper, " FROM ")

	endIdx := -1
	if toIdx > 0 {
		endIdx = toIdx
	} else if fromIdx > 0 {
		endIdx = fromIdx
	}

	if endIdx < 0 {
		return nil
	}

	// Look for parentheses in the part before TO/FROM
	prefix := sql[:endIdx]
	start := strings.Index(prefix, "(")
	end := strings.LastIndex(prefix, ")")

	if start < 0 || end < start {
		return nil
	}

	// Extract column list and split by comma
	colList := prefix[start+1 : end]
	cols := strings.Split(colList, ",")
	result := make([]string, 0, len(cols))

	for _, col := range cols {
		trimmed := strings.TrimSpace(col)
		if trimmed != "" {
			result = append(result, trimmed)
		}
	}

	if len(result) == 0 {
		return nil
	}

	return result
}

// parseCopyDataToRows parses text/CSV format COPY data into query rows.
// Format 0 = text (tab-separated), Format 1 = binary
func (s *Session) parseCopyDataToRows() []store.QueryRow {
	if s.copyState == nil || len(s.copyState.dataChunks) == 0 || s.copyState.format == 1 {
		// No data or binary format (not parsed)
		return nil
	}

	// Concatenate all chunks
	var fullData []byte
	for _, chunk := range s.copyState.dataChunks {
		fullData = append(fullData, chunk...)
	}

	// Split into lines (COPY text format uses \n as row separator)
	lines := strings.Split(string(fullData), "\n")
	rows := make([]store.QueryRow, 0, len(lines))

	for i, line := range lines {
		if line == "" || line == "\\." {
			// Empty line or end-of-data marker
			continue
		}

		// Check max rows limit
		if len(rows) >= s.queryStorage.MaxResultRows {
			s.logger.Warn("COPY row capture truncated - row limit exceeded",
				"rows_captured", len(rows),
				"max_rows", s.queryStorage.MaxResultRows)
			break
		}

		// Parse tab-separated values (default COPY format)
		fields := strings.Split(line, "\t")
		rowData := make(map[string]interface{})

		for j, field := range fields {
			colName := fmt.Sprintf("col_%d", j)
			if j < len(s.copyState.columnNames) {
				colName = s.copyState.columnNames[j]
			}

			// Handle NULL representation in COPY format
			if field == "\\N" {
				rowData[colName] = nil
			} else {
				// Unescape COPY text format
				rowData[colName] = unescapeCopyText(field)
			}
		}

		jsonData, err := json.Marshal(rowData)
		if err != nil {
			s.logger.Error("failed to marshal COPY row", "error", err, "row", i)
			continue
		}

		rows = append(rows, store.QueryRow{
			RowNumber:    i + 1,
			RowData:      jsonData,
			RowSizeBytes: int64(len(line)),
		})
	}

	return rows
}

// unescapeCopyText unescapes PostgreSQL COPY text format escape sequences.
func unescapeCopyText(s string) string {
	// COPY text format escapes:
	// \\ -> \
	// \n -> newline
	// \r -> carriage return
	// \t -> tab
	// \b -> backspace
	// \f -> form feed
	// \N -> NULL (handled separately)
	replacer := strings.NewReplacer(
		"\\\\", "\\",
		"\\n", "\n",
		"\\r", "\r",
		"\\t", "\t",
		"\\b", "\b",
		"\\f", "\f",
	)
	return replacer.Replace(s)
}

// decodeColumnValue decodes a column value based on its type OID.
// Values from DataRow are typically in text format.
func decodeColumnValue(data []byte, oid uint32) interface{} {
	str := string(data)

	switch oid {
	case 16: // bool
		return str == "t" || str == "true" || str == "1"

	case 21, 23, 20: // int2, int4, int8
		if n, err := strconv.ParseInt(str, 10, 64); err == nil {
			return n
		}
		return str

	case 700, 701, 1700: // float4, float8, numeric
		if f, err := strconv.ParseFloat(str, 64); err == nil {
			return f
		}
		return str

	case 17: // bytea
		// Typically in hex format (\x...) or escape format
		return str

	case 114, 3802: // json, jsonb
		// Return raw JSON
		var js json.RawMessage
		if err := json.Unmarshal(data, &js); err == nil {
			return js
		}
		return str

	default:
		// Text types and unknown: return as string
		return str
	}
}
