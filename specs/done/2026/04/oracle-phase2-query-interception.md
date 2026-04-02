# Oracle Proxy — Phase 2: Query Interception & Access Control

> Parent spec: `specs/2026-04-02-oracle-proxy.md`

## Goal

Replace the raw bidirectional relay (Phase 1) with a TTC-aware message router that intercepts query operations. Extract SQL text and bind parameters from OALL8 messages, enforce access controls (read-only, block DDL, Oracle-specific patterns), log every query to the store, and track cursor state across the session.

## Prerequisites

- **Phase 1 complete**: TNS packet reader/writer, connect descriptor parser, server/session skeleton, auth passthrough all working. Client can connect through proxy to Oracle.

## Outcome

- TTC function code parser
- OALL8 (parse+execute) decoder — extracts SQL text, bind values, cursor ID
- OFETCH decoder — links fetch to cursor
- Cursor state tracker (cursor ID → SQL mapping)
- Query validation extracted to `shared/validation.go` (reusable by PG and Oracle)
- Oracle-specific blocked patterns (`ALTER SYSTEM`, `UTL_HTTP`, `DBMS_SCHEDULER`, etc.)
- TTC error encoder (send ORA-style errors to client when blocking)
- Query logging to `store.CreateQuery` with timing and bind parameters
- Connection stats tracking (query count, bytes transferred)

## Non-Goals (deferred to Phase 3)

- Result row capture (column definitions, row data decoding)
- Oracle data type decoders (NUMBER, DATE, etc.)
- LOB handling

---

## Architecture

```
Client ←──TNS──→ DBBat Proxy ←──TNS──→ Oracle Upstream

         ┌─────────────────────────────────────────┐
         │           OracleSession                  │
         │                                          │
         │  goroutine 1: clientToUpstream()         │
         │  ├─ readTNSPacket(clientConn)            │
         │  ├─ if Data packet:                      │
         │  │   ├─ parseTTCFunctionCode()           │
         │  │   ├─ switch functionCode:             │
         │  │   │   case OALL8: interceptOALL8()    │
         │  │   │   case OFETCH: interceptOFETCH()  │
         │  │   │   case OCLOSE: cleanupCursor()    │
         │  │   │   default: pass through           │
         │  │   └─ forward packet to upstream       │
         │  └─ else: forward packet                 │
         │                                          │
         │  goroutine 2: upstreamToClient()         │
         │  ├─ readTNSPacket(upstreamConn)          │
         │  ├─ if Data packet with Response:        │
         │  │   ├─ if error: capture error message  │
         │  │   ├─ if complete: logQuery()          │
         │  │   └─ track bytes transferred          │
         │  └─ forward packet to client             │
         └─────────────────────────────────────────┘
```

## TTC Message Structure

Inside every TNS Data packet:

```
Offset  Size  Field
0       2     Data flags (usually 0x0000)
2       1     TTC function code
3       ...   Function-specific payload
```

### TTC Function Codes

| Code | Name | Direction | Intercepted? |
|------|------|-----------|-------------|
| 0x01 | OSETPRO | Bidir | No (session init, Phase 1) |
| 0x02 | ODTYPES | Bidir | No (session init, Phase 1) |
| 0x03 | OOPEN | C→S | No (pass through) |
| 0x05 | OCLOSE | C→S | **Yes** — clean up cursor tracking |
| 0x08 | Response | S→C | **Yes** — detect errors, completion |
| 0x09 | OMARKER | Bidir | No (pass through) |
| 0x0B | OVERSION | C→S | No (pass through) |
| 0x0E | OALL8 | C→S | **Yes** — extract SQL, binds, cursor ID |
| 0x11 | OFETCH | C→S | **Yes** — link to cursor for timing |
| 0x14 | OCANCEL | C→S | No (pass through) |
| 0x44 | OLOBOPS | Bidir | No (pass through, Phase 3) |
| 0x47 | OSQL7 | C→S | No (pass through) |
| 0x5E | OAUTH | Bidir | No (handled in Phase 1) |
| 0x73 | OSESSKEY | Bidir | No (handled in Phase 1) |

## Cursor-Based State Tracking

In PostgreSQL, every `Query` message contains SQL text. In Oracle, SQL only appears in OALL8 (parse). Subsequent OFETCH operations reference a **cursor ID**. We need a per-session map.

```go
type trackedCursor struct {
    cursorID    uint16
    sql         string
    bindValues  []string
    parsedAt    time.Time
    columnNames []string       // Populated later (Phase 3)
    columnTypes []uint8        // Oracle type codes (Phase 3)
}

type oracleQueryTracker struct {
    cursors       map[uint16]*trackedCursor
    pendingQuery  *pendingOracleQuery
}

type pendingOracleQuery struct {
    cursor        *trackedCursor
    startTime     time.Time
    capturedRows  []store.QueryRow  // Phase 3
    capturedBytes int64
    rowNumber     int
    truncated     bool
}
```

## OALL8 Binary Layout

OALL8 is the primary query message (parse + execute combined). Simplified layout:

```
[TTC header: function code 0x0E]
[options: uint32]
[cursor ID: uint16]
[SQL length: varies]
[SQL text: UTF-8 bytes]         ← EXTRACT THIS
[bind count: uint16]
[bind definitions...]
[bind values...]                ← AND THESE
[define count: uint16]
[column definitions...]
```

Exact byte offsets depend on TTC version negotiated during session setup. Reference: go-ora `command.go` → `execute()`.

## Access Control

### Shared Validation (extracted from PG proxy)

Factor the SQL validation logic out of `internal/proxy/intercept.go` into `internal/proxy/shared/validation.go`:

```go
package shared

func ValidateQuery(sql string, grant *store.Grant) error {
    normalized := strings.ToUpper(strings.TrimSpace(sql))

    if containsPasswordChange(normalized) {
        return ErrPasswordChangeBlocked
    }
    if grant.IsReadOnly() && isWriteQuery(normalized) {
        return ErrReadOnlyViolation
    }
    if grant.ShouldBlockDDL() && isDDLQuery(normalized) {
        return ErrDDLBlocked
    }
    return nil
}
```

### Oracle-Specific Patterns

Additional patterns blocked for Oracle connections (always, regardless of grant controls):

```go
var oracleBlockedPatterns = []*regexp.Regexp{
    regexp.MustCompile(`(?i)ALTER\s+SYSTEM`),
    regexp.MustCompile(`(?i)CREATE\s+DATABASE\s+LINK`),
    regexp.MustCompile(`(?i)DBMS_SCHEDULER`),
    regexp.MustCompile(`(?i)DBMS_JOB`),
    regexp.MustCompile(`(?i)UTL_HTTP`),
    regexp.MustCompile(`(?i)UTL_TCP`),
    regexp.MustCompile(`(?i)UTL_FILE`),
    regexp.MustCompile(`(?i)DBMS_PIPE`),
}

func ValidateOracleQuery(sql string, grant *store.Grant) error {
    if err := ValidateQuery(sql, grant); err != nil {
        return err
    }
    for _, pattern := range oracleBlockedPatterns {
        if pattern.MatchString(sql) {
            return ErrOraclePatternBlocked
        }
    }
    return nil
}
```

### Sending TTC Error Responses

When we block a query, we send back a TTC response that the Oracle client understands:

```go
func (s *OracleSession) sendOracleError(queryErr error) error {
    // TTC response format:
    // - Function code: 0x08 (response)
    // - Error flag set
    // - ORA error number: 1031 (insufficient privileges)
    // - Error message text
    return s.writeTTCError(1031, queryErr.Error())
}
```

## Query Logging

Same pattern as PG proxy — async write to avoid blocking the relay:

```go
func (s *OracleSession) logQuery(cursor *trackedCursor, pending *pendingOracleQuery,
    rowsAffected *int64, queryError *string, bytesTransferred int64) {

    duration := time.Since(pending.startTime).Seconds() * 1000

    query := &store.Query{
        UID:          uuid.Must(uuid.NewV7()),
        ConnectionID: s.connectionUID,
        SQLText:      cursor.sql,
        Parameters:   formatOracleBinds(cursor.bindValues),
        ExecutedAt:   pending.startTime,
        DurationMs:   &duration,
        RowsAffected: rowsAffected,
        Error:        queryError,
    }

    go func() {
        if err := s.store.CreateQuery(s.ctx, query); err != nil {
            s.logger.Error("failed to log query", "error", err)
        }
    }()
}
```

---

## Implementation Steps & Tests

### Step 5: TTC Function Code Identification

Parse TTC function codes from TNS Data packets. Identify and log each function code passing through — no deep decoding yet.

**Files:** `oracle/ttc.go`, `oracle/ttc_test.go`

**Key types:**

```go
type TTCFunctionCode byte

const (
    TTCFuncSetProtocol  TTCFunctionCode = 0x01
    TTCFuncSetDataTypes TTCFunctionCode = 0x02
    TTCFuncOOPEN        TTCFunctionCode = 0x03
    TTCFuncOCLOSE       TTCFunctionCode = 0x05
    TTCFuncResponse     TTCFunctionCode = 0x08
    TTCFuncOALL8        TTCFunctionCode = 0x0E
    TTCFuncOFETCH       TTCFunctionCode = 0x11
    TTCFuncOCANCEL      TTCFunctionCode = 0x14
    TTCFuncOLOBOPS      TTCFunctionCode = 0x44
    TTCFuncOAUTH        TTCFunctionCode = 0x5E
)

func parseTTCFunctionCode(tnsDataPayload []byte) (TTCFunctionCode, error)
func (fc TTCFunctionCode) String() string
func (fc TTCFunctionCode) IsKnown() bool
```

**Tests:**

```go
func TestParseTTCFunctionCode(t *testing.T) {
    tests := []struct {
        name    string
        payload []byte
        want    TTCFunctionCode
    }{
        {"OALL8", buildTTCDataPayload(0x0E, []byte("...")), TTCFuncOALL8},
        {"OFETCH", buildTTCDataPayload(0x11, []byte("...")), TTCFuncOFETCH},
        {"OCLOSE", buildTTCDataPayload(0x05, []byte("...")), TTCFuncOCLOSE},
        {"OAUTH", buildTTCDataPayload(0x5E, []byte("...")), TTCFuncOAUTH},
        {"OOPEN", buildTTCDataPayload(0x03, []byte("...")), TTCFuncOOPEN},
        {"Response", buildTTCDataPayload(0x08, []byte("...")), TTCFuncResponse},
        {"OCANCEL", buildTTCDataPayload(0x14, []byte("...")), TTCFuncOCANCEL},
        {"OLOBOPS", buildTTCDataPayload(0x44, []byte("...")), TTCFuncOLOBOPS},
        {"SetProtocol", buildTTCDataPayload(0x01, []byte("...")), TTCFuncSetProtocol},
        {"SetDataTypes", buildTTCDataPayload(0x02, []byte("...")), TTCFuncSetDataTypes},
    }
    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            fc, err := parseTTCFunctionCode(tt.payload)
            require.NoError(t, err)
            assert.Equal(t, tt.want, fc)
        })
    }
}

func TestParseTTCFunctionCode_EmptyPayload(t *testing.T) {
    _, err := parseTTCFunctionCode([]byte{})
    assert.ErrorIs(t, err, ErrTTCPayloadTooShort)
}

func TestParseTTCFunctionCode_DataFlagsPrefixed(t *testing.T) {
    payload := []byte{0x00, 0x00, 0x0E} // flags=0, func=OALL8
    fc, err := parseTTCFunctionCode(payload)
    require.NoError(t, err)
    assert.Equal(t, TTCFuncOALL8, fc)
}

func TestParseTTCFunctionCode_UnknownCode(t *testing.T) {
    payload := buildTTCDataPayload(0xFE, nil)
    fc, err := parseTTCFunctionCode(payload)
    require.NoError(t, err)
    assert.Equal(t, TTCFunctionCode(0xFE), fc)
    assert.False(t, fc.IsKnown())
}

func TestParseTTCFunctionCode_RealCaptures(t *testing.T) {
    captures := []struct {
        file string
        want TTCFunctionCode
    }{
        {"testdata/captures/select_dual_request.bin", TTCFuncOALL8},
        {"testdata/captures/select_dual_response.bin", TTCFuncResponse},
        {"testdata/captures/fetch_request.bin", TTCFuncOFETCH},
        {"testdata/captures/close_cursor.bin", TTCFuncOCLOSE},
    }
    for _, c := range captures {
        t.Run(c.file, func(t *testing.T) {
            raw := loadCapture(t, c.file)
            fc, err := parseTTCFunctionCode(extractTNSDataPayload(raw))
            require.NoError(t, err)
            assert.Equal(t, c.want, fc)
        })
    }
}

func TestTTCFunctionCode_Stringer(t *testing.T) {
    assert.Equal(t, "OALL8", TTCFuncOALL8.String())
    assert.Equal(t, "OFETCH", TTCFuncOFETCH.String())
    assert.Equal(t, "UNKNOWN(0xfe)", TTCFunctionCode(0xFE).String())
}

func TestSessionLogsFunctionCodes(t *testing.T) {
    var logBuf bytes.Buffer
    logger := slog.New(slog.NewJSONHandler(&logBuf, nil))

    upstream := newEchoTNSServer(t)
    client, proxyEnd := net.Pipe()

    session := &OracleSession{clientConn: proxyEnd, logger: logger}
    session.upstreamConn, _ = net.Dial("tcp", upstream.Addr().String())

    go session.proxyMessages()

    client.Write(encodeTNSPacket(TNSPacketTypeData, buildTTCDataPayload(0x0E, []byte("SELECT 1"))))
    time.Sleep(50 * time.Millisecond)

    assert.Contains(t, logBuf.String(), "OALL8")
    client.Close()
}
```

---

### Step 6: OALL8 SQL Extraction

Decode the OALL8 message to extract SQL text, bind values, and cursor ID. This is the hardest decoding step — the binary format is undocumented, reference is go-ora's `command.go`.

**Files:** `oracle/ttc_decode.go`, `oracle/ttc_decode_test.go`

**Key types:**

```go
type OALL8Result struct {
    SQL        string
    CursorID   uint16
    BindValues []string
}

func (r *OALL8Result) IsPLSQL() bool {
    normalized := strings.ToUpper(strings.TrimSpace(r.SQL))
    return strings.HasPrefix(normalized, "BEGIN") || strings.HasPrefix(normalized, "DECLARE")
}

func decodeOALL8(ttcPayload []byte) (*OALL8Result, error)

type OFETCHResult struct {
    CursorID  uint16
    FetchSize uint32
}

func decodeOFETCH(ttcPayload []byte) (*OFETCHResult, error)
```

**Tests:**

```go
func TestDecodeOALL8_SimpleSELECT(t *testing.T) {
    payload := buildOALL8("SELECT * FROM employees WHERE id = :1", []string{"42"}, 7)
    result, err := decodeOALL8(payload)
    require.NoError(t, err)
    assert.Equal(t, "SELECT * FROM employees WHERE id = :1", result.SQL)
    assert.Equal(t, uint16(7), result.CursorID)
    assert.Equal(t, []string{"42"}, result.BindValues)
}

func TestDecodeOALL8_NoBinds(t *testing.T) {
    payload := buildOALL8("SELECT SYSDATE FROM DUAL", nil, 1)
    result, err := decodeOALL8(payload)
    require.NoError(t, err)
    assert.Equal(t, "SELECT SYSDATE FROM DUAL", result.SQL)
    assert.Empty(t, result.BindValues)
}

func TestDecodeOALL8_MultipleBinds(t *testing.T) {
    sql := "INSERT INTO t (a, b, c) VALUES (:1, :2, :3)"
    binds := []string{"hello", "42", "2024-01-15"}
    payload := buildOALL8(sql, binds, 3)
    result, err := decodeOALL8(payload)
    require.NoError(t, err)
    assert.Equal(t, sql, result.SQL)
    assert.Equal(t, binds, result.BindValues)
}

func TestDecodeOALL8_PLSQLBlock(t *testing.T) {
    sql := "BEGIN my_package.do_something(:1, :2); END;"
    payload := buildOALL8(sql, []string{"arg1", "arg2"}, 5)
    result, err := decodeOALL8(payload)
    require.NoError(t, err)
    assert.True(t, result.IsPLSQL())
}

func TestDecodeOALL8_LargeSQL(t *testing.T) {
    sql := "SELECT " + strings.Repeat("col, ", 1000) + "col FROM t"
    payload := buildOALL8(sql, nil, 10)
    result, err := decodeOALL8(payload)
    require.NoError(t, err)
    assert.Equal(t, sql, result.SQL)
}

func TestDecodeOALL8_EmptySQL(t *testing.T) {
    _, err := decodeOALL8(buildOALL8("", nil, 1))
    assert.ErrorIs(t, err, ErrEmptySQL)
}

func TestDecodeOALL8_UnicodeSQL(t *testing.T) {
    sql := "SELECT * FROM données WHERE nom = :1"
    payload := buildOALL8(sql, []string{"Éric"}, 2)
    result, err := decodeOALL8(payload)
    require.NoError(t, err)
    assert.Equal(t, sql, result.SQL)
}

func TestDecodeOALL8_NullBindValue(t *testing.T) {
    payload := buildOALL8WithNulls("UPDATE t SET col = :1 WHERE id = :2", []interface{}{nil, 42}, 3)
    result, err := decodeOALL8(payload)
    require.NoError(t, err)
    assert.Equal(t, "NULL", result.BindValues[0])
    assert.Equal(t, "42", result.BindValues[1])
}

func TestDecodeOALL8_BinaryBindValue(t *testing.T) {
    payload := buildOALL8WithBinaryBind("INSERT INTO t (raw_col) VALUES (:1)", []byte{0xDE, 0xAD, 0xBE, 0xEF}, 4)
    result, err := decodeOALL8(payload)
    require.NoError(t, err)
    assert.Equal(t, "DEADBEEF", result.BindValues[0])
}

func TestDecodeOALL8_RealCaptures(t *testing.T) {
    for _, tt := range []struct {
        file      string
        expectSQL string
    }{
        {"testdata/captures/select_dual_oall8.bin", "DUAL"},
        {"testdata/captures/insert_oall8.bin", "INSERT"},
        {"testdata/captures/plsql_oall8.bin", "BEGIN"},
    } {
        t.Run(tt.file, func(t *testing.T) {
            raw := loadCapture(t, tt.file)
            result, err := decodeOALL8(extractTTCPayload(raw))
            require.NoError(t, err)
            assert.Contains(t, strings.ToUpper(result.SQL), tt.expectSQL)
        })
    }
}

func TestDecodeOALL8_CorruptPayload(t *testing.T) {
    _, err := decodeOALL8([]byte{0x0E, 0x00, 0x01})
    assert.Error(t, err)
}

func TestDecodeOALL8_FuzzInputs(t *testing.T) {
    f := fuzz.New()
    for i := 0; i < 1000; i++ {
        var data []byte
        f.Fuzz(&data)
        decodeOALL8(data) // must not panic
    }
}

func TestDecodeOFETCH(t *testing.T) {
    payload := buildOFETCH(7, 100)
    result, err := decodeOFETCH(payload)
    require.NoError(t, err)
    assert.Equal(t, uint16(7), result.CursorID)
    assert.Equal(t, uint32(100), result.FetchSize)
}
```

---

### Step 7: Query Logging

Wire up OALL8/OFETCH interception to `store.CreateQuery`. Log SQL text, bind values, timing, connection ID. Track connection stats.

**Files:** `oracle/intercept.go`, `oracle/intercept_test.go`

**Tests:**

```go
func TestQueryLogging_SimpleSELECT(t *testing.T) {
    ctx := context.Background()
    testStore := setupTestStore(t)
    session := setupTestSession(t, testStore, newFakeOracle(t))
    session.connectionUID = uuid.Must(uuid.NewV7())

    session.handleOALL8(buildOALL8("SELECT 1 FROM DUAL", nil, 1))
    session.completeQuery(nil, nil, 100)

    queries, err := testStore.ListQueries(ctx, &store.QueryFilter{ConnectionID: &session.connectionUID})
    require.NoError(t, err)
    require.Len(t, queries, 1)
    assert.Equal(t, "SELECT 1 FROM DUAL", queries[0].SQLText)
    assert.NotNil(t, queries[0].DurationMs)
}

func TestQueryLogging_WithBindParameters(t *testing.T) {
    testStore := setupTestStore(t)
    session := setupTestSession(t, testStore, newFakeOracle(t))
    session.connectionUID = uuid.Must(uuid.NewV7())

    session.handleOALL8(buildOALL8("SELECT * FROM emp WHERE dept_id = :1 AND status = :2", []string{"10", "ACTIVE"}, 2))
    session.completeQuery(nil, nil, 0)

    queries, _ := testStore.ListQueries(context.Background(), &store.QueryFilter{ConnectionID: &session.connectionUID})
    require.Len(t, queries, 1)
    assert.NotNil(t, queries[0].Parameters)
    assert.Equal(t, []string{"10", "ACTIVE"}, queries[0].Parameters.Values)
}

func TestQueryLogging_Error(t *testing.T) {
    testStore := setupTestStore(t)
    session := setupTestSession(t, testStore, newFakeOracle(t))
    session.connectionUID = uuid.Must(uuid.NewV7())

    session.handleOALL8(buildOALL8("SELECT * FROM nonexistent", nil, 1))
    errMsg := "ORA-00942: table or view does not exist"
    session.completeQuery(nil, &errMsg, 0)

    queries, _ := testStore.ListQueries(context.Background(), &store.QueryFilter{ConnectionID: &session.connectionUID})
    require.Len(t, queries, 1)
    assert.Contains(t, *queries[0].Error, "ORA-00942")
}

func TestQueryLogging_DurationTracked(t *testing.T) {
    testStore := setupTestStore(t)
    session := setupTestSession(t, testStore, newFakeOracle(t))
    session.connectionUID = uuid.Must(uuid.NewV7())

    session.handleOALL8(buildOALL8("SELECT 1 FROM DUAL", nil, 1))
    time.Sleep(100 * time.Millisecond)
    session.completeQuery(nil, nil, 0)

    queries, _ := testStore.ListQueries(context.Background(), &store.QueryFilter{ConnectionID: &session.connectionUID})
    assert.True(t, *queries[0].DurationMs >= 90)
}

func TestQueryLogging_MultipleQueries(t *testing.T) {
    testStore := setupTestStore(t)
    session := setupTestSession(t, testStore, newFakeOracle(t))
    session.connectionUID = uuid.Must(uuid.NewV7())

    for i := 0; i < 5; i++ {
        session.handleOALL8(buildOALL8(fmt.Sprintf("SELECT %d FROM DUAL", i), nil, uint16(i+1)))
        session.completeQuery(nil, nil, 0)
    }

    queries, _ := testStore.ListQueries(context.Background(), &store.QueryFilter{ConnectionID: &session.connectionUID})
    assert.Len(t, queries, 5)
}

func TestQueryLogging_ConnectionStatsUpdated(t *testing.T) {
    testStore := setupTestStore(t)
    session := setupTestSession(t, testStore, newFakeOracle(t))
    conn, _ := testStore.CreateConnection(context.Background(), session.user.UID, session.database.UID, "127.0.0.1")
    session.connectionUID = conn.UID

    session.handleOALL8(buildOALL8("SELECT * FROM big_table", nil, 1))
    session.completeQuery(ptr(int64(500)), nil, 1048576)

    updatedConn, _ := testStore.GetConnection(context.Background(), conn.UID)
    assert.Equal(t, int64(1), updatedConn.Queries)
    assert.True(t, updatedConn.BytesTransferred > 0)
}

func TestQueryLogging_CursorReuse(t *testing.T) {
    testStore := setupTestStore(t)
    session := setupTestSession(t, testStore, newFakeOracle(t))
    session.connectionUID = uuid.Must(uuid.NewV7())

    session.handleOALL8(buildOALL8("SELECT 1 FROM DUAL", nil, 5))
    session.completeQuery(nil, nil, 0)
    session.handleClose(5)

    session.handleOALL8(buildOALL8("SELECT 2 FROM DUAL", nil, 5))
    session.completeQuery(nil, nil, 0)

    queries, _ := testStore.ListQueries(context.Background(), &store.QueryFilter{ConnectionID: &session.connectionUID})
    assert.Len(t, queries, 2)
    assert.Equal(t, "SELECT 1 FROM DUAL", queries[0].SQLText)
    assert.Equal(t, "SELECT 2 FROM DUAL", queries[1].SQLText)
}
```

---

### Step 8: Access Control Enforcement

Block writes, DDL, and Oracle-specific dangerous patterns based on grant controls. Factor shared validation out of the PG proxy.

**Files:** `shared/validation.go`, `shared/validation_test.go`, `oracle/intercept.go` (enforcement), `oracle/intercept_test.go`

**Refactoring note:** Extract `isWriteQuery()`, `isDDLQuery()`, `containsPasswordChange()` from `internal/proxy/intercept.go` into `internal/proxy/shared/validation.go`. Update the PG proxy to call the shared functions. This is a refactor with no behavior change for PG.

**Tests:**

```go
// shared/validation_test.go

func TestValidateQuery_ReadOnly_BlocksWrites(t *testing.T) {
    grant := &store.Grant{Controls: []string{store.ControlReadOnly}}
    blocked := []string{
        "INSERT INTO t VALUES (1)", "UPDATE t SET x = 1", "DELETE FROM t WHERE id = 1",
        "MERGE INTO t USING s ON (t.id = s.id) WHEN MATCHED THEN UPDATE SET t.x = s.x",
        "DROP TABLE t", "TRUNCATE TABLE t", "CREATE TABLE t (id NUMBER)",
        "ALTER TABLE t ADD (col VARCHAR2(100))", "GRANT SELECT ON t TO u", "REVOKE SELECT ON t FROM u",
    }
    for _, sql := range blocked {
        assert.ErrorIs(t, ValidateQuery(sql, grant), ErrReadOnlyViolation, "should block: %s", sql)
    }
}

func TestValidateQuery_ReadOnly_AllowsReads(t *testing.T) {
    grant := &store.Grant{Controls: []string{store.ControlReadOnly}}
    allowed := []string{
        "SELECT * FROM t", "SELECT 1 FROM DUAL",
        "WITH cte AS (SELECT 1 FROM DUAL) SELECT * FROM cte",
        "EXPLAIN PLAN FOR SELECT * FROM t", "  select * from t  ",
    }
    for _, sql := range allowed {
        assert.NoError(t, ValidateQuery(sql, grant), "should allow: %s", sql)
    }
}

func TestValidateQuery_BlockDDL(t *testing.T) {
    grant := &store.Grant{Controls: []string{store.ControlBlockDDL}}
    blocked := []string{
        "CREATE TABLE t (id NUMBER)", "ALTER TABLE t ADD (col NUMBER)", "DROP TABLE t",
        "CREATE INDEX idx ON t(col)", "CREATE OR REPLACE VIEW v AS SELECT 1 FROM DUAL",
        "CREATE SEQUENCE s", "ALTER INDEX idx REBUILD",
    }
    allowed := []string{"INSERT INTO t VALUES (1)", "SELECT * FROM t", "UPDATE t SET x = 1"}

    for _, sql := range blocked {
        assert.ErrorIs(t, ValidateQuery(sql, grant), ErrDDLBlocked, "should block: %s", sql)
    }
    for _, sql := range allowed {
        assert.NoError(t, ValidateQuery(sql, grant), "should allow: %s", sql)
    }
}

func TestValidateQuery_CaseInsensitive(t *testing.T) {
    grant := &store.Grant{Controls: []string{store.ControlReadOnly}}
    assert.Error(t, ValidateQuery("insert INTO t VALUES (1)", grant))
    assert.Error(t, ValidateQuery("  INSERT INTO t VALUES (1)  ", grant))
}

func TestValidateQuery_CommentBypass(t *testing.T) {
    grant := &store.Grant{Controls: []string{store.ControlReadOnly}}
    assert.Error(t, ValidateQuery("/* harmless */ INSERT INTO t VALUES (1)", grant))
    assert.Error(t, ValidateQuery("-- just a select\nINSERT INTO t VALUES (1)", grant))
}

// oracle/intercept_test.go

func TestValidateOracleQuery_BlocksDangerousPatterns(t *testing.T) {
    grant := &store.Grant{Controls: []string{store.ControlReadOnly}}
    blocked := []struct{ sql, reason string }{
        {"ALTER SYSTEM SET open_cursors=1000", "system config"},
        {"ALTER SYSTEM KILL SESSION '123,456'", "kill session"},
        {"CREATE DATABASE LINK remote CONNECT TO u IDENTIFIED BY p USING 'tns'", "network escape"},
        {"BEGIN DBMS_SCHEDULER.CREATE_JOB(...); END;", "async execution"},
        {"SELECT UTL_HTTP.REQUEST('http://evil.com') FROM DUAL", "network access"},
        {"SELECT UTL_FILE.FOPEN('/etc/passwd','r') FROM DUAL", "file system access"},
        {"DECLARE PRAGMA AUTONOMOUS_TRANSACTION; BEGIN DELETE FROM t; COMMIT; END;", "autonomous txn"},
        {"BEGIN DBMS_PIPE.SEND_MESSAGE('pipe'); END;", "IPC escape"},
        {"BEGIN UTL_TCP.OPEN_CONNECTION('evil.com', 80); END;", "network escape"},
        {"BEGIN DBMS_JOB.SUBMIT(...); END;", "async execution"},
    }
    for _, tt := range blocked {
        t.Run(tt.reason, func(t *testing.T) {
            assert.Error(t, ValidateOracleQuery(tt.sql, grant))
        })
    }
}

func TestValidateOracleQuery_AllowsSafePLSQL(t *testing.T) {
    grant := &store.Grant{} // No restrictions
    allowed := []string{
        "BEGIN my_pkg.read_data(:1, :2); END;",
        "DECLARE v NUMBER; BEGIN SELECT COUNT(*) INTO v FROM t; END;",
        "BEGIN NULL; END;",
    }
    for _, sql := range allowed {
        assert.NoError(t, ValidateOracleQuery(sql, grant))
    }
}

func TestInterceptOALL8_BlockedQuery_SendsError(t *testing.T) {
    client, proxyEnd := net.Pipe()
    defer client.Close()

    session := &OracleSession{
        clientConn: proxyEnd,
        grant:      &store.Grant{Controls: []string{store.ControlReadOnly}},
    }

    go func() {
        client.Write(encodeTNSPacket(TNSPacketTypeData,
            buildTTCDataPayload(0x0E, buildOALL8("DELETE FROM t", nil, 1))))
    }()

    err := session.handleClientMessage()
    assert.ErrorIs(t, err, ErrReadOnlyViolation)

    pkt, _ := readTNSPacket(client)
    assert.Equal(t, TNSPacketTypeData, pkt.Type)
    fc, _ := parseTTCFunctionCode(pkt.Payload)
    assert.Equal(t, TTCFuncResponse, fc)
    assert.Contains(t, string(pkt.Payload), "1031")
}

func TestInterceptOALL8_AllowedQuery_Forwarded(t *testing.T) {
    upstream := newEchoTNSServer(t)
    client, proxyEnd := net.Pipe()
    defer client.Close()

    session := &OracleSession{
        clientConn: proxyEnd,
        grant:      &store.Grant{Controls: []string{store.ControlReadOnly}},
    }
    session.upstreamConn, _ = net.Dial("tcp", upstream.Addr().String())

    go func() {
        client.Write(encodeTNSPacket(TNSPacketTypeData,
            buildTTCDataPayload(0x0E, buildOALL8("SELECT 1 FROM DUAL", nil, 1))))
    }()

    err := session.handleClientMessage()
    assert.NoError(t, err)
}
```

---

## Files Summary

| File | Type | Description |
|------|------|-------------|
| `internal/proxy/oracle/ttc.go` | New | TTC function code parser |
| `internal/proxy/oracle/ttc_test.go` | New | TTC parser tests |
| `internal/proxy/oracle/ttc_decode.go` | New | OALL8 + OFETCH decoders |
| `internal/proxy/oracle/ttc_decode_test.go` | New | Decoder tests |
| `internal/proxy/oracle/intercept.go` | New | Query interception, logging, access control |
| `internal/proxy/oracle/intercept_test.go` | New | Interception tests |
| `internal/proxy/oracle/session.go` | Modified | Replace raw relay with TTC-aware relay |
| `internal/proxy/shared/validation.go` | New | Shared SQL validation (extracted from PG) |
| `internal/proxy/shared/validation_test.go` | New | Shared validation tests |
| `internal/proxy/intercept.go` | Modified | Delegate to shared/validation.go |

## Acceptance Criteria

1. Every SQL query through the Oracle proxy is logged in the `queries` table with SQL text, bind parameters, duration, and connection ID
2. `read_only` grants block INSERT/UPDATE/DELETE/DDL through the Oracle proxy with ORA-01031 error
3. `block_ddl` grants block CREATE/ALTER/DROP through the Oracle proxy
4. Oracle-specific dangerous patterns (ALTER SYSTEM, UTL_HTTP, etc.) are always blocked on read-only grants
5. Blocked queries return a valid TTC error response that sqlplus/JDBC can parse
6. Cursor state is tracked: OFETCH operations are linked to the correct SQL via cursor ID
7. Cursor close (OCLOSE) cleans up tracking state
8. Connection stats (query count, bytes transferred) are updated
9. PG proxy still works — shared validation is a refactor with no behavior change
10. Non-OALL8 TTC messages (OOPEN, OCANCEL, OLOBOPS, etc.) pass through transparently

## Estimated Size

~800 lines new Go code + ~500 lines ported from go-ora + ~200 lines refactored = **~1,500 lines total** (+ ~600 lines tests)
