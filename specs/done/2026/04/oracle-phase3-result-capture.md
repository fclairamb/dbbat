# Oracle Proxy — Phase 3: Result Capture & Storage

> Parent spec: `specs/2026-04-02-oracle-proxy.md`

## Goal

Capture query results from Oracle TTC responses: decode column definitions and row data, convert Oracle data types (NUMBER, DATE, TIMESTAMP, VARCHAR2, etc.) to JSON, and store them using the existing `Query`/`QueryRow` model. Run a full end-to-end integration test suite against Oracle XE 18c.

## Prerequisites

- **Phase 1 complete**: Connection & auth passthrough working
- **Phase 2 complete**: TTC function code parsing, OALL8 decoding, cursor tracking, query logging, access control all working

## Outcome

- TTC response parser (column definitions, row data, return codes, error messages, more-data flag)
- Oracle data type decoders (NUMBER, DATE, TIMESTAMP, VARCHAR2, CHAR, RAW, BINARY_FLOAT/DOUBLE, LOB placeholders)
- Row capture with limits (same MaxResultRows/MaxResultBytes as PG proxy)
- Query logging with result rows via `store.StoreQueryRows`
- LOB placeholder handling
- PL/SQL block logging
- Full integration test suite against Oracle XE 18c via testcontainers

## Non-Goals

- LOB content capture (store `[LOB]` placeholder)
- Tracking SQL inside PL/SQL blocks (Oracle audit handles that)
- Oracle Native Network Encryption termination
- SASL/Kerberos auth

---

## Oracle Response Structure

TTC response packets (function code 0x08) carry results back from Oracle. The response to OALL8 or OFETCH contains:

```
[TTC header: function code 0x08]
[return code: uint16]           ← 0 = success
[row count: uint32]
[column definitions (first response only):]
  [column count: uint16]
  [for each column:]
    [name length + name]
    [data type: uint8]          ← Oracle type code
    [max size: uint32]
    [precision: uint8]
    [scale: uint8]
    [nullable: uint8]
[row data:]
  [for each row:]
    [for each column:]
      [value length prefix]
      [value bytes]             ← Encoded per type
[more-data flag: bool]          ← If true, client must OFETCH for more
```

## Oracle Data Type Decoding

| Type Code | Type Name | Go Decode Strategy |
|-----------|-----------|--------------------|
| 1 | VARCHAR2 | Direct UTF-8 string |
| 2 | NUMBER | Oracle NUMBER format → `big.Float` → string |
| 12 | DATE | 7-byte Oracle date → `time.Time` |
| 23 | RAW | Hex-encode bytes |
| 96 | CHAR | UTF-8 string, trim trailing spaces |
| 100 | BINARY_FLOAT | IEEE 754 float32 |
| 101 | BINARY_DOUBLE | IEEE 754 float64 |
| 112 | CLOB | LOB locator → `"[LOB]"` placeholder |
| 113 | BLOB | LOB locator → `"[LOB]"` placeholder |
| 180 | TIMESTAMP | 7 DATE bytes + 4 bytes fractional seconds |
| 181 | TIMESTAMP WITH TIME ZONE | Timestamp + TZ offset |
| 231 | TIMESTAMP WITH LOCAL TIME ZONE | Timestamp in session TZ |

### Oracle NUMBER Format

Variable-length format with sign byte, exponent byte, and mantissa digits in base-100. Port from go-ora's `converters.go`:

```go
func decodeOracleNumber(data []byte) (string, error)
```

### Oracle DATE Format

7-byte fixed format:
```
byte 0: century (e.g., 120 = 20th century)
byte 1: year within century (e.g., 124 = year 24 → 2024)
byte 2: month (1-12)
byte 3: day (1-31)
byte 4: hour + 1 (1-24, so 14:00 = 15)
byte 5: minute + 1 (1-60, so 30 = 31)
byte 6: second + 1 (1-60, so 0 = 1)
```

```go
func decodeOracleDate(data []byte) (time.Time, error)
```

### Type Decoder Dispatch

```go
func decodeOracleValue(typeCode uint8, data []byte) (interface{}, error) {
    switch typeCode {
    case 1, 96: // VARCHAR2, CHAR
        return string(data), nil // CHAR: trim trailing spaces
    case 2:
        return decodeOracleNumber(data)
    case 12:
        return decodeOracleDate(data)
    case 23:
        return hex.EncodeToString(data), nil
    case 100:
        return math.Float32frombits(binary.BigEndian.Uint32(data)), nil
    case 101:
        return math.Float64frombits(binary.BigEndian.Uint64(data)), nil
    case 180, 181, 231:
        return decodeOracleTimestamp(data)
    case 112, 113:
        return "[LOB]", nil
    default:
        return base64.StdEncoding.EncodeToString(data), nil
    }
}
```

## Row Capture Flow

```
   OALL8 (SQL + binds)                OFETCH (cursor ID)
        │                                    │
        ▼                                    ▼
   Track cursor                        Look up cursor
   Start timing                        Continue timing
        │                                    │
        ▼                                    ▼
   ┌─────────── Response from Oracle ─────────────┐
   │                                               │
   │  First response → column definitions          │
   │  ├─ Extract column names + types              │
   │  ├─ Store in trackedCursor                    │
   │  └─ Capture rows (if any inline)              │
   │                                               │
   │  Row data → decode per column type            │
   │  ├─ Build JSON row: {"COL1": val, "COL2": val}│
   │  ├─ Check limits (MaxResultRows, MaxBytes)    │
   │  ├─ If exceeded → discard all, set truncated  │
   │  └─ Append to capturedRows                    │
   │                                               │
   │  More-data flag = false → query complete      │
   │  ├─ Calculate duration                        │
   │  ├─ logQuery() → store.CreateQuery()          │
   │  └─ store.StoreQueryRows() (if not truncated) │
   └───────────────────────────────────────────────┘
```

## Storing Results

Uses existing `Query` and `QueryRow` models unchanged:

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
            return
        }
        if len(pending.capturedRows) > 0 {
            s.store.StoreQueryRows(s.ctx, query.UID, pending.capturedRows)
        }
    }()
}
```

## LOB Handling

LOBs use a locator pattern — row data contains a LOB locator, client issues `OLOBOPS` (0x44) for content. For the MVP:
- Store `"[CLOB]"` or `"[BLOB]"` as the value
- Track LOB read operations as separate log entries
- Future: capture small LOBs (< configurable threshold) inline

## PL/SQL Blocks

`BEGIN...END;` blocks are sent as SQL text in OALL8:
- Log the PL/SQL block text as-is
- Capture OUT parameter values as a single "row" if available
- Don't track internal SQL within PL/SQL

---

## Implementation Steps & Tests

### Step 9: Response Parsing + Row Capture

Decode TTC responses to capture column definitions, row data, and Oracle data types. Store captured rows via existing QueryRow model.

**Files:** `oracle/ttc_decode.go` (response parsing additions), `oracle/types.go`, `oracle/types_test.go`, `oracle/ttc_decode_test.go` (response tests)

#### Oracle Type Decoder Tests

```go
// types_test.go

func TestDecodeOracleNumber(t *testing.T) {
    tests := []struct {
        name     string
        raw      []byte
        expected string
    }{
        {"zero", []byte{0x80}, "0"},
        {"one", []byte{0xC1, 0x02}, "1"},
        {"negative_one", []byte{0x3E, 0x64, 0x66}, "-1"},
        {"large", []byte{0xC3, 0x0D, 0x2A, 0x04}, "123456"},
        {"decimal", []byte{0xC1, 0x04, 0x1F}, "3.14"},
        {"max_precision", generateOracleNumber(t, "99999999999999999999999999999999999999"), "99999999999999999999999999999999999999"},
        {"very_small", generateOracleNumber(t, "0.000001"), "0.000001"},
        {"negative_decimal", generateOracleNumber(t, "-42.5"), "-42.5"},
    }
    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            result, err := decodeOracleNumber(tt.raw)
            require.NoError(t, err)
            assert.Equal(t, tt.expected, result)
        })
    }
}

func TestDecodeOracleNumber_EmptyBytes(t *testing.T) {
    _, err := decodeOracleNumber([]byte{})
    assert.Error(t, err)
}

func TestDecodeOracleDate(t *testing.T) {
    tests := []struct {
        name     string
        raw      []byte
        expected time.Time
    }{
        {"2024-03-15 14:30:00", []byte{120, 124, 3, 15, 15, 31, 1}, time.Date(2024, 3, 15, 14, 30, 0, 0, time.UTC)},
        {"2000-01-01 00:00:00", []byte{120, 100, 1, 1, 1, 1, 1}, time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)},
        {"1999-12-31 23:59:59", []byte{119, 199, 12, 31, 24, 60, 60}, time.Date(1999, 12, 31, 23, 59, 59, 0, time.UTC)},
    }
    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            result, err := decodeOracleDate(tt.raw)
            require.NoError(t, err)
            assert.Equal(t, tt.expected, result)
        })
    }
}

func TestDecodeOracleDate_WrongLength(t *testing.T) {
    _, err := decodeOracleDate([]byte{1, 2, 3})
    assert.ErrorIs(t, err, ErrInvalidDateLength)
}

func TestDecodeOracleTimestamp(t *testing.T) {
    raw := append(
        []byte{120, 124, 6, 15, 11, 31, 1},
        []byte{0x05, 0xF5, 0xE1, 0x00}...,
    )
    result, err := decodeOracleTimestamp(raw)
    require.NoError(t, err)
    assert.Equal(t, 2024, result.Year())
    assert.Equal(t, time.June, result.Month())
    assert.Equal(t, 100000000, result.Nanosecond())
}

func TestDecodeOracleVARCHAR2(t *testing.T) {
    result, _ := decodeOracleValue(1, []byte("hello world"))
    assert.Equal(t, "hello world", result)
}

func TestDecodeOracleVARCHAR2_UTF8(t *testing.T) {
    result, _ := decodeOracleValue(1, []byte("héllo wörld 日本語"))
    assert.Equal(t, "héllo wörld 日本語", result)
}

func TestDecodeOracleCHAR_TrimsPadding(t *testing.T) {
    result, _ := decodeOracleValue(96, []byte("hello     "))
    assert.Equal(t, "hello", result)
}

func TestDecodeOracleRAW(t *testing.T) {
    result, _ := decodeOracleValue(23, []byte{0xDE, 0xAD, 0xBE, 0xEF})
    assert.Equal(t, "deadbeef", result)
}

func TestDecodeOracleBINARY_FLOAT(t *testing.T) {
    buf := make([]byte, 4)
    binary.BigEndian.PutUint32(buf, math.Float32bits(3.14))
    result, _ := decodeOracleValue(100, buf)
    assert.InDelta(t, 3.14, result, 0.01)
}

func TestDecodeOracleBINARY_DOUBLE(t *testing.T) {
    buf := make([]byte, 8)
    binary.BigEndian.PutUint64(buf, math.Float64bits(2.718281828))
    result, _ := decodeOracleValue(101, buf)
    assert.InDelta(t, 2.718281828, result, 0.0001)
}

func TestDecodeOracleLOB_ReturnsPlaceholder(t *testing.T) {
    result, _ := decodeOracleValue(112, []byte{0x01, 0x02, 0x03})
    assert.Equal(t, "[LOB]", result)
    result, _ = decodeOracleValue(113, []byte{0x01, 0x02, 0x03})
    assert.Equal(t, "[LOB]", result)
}

func TestDecodeOracleValue_UnknownType_Base64(t *testing.T) {
    result, _ := decodeOracleValue(255, []byte{0x01, 0x02})
    assert.Equal(t, base64.StdEncoding.EncodeToString([]byte{0x01, 0x02}), result)
}

func TestDecodeOracleValue_NilData(t *testing.T) {
    result, _ := decodeOracleValue(1, nil)
    assert.Nil(t, result)
}
```

#### TTC Response Parsing Tests

```go
// ttc_decode_test.go (response parsing additions)

func TestDecodeTTCResponse_ColumnDefinitions(t *testing.T) {
    resp := buildTTCResponse(
        []columnDef{
            {Name: "ID", TypeCode: 2, Size: 22},
            {Name: "NAME", TypeCode: 1, Size: 100},
            {Name: "CREATED", TypeCode: 12, Size: 7},
        },
        nil,
    )
    result, err := decodeTTCResponse(resp)
    require.NoError(t, err)
    assert.Len(t, result.Columns, 3)
    assert.Equal(t, "ID", result.Columns[0].Name)
    assert.Equal(t, OracleTypeNUMBER, result.Columns[0].TypeCode)
}

func TestDecodeTTCResponse_WithRows(t *testing.T) {
    resp := buildTTCResponse(
        []columnDef{{Name: "ID", TypeCode: 2}, {Name: "NAME", TypeCode: 1}},
        [][]interface{}{{1, "Alice"}, {2, "Bob"}},
    )
    result, err := decodeTTCResponse(resp)
    require.NoError(t, err)
    assert.Len(t, result.Rows, 2)
}

func TestDecodeTTCResponse_ErrorResponse(t *testing.T) {
    resp := buildTTCErrorResponse(942, "ORA-00942: table or view does not exist")
    result, err := decodeTTCResponse(resp)
    require.NoError(t, err)
    assert.True(t, result.IsError)
    assert.Equal(t, 942, result.ErrorCode)
    assert.Contains(t, result.ErrorMessage, "ORA-00942")
}

func TestDecodeTTCResponse_MoreDataFlag(t *testing.T) {
    resp := buildTTCResponseWithMoreData(true)
    result, _ := decodeTTCResponse(resp)
    assert.True(t, result.MoreData)
}

func TestDecodeTTCResponse_RealCapture(t *testing.T) {
    raw := loadCapture(t, "testdata/captures/select_emp_response.bin")
    result, err := decodeTTCResponse(extractTTCPayload(raw))
    require.NoError(t, err)
    assert.True(t, len(result.Columns) > 0)
    assert.True(t, len(result.Rows) > 0)
}
```

#### Row Capture Tests

```go
func TestRowCapture_Limits(t *testing.T) {
    testStore := setupTestStore(t)
    session := setupTestSession(t, testStore, newFakeOracle(t))
    session.connectionUID = uuid.Must(uuid.NewV7())
    session.queryStorage = config.QueryStorageConfig{StoreResults: true, MaxResultRows: 3, MaxResultBytes: 1024}

    session.handleOALL8(buildOALL8("SELECT * FROM big_table", nil, 1))
    for i := 0; i < 10; i++ {
        session.captureRow(
            []columnDef{{Name: "id", TypeCode: 2}, {Name: "data", TypeCode: 1}},
            []interface{}{i, fmt.Sprintf("row-%d", i)},
        )
    }
    session.completeQuery(ptr(int64(10)), nil, 5000)

    queries, _ := testStore.ListQueriesWithRows(context.Background(), &store.QueryFilter{ConnectionID: &session.connectionUID})
    require.Len(t, queries, 1)
    assert.Len(t, queries[0].Rows, 0) // Truncated → all discarded
}

func TestRowCapture_WithinLimits(t *testing.T) {
    testStore := setupTestStore(t)
    session := setupTestSession(t, testStore, newFakeOracle(t))
    session.connectionUID = uuid.Must(uuid.NewV7())
    session.queryStorage = config.QueryStorageConfig{StoreResults: true, MaxResultRows: 100, MaxResultBytes: 1048576}

    session.handleOALL8(buildOALL8("SELECT id, name FROM emp", nil, 1))
    session.captureRow([]columnDef{{Name: "ID", TypeCode: 2}, {Name: "NAME", TypeCode: 1}}, []interface{}{1, "Alice"})
    session.captureRow([]columnDef{{Name: "ID", TypeCode: 2}, {Name: "NAME", TypeCode: 1}}, []interface{}{2, "Bob"})
    session.completeQuery(ptr(int64(2)), nil, 200)

    queries, _ := testStore.ListQueriesWithRows(context.Background(), &store.QueryFilter{ConnectionID: &session.connectionUID})
    require.Len(t, queries[0].Rows, 2)

    var row0 map[string]interface{}
    json.Unmarshal(queries[0].Rows[0].RowData, &row0)
    assert.Equal(t, "1", row0["ID"])
    assert.Equal(t, "Alice", row0["NAME"])
}

func TestRowCapture_MultiFetch(t *testing.T) {
    testStore := setupTestStore(t)
    session := setupTestSession(t, testStore, newFakeOracle(t))
    session.connectionUID = uuid.Must(uuid.NewV7())
    session.queryStorage = config.QueryStorageConfig{StoreResults: true, MaxResultRows: 100, MaxResultBytes: 1048576}

    session.handleOALL8(buildOALL8("SELECT id FROM t", nil, 5))
    session.captureRow([]columnDef{{Name: "ID", TypeCode: 2}}, []interface{}{1})
    session.captureRow([]columnDef{{Name: "ID", TypeCode: 2}}, []interface{}{2})

    session.handleOFETCH(5)
    session.captureRow([]columnDef{{Name: "ID", TypeCode: 2}}, []interface{}{3})
    session.completeQuery(ptr(int64(3)), nil, 300)

    queries, _ := testStore.ListQueriesWithRows(context.Background(), &store.QueryFilter{ConnectionID: &session.connectionUID})
    assert.Len(t, queries[0].Rows, 3)
    assert.Equal(t, "SELECT id FROM t", queries[0].SQLText)
}

func TestRowCapture_StoreResultsDisabled(t *testing.T) {
    testStore := setupTestStore(t)
    session := setupTestSession(t, testStore, newFakeOracle(t))
    session.connectionUID = uuid.Must(uuid.NewV7())
    session.queryStorage = config.QueryStorageConfig{StoreResults: false}

    session.handleOALL8(buildOALL8("SELECT * FROM t", nil, 1))
    session.captureRow([]columnDef{{Name: "ID", TypeCode: 2}}, []interface{}{1})
    session.completeQuery(ptr(int64(1)), nil, 100)

    queries, _ := testStore.ListQueriesWithRows(context.Background(), &store.QueryFilter{ConnectionID: &session.connectionUID})
    assert.Len(t, queries[0].Rows, 0)
}
```

---

### Step 10: Full Integration Tests

End-to-end tests with a real Oracle database (Oracle XE 18c via testcontainers), a real client (go-ora), and the proxy in between. These tests exercise all three phases together.

**Files:** `oracle/integration_test.go`

**Build tag:** `//go:build integration`

#### Container Strategy

| Environment | Image | Why |
|-------------|-------|-----|
| **CI (default)** | `gvenzl/oracle-xe:18.4.0-slim` | No auth, TTC-compatible with 19c, ~1.5GB, ~2min |
| **CI (matrix)** | `gvenzl/oracle-free:23-slim` | Forward-compat with 23ai |
| **Local (optional)** | `container-registry.oracle.com/database/enterprise:19.3.0.0` | Real 19c (Oracle SSO required) |

```bash
make test-e2e-oracle                                                    # Default: 18c XE
ORACLE_TEST_IMAGE=gvenzl/oracle-free:23-slim make test-e2e-oracle       # 23ai
ORACLE_TEST_IMAGE=container-registry.oracle.com/database/enterprise:19.3.0.0 make test-e2e-oracle  # Real 19c
```

#### Tests

```go
//go:build integration

const defaultOracleImage = "gvenzl/oracle-xe:18.4.0-slim"

func oracleTestImage() string {
    if img := os.Getenv("ORACLE_TEST_IMAGE"); img != "" {
        return img
    }
    return defaultOracleImage
}

func TestIntegration_Setup(t *testing.T) {
    oracleC := startOracleContainer(t)
    defer oracleC.Terminate(context.Background())
    dsn := oracleC.MustConnectionString(t)
    assert.NotEmpty(t, dsn)
    t.Logf("Oracle container started: image=%s dsn=%s", oracleTestImage(), dsn)
}

func TestIntegration_ConnectThroughProxy(t *testing.T) {
    env := setupIntegrationEnv(t)
    defer env.Cleanup()

    db := env.ConnectAsUser("TESTUSER", "testpass")
    defer db.Close()

    err := db.PingContext(context.Background())
    require.NoError(t, err)
}

func TestIntegration_SimpleQuery(t *testing.T) {
    env := setupIntegrationEnv(t)
    defer env.Cleanup()

    db := env.ConnectAsUser("TESTUSER", "testpass")
    defer db.Close()

    var result string
    err := db.QueryRowContext(context.Background(), "SELECT 'hello' FROM DUAL").Scan(&result)
    require.NoError(t, err)
    assert.Equal(t, "hello", result)

    time.Sleep(100 * time.Millisecond)
    queries := env.ListQueriesForUser("TESTUSER")
    require.True(t, len(queries) >= 1)
    assert.Contains(t, queries[0].SQLText, "DUAL")
}

func TestIntegration_QueryWithBinds(t *testing.T) {
    env := setupIntegrationEnv(t)
    defer env.Cleanup()

    db := env.ConnectAsUser("TESTUSER", "testpass")
    defer db.Close()

    env.ExecUpstream("CREATE TABLE test_binds (id NUMBER, name VARCHAR2(100))")
    env.ExecUpstream("INSERT INTO test_binds VALUES (1, 'Alice')")
    env.ExecUpstream("INSERT INTO test_binds VALUES (2, 'Bob')")
    env.ExecUpstream("COMMIT")

    var name string
    err := db.QueryRowContext(context.Background(), "SELECT name FROM test_binds WHERE id = :1", 1).Scan(&name)
    require.NoError(t, err)
    assert.Equal(t, "Alice", name)

    time.Sleep(100 * time.Millisecond)
    queries := env.ListQueriesForUser("TESTUSER")
    found := false
    for _, q := range queries {
        if strings.Contains(q.SQLText, "test_binds") {
            found = true
            assert.NotNil(t, q.Parameters)
            assert.Contains(t, q.Parameters.Values, "1")
        }
    }
    assert.True(t, found)
}

func TestIntegration_ResultCapture(t *testing.T) {
    env := setupIntegrationEnv(t)
    env.QueryStorage = config.QueryStorageConfig{StoreResults: true, MaxResultRows: 100, MaxResultBytes: 1048576}
    defer env.Cleanup()

    db := env.ConnectAsUser("TESTUSER", "testpass")
    defer db.Close()

    env.ExecUpstream("CREATE TABLE test_capture (id NUMBER, val VARCHAR2(50))")
    env.ExecUpstream("INSERT INTO test_capture VALUES (1, 'alpha')")
    env.ExecUpstream("INSERT INTO test_capture VALUES (2, 'beta')")
    env.ExecUpstream("COMMIT")

    rows, _ := db.QueryContext(context.Background(), "SELECT id, val FROM test_capture ORDER BY id")
    defer rows.Close()
    for rows.Next() {
        var id int; var val string
        rows.Scan(&id, &val)
    }

    time.Sleep(200 * time.Millisecond)
    queriesWithRows := env.ListQueriesWithRowsForUser("TESTUSER")
    found := false
    for _, q := range queriesWithRows {
        if strings.Contains(q.SQLText, "test_capture") {
            found = true
            assert.Len(t, q.Rows, 2)
            var row0 map[string]interface{}
            json.Unmarshal(q.Rows[0].RowData, &row0)
            assert.Equal(t, "1", row0["ID"])
            assert.Equal(t, "alpha", row0["VAL"])
        }
    }
    assert.True(t, found)
}

func TestIntegration_ReadOnlyEnforcement(t *testing.T) {
    env := setupIntegrationEnv(t)
    defer env.Cleanup()

    env.SetGrantControls("TESTUSER", "TESTDB", []string{store.ControlReadOnly})

    db := env.ConnectAsUser("TESTUSER", "testpass")
    defer db.Close()

    var n int
    assert.NoError(t, db.QueryRowContext(context.Background(), "SELECT 1 FROM DUAL").Scan(&n))
    _, err := db.ExecContext(context.Background(), "CREATE TABLE t (id NUMBER)")
    assert.Error(t, err)
    assert.Contains(t, err.Error(), "ORA-01031")
}

func TestIntegration_DDLBlocking(t *testing.T) {
    env := setupIntegrationEnv(t)
    defer env.Cleanup()

    env.SetGrantControls("TESTUSER", "TESTDB", []string{store.ControlBlockDDL})

    db := env.ConnectAsUser("TESTUSER", "testpass")
    defer db.Close()

    env.ExecUpstream("CREATE TABLE test_ddl (id NUMBER)")
    _, err := db.ExecContext(context.Background(), "INSERT INTO test_ddl VALUES (1)")
    assert.NoError(t, err)
    _, err = db.ExecContext(context.Background(), "DROP TABLE test_ddl")
    assert.Error(t, err)
}

func TestIntegration_OracleSpecificBlocking(t *testing.T) {
    env := setupIntegrationEnv(t)
    defer env.Cleanup()

    env.SetGrantControls("TESTUSER", "TESTDB", []string{store.ControlReadOnly})

    db := env.ConnectAsUser("TESTUSER", "testpass")
    defer db.Close()

    for _, sql := range []string{
        "ALTER SYSTEM SET open_cursors=5000",
        "CREATE DATABASE LINK remote CONNECT TO sys IDENTIFIED BY pwd USING 'remote'",
    } {
        _, err := db.ExecContext(context.Background(), sql)
        assert.Error(t, err, "should block: %s", sql)
    }
}

func TestIntegration_ConnectionTracking(t *testing.T) {
    env := setupIntegrationEnv(t)
    defer env.Cleanup()

    db := env.ConnectAsUser("TESTUSER", "testpass")
    db.QueryRowContext(context.Background(), "SELECT 1 FROM DUAL")

    conns := env.Store.ListConnections(context.Background(), &store.ConnectionFilter{})
    require.True(t, len(conns) >= 1)
    assert.Nil(t, conns[0].DisconnectedAt)

    db.Close()
    time.Sleep(100 * time.Millisecond)

    updatedConn, _ := env.Store.GetConnection(context.Background(), conns[0].UID)
    assert.NotNil(t, updatedConn.DisconnectedAt)
    assert.True(t, updatedConn.Queries >= 1)
}

func TestIntegration_QuotaEnforcement(t *testing.T) {
    env := setupIntegrationEnv(t)
    defer env.Cleanup()

    maxQueries := int64(3)
    env.SetGrantQuota("TESTUSER", "TESTDB", &maxQueries, nil)

    db := env.ConnectAsUser("TESTUSER", "testpass")
    defer db.Close()

    for i := 0; i < 3; i++ {
        _, err := db.QueryContext(context.Background(), "SELECT 1 FROM DUAL")
        assert.NoError(t, err)
    }

    db.Close()
    db2, err := env.TryConnect("TESTUSER", "testpass")
    if err == nil {
        _, err = db2.QueryContext(context.Background(), "SELECT 1 FROM DUAL")
        db2.Close()
    }
}

func TestIntegration_MultipleDataTypes(t *testing.T) {
    env := setupIntegrationEnv(t)
    env.QueryStorage = config.QueryStorageConfig{StoreResults: true, MaxResultRows: 100, MaxResultBytes: 1048576}
    defer env.Cleanup()

    db := env.ConnectAsUser("TESTUSER", "testpass")
    defer db.Close()

    env.ExecUpstream(`CREATE TABLE test_types (
        num_col NUMBER(10,2), str_col VARCHAR2(100), date_col DATE,
        ts_col TIMESTAMP, raw_col RAW(16), float_col BINARY_FLOAT,
        double_col BINARY_DOUBLE, char_col CHAR(10)
    )`)
    env.ExecUpstream(`INSERT INTO test_types VALUES (
        42.50, 'hello', DATE '2024-03-15', TIMESTAMP '2024-03-15 14:30:00',
        HEXTORAW('DEADBEEF'), 3.14, 2.718281828, 'fixed'
    )`)
    env.ExecUpstream("COMMIT")

    rows, _ := db.QueryContext(context.Background(), "SELECT * FROM test_types")
    defer rows.Close()
    for rows.Next() {
        cols := make([]interface{}, 8)
        ptrs := make([]interface{}, 8)
        for i := range cols { ptrs[i] = &cols[i] }
        rows.Scan(ptrs...)
    }

    time.Sleep(200 * time.Millisecond)
    for _, q := range env.ListQueriesWithRowsForUser("TESTUSER") {
        if strings.Contains(q.SQLText, "test_types") {
            require.Len(t, q.Rows, 1)
            var row map[string]interface{}
            json.Unmarshal(q.Rows[0].RowData, &row)
            assert.Equal(t, "42.50", row["NUM_COL"])
            assert.Equal(t, "hello", row["STR_COL"])
            assert.Contains(t, row["DATE_COL"], "2024-03-15")
            assert.Equal(t, "deadbeef", row["RAW_COL"])
            assert.Equal(t, "fixed", row["CHAR_COL"])
        }
    }
}

func TestIntegration_ConcurrentSessions(t *testing.T) {
    env := setupIntegrationEnv(t)
    defer env.Cleanup()

    var wg sync.WaitGroup
    errors := make([]error, 10)

    for i := 0; i < 10; i++ {
        wg.Add(1)
        go func(idx int) {
            defer wg.Done()
            db := env.ConnectAsUser("TESTUSER", "testpass")
            defer db.Close()
            var n int
            errors[idx] = db.QueryRowContext(context.Background(), fmt.Sprintf("SELECT %d FROM DUAL", idx)).Scan(&n)
        }(i)
    }
    wg.Wait()
    for i, err := range errors {
        assert.NoError(t, err, "session %d failed", i)
    }
}

func TestIntegration_LargeResultSet(t *testing.T) {
    env := setupIntegrationEnv(t)
    defer env.Cleanup()

    db := env.ConnectAsUser("TESTUSER", "testpass")
    defer db.Close()

    rows, err := db.QueryContext(context.Background(), "SELECT LEVEL AS n FROM DUAL CONNECT BY LEVEL <= 10000")
    require.NoError(t, err)
    defer rows.Close()

    count := 0
    for rows.Next() { var n int; rows.Scan(&n); count++ }
    assert.Equal(t, 10000, count)

    time.Sleep(200 * time.Millisecond)
    for _, q := range env.ListQueriesForUser("TESTUSER") {
        if strings.Contains(q.SQLText, "CONNECT BY") {
            assert.Equal(t, int64(10000), *q.RowsAffected)
        }
    }
}

func TestIntegration_PLSQL(t *testing.T) {
    env := setupIntegrationEnv(t)
    defer env.Cleanup()

    db := env.ConnectAsUser("TESTUSER", "testpass")
    defer db.Close()

    env.ExecUpstream("CREATE TABLE test_plsql (val NUMBER)")

    _, err := db.ExecContext(context.Background(), "BEGIN INSERT INTO test_plsql VALUES (42); COMMIT; END;")
    require.NoError(t, err)

    time.Sleep(100 * time.Millisecond)
    found := false
    for _, q := range env.ListQueriesForUser("TESTUSER") {
        if strings.Contains(q.SQLText, "BEGIN") && strings.Contains(q.SQLText, "test_plsql") {
            found = true
        }
    }
    assert.True(t, found)
}

func TestIntegration_VersionDetection(t *testing.T) {
    env := setupIntegrationEnv(t)
    defer env.Cleanup()

    db := env.ConnectAsUser("TESTUSER", "testpass")
    defer db.Close()

    var banner string
    err := db.QueryRowContext(context.Background(), "SELECT banner FROM v$version WHERE ROWNUM = 1").Scan(&banner)
    require.NoError(t, err)
    t.Logf("Oracle version: %s", banner)

    image := oracleTestImage()
    switch {
    case strings.Contains(image, "oracle-xe:18"):
        assert.Contains(t, banner, "18c")
    case strings.Contains(image, "oracle-free:23"):
        assert.Contains(t, banner, "23")
    case strings.Contains(image, "enterprise:19"):
        assert.Contains(t, banner, "19c")
    }
}

// --- Test helpers ---

func startOracleContainer(t *testing.T) testcontainers.Container {
    t.Helper()
    ctx := context.Background()
    image := oracleTestImage()

    isXE := strings.Contains(image, "oracle-xe")
    isFree := strings.Contains(image, "oracle-free")
    isEnterprise := strings.Contains(image, "enterprise")

    env := map[string]string{}
    switch {
    case isXE:      env["ORACLE_PASSWORD"] = "oracle"
    case isFree:    env["ORACLE_PASSWORD"] = "oracle"
    case isEnterprise:
        env["ORACLE_SID"] = "ORCLCDB"
        env["ORACLE_PDB"] = "ORCLPDB1"
        env["ORACLE_PWD"] = "oracle"
    }

    timeout := 5 * time.Minute
    if isEnterprise { timeout = 10 * time.Minute }

    req := testcontainers.ContainerRequest{
        Image: image, ExposedPorts: []string{"1521/tcp"}, Env: env,
        WaitingFor: wait.ForLog("DATABASE IS READY TO USE!").WithStartupTimeout(timeout),
    }
    container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
        ContainerRequest: req, Started: true,
    })
    require.NoError(t, err)
    t.Logf("Oracle container ready: image=%s", image)
    return container
}
```

---

## Files Summary

| File | Type | Description |
|------|------|-------------|
| `internal/proxy/oracle/types.go` | New | Oracle data type decoders |
| `internal/proxy/oracle/types_test.go` | New | Type decoder tests |
| `internal/proxy/oracle/ttc_decode.go` | Modified | Add response parsing (column defs, rows, errors) |
| `internal/proxy/oracle/ttc_decode_test.go` | Modified | Add response parsing tests |
| `internal/proxy/oracle/intercept.go` | Modified | Add row capture, result storage |
| `internal/proxy/oracle/session.go` | Modified | Wire up response interception in upstreamToClient |
| `internal/proxy/oracle/integration_test.go` | New | Full E2E integration tests |
| `Makefile` | Modified | Add `test-e2e-oracle` target |

## Acceptance Criteria

1. Query results are captured and stored in `query_rows` table with correct column names and decoded values
2. Oracle NUMBER → string, DATE → ISO timestamp, VARCHAR2 → string, RAW → hex, CHAR → trimmed string
3. BINARY_FLOAT/DOUBLE → numeric, TIMESTAMP → ISO with fractional seconds
4. LOBs stored as `"[LOB]"` placeholder
5. Row capture respects `MaxResultRows` and `MaxResultBytes` — truncation discards all rows (same as PG)
6. Multi-fetch queries (OALL8 + OFETCH) accumulate rows correctly
7. `StoreResults: false` disables row capture
8. PL/SQL blocks are logged with their full text
9. All integration tests pass against Oracle XE 18c
10. Integration tests are configurable via `ORACLE_TEST_IMAGE` env var
11. JSON row format matches PG proxy format: `{"COL1": "value1", "COL2": "value2"}`
12. 10 concurrent sessions work correctly
13. Large result sets (10,000 rows) relay without data loss

## Estimated Size

~500 lines new Go code + ~300 lines ported from go-ora (type decoders) + ~150 lines refactored = **~950 lines total** (+ ~800 lines integration tests)
