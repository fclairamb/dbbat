# Spec: Store Query Result Rows

**Date**: 2026-01-08
**Status**: Draft
**Author**: Claude

## Summary

PgLens currently logs query execution metadata (SQL, duration, rows affected) but does not capture the actual result data. The `query_result_rows` table exists in the schema but is never populated. This prevents audit replay and limits observability for SELECT queries.

This specification outlines how to capture and store query result rows from `DataRow` messages in the PostgreSQL wire protocol.

## Problem Statement

### Observed Behavior

When a client executes a SELECT query through the proxy:

```sql
SELECT * FROM test_data;
```

The `queries` table is populated with:
- `sql_text`: "SELECT * FROM test_data"
- `duration_ms`: 2.5
- `rows_affected`: 3

But `query_result_rows` remains empty:

```sql
SELECT COUNT(*) FROM query_result_rows;
-- Result: 0
```

### Why This Matters

1. **Audit/Compliance**: Auditors need to see exactly what data was accessed, not just that a query ran
2. **Data Access Tracking**: For security reviews, knowing which rows were retrieved is critical
3. **Query Replay**: Cannot reproduce query results without the actual data
4. **Debugging**: When investigating issues, seeing the actual returned data is invaluable

### PostgreSQL Protocol Background

When a query returns data:

1. **RowDescription**: Sent once, contains column metadata (names, types, OIDs)
2. **DataRow**: Sent for each result row, contains column values as `[][]byte`
3. **CommandComplete**: Indicates query finished, contains row count

Current implementation (`internal/proxy/session.go:260-264`):

```go
case *pgproto3.DataRow:
    // Track bytes transferred (approximate size of row data)
    for _, val := range m.Values {
        bytesTransferred += int64(len(val))
    }
```

The `DataRow` values are used only for byte counting, then discarded.

### Existing Infrastructure

The store already has the infrastructure for this:

**Model** (`internal/store/models.go:104-119`):
```go
type QueryResultRow struct {
    ID           int64           `bun:"id,pk,autoincrement" json:"id"`
    QueryID      int64           `bun:"query_id,notnull" json:"query_id"`
    RowNumber    int             `bun:"row_number,notnull" json:"row_number"`
    RowData      json.RawMessage `bun:"row_data,notnull,type:jsonb" json:"row_data"`
    RowSizeBytes int64           `bun:"row_size_bytes,notnull" json:"row_size_bytes"`
}
```

**Store method** (`internal/store/queries.go:40-64`):
```go
func (s *Store) StoreQueryRows(ctx context.Context, queryID int64, rows []QueryRow) error
```

This method is never called.

## Design

### Goals

1. **Complete audit trail**: Store actual result data for SELECT queries
2. **Configurable limits**: Control storage via max rows and max bytes settings
3. **Minimal performance impact**: Capture asynchronously, don't block proxy
4. **Structured data**: Store rows as JSON with column names for readability

### Storage Format

Result rows are stored as JSONB with column names as keys:

```json
{
  "id": 1,
  "name": "Test 1",
  "value": 100,
  "created_at": "2024-01-01T00:00:00Z"
}
```

This format is:
- Human-readable in API responses
- Queryable via PostgreSQL JSONB operators
- Self-describing (includes column names)

### Configurable Limits

Add configuration options to control storage:

| Option | Default | Description |
|--------|---------|-------------|
| `max_result_rows` | 100,000 | Maximum rows to store per query |
| `max_result_bytes` | 100MB | Maximum total bytes to store per query |
| `store_results` | true | Enable/disable result storage globally |

When limits are exceeded:
- **Refuse to store**: Discard ALL captured rows for that query (store nothing)
- Log a warning with details about the exceeded limits
- Continue proxying to client (never block the query itself)

### State Tracking

Add fields to track result capture during query execution:

```go
type pendingQuery struct {
    sql             string
    startTime       time.Time
    parameters      *store.QueryParameters
    columnNames     []string            // From RowDescription
    columnOIDs      []uint32            // Type OIDs for decoding
    capturedRows    []store.QueryRow    // Accumulated result rows
    capturedBytes   int64               // Total bytes captured
    rowNumber       int                 // Current row counter
    truncated       bool                // True if limits exceeded
}
```

## Implementation

### 1. Configuration Changes

**File**: `internal/config/config.go`

Add result storage configuration:

```go
type QueryStorageConfig struct {
    MaxResultRows  int   `koanf:"max_result_rows"`
    MaxResultBytes int64 `koanf:"max_result_bytes"`
    StoreResults   bool  `koanf:"store_results"`
}
```

Default values:
```go
MaxResultRows:  100000,
MaxResultBytes: 100 * 1024 * 1024, // 100MB
StoreResults:   true,
```

### 2. Update State Structures

**File**: `internal/proxy/session.go`

```go
type pendingQuery struct {
    sql             string
    startTime       time.Time
    parameters      *store.QueryParameters

    // Result capture state
    columnNames     []string
    columnOIDs      []uint32
    capturedRows    []store.QueryRow
    capturedBytes   int64
    rowNumber       int
    truncated       bool
}
```

### 3. Handle RowDescription

**File**: `internal/proxy/session.go`

In `proxyUpstreamToClient()`, capture column metadata:

```go
case *pgproto3.RowDescription:
    // Capture column metadata for the current/pending query
    query := s.getCurrentPendingQuery()
    if query != nil {
        query.columnNames = make([]string, len(m.Fields))
        query.columnOIDs = make([]uint32, len(m.Fields))
        for i, field := range m.Fields {
            query.columnNames[i] = string(field.Name)
            query.columnOIDs[i] = field.DataTypeOID
        }
    }
```

### 4. Handle DataRow

**File**: `internal/proxy/session.go`

In `proxyUpstreamToClient()`, capture row data:

```go
case *pgproto3.DataRow:
    // Track bytes transferred
    rowSize := int64(0)
    for _, val := range m.Values {
        rowSize += int64(len(val))
    }
    bytesTransferred += rowSize

    // Capture row data if enabled and within limits
    query := s.getCurrentPendingQuery()
    if query != nil && s.queryStorage.StoreResults && !query.truncated {
        // Check if this row would exceed limits
        if query.rowNumber >= s.queryStorage.MaxResultRows ||
           query.capturedBytes+rowSize > s.queryStorage.MaxResultBytes {
            // Limits exceeded - discard all captured rows and stop capturing
            query.truncated = true
            query.capturedRows = nil // Discard all previously captured rows
            s.logger.Warn("result capture refused - limits exceeded",
                "rows_captured", query.rowNumber,
                "bytes_captured", query.capturedBytes,
                "max_rows", s.queryStorage.MaxResultRows,
                "max_bytes", s.queryStorage.MaxResultBytes)
        } else {
            row := s.convertDataRow(m.Values, query.columnNames, query.columnOIDs)
            query.capturedRows = append(query.capturedRows, row)
            query.capturedBytes += rowSize
            query.rowNumber++
        }
    }
```

### 5. Convert DataRow to JSON

**File**: `internal/proxy/intercept.go`

Add a function to convert DataRow values to JSON:

```go
// convertDataRow converts a DataRow to a QueryRow with JSON data
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
        // Fallback: store raw values as base64
        jsonData = []byte(fmt.Sprintf(`{"error":"marshal failed","raw":%q}`,
            base64.StdEncoding.EncodeToString(values[0])))
    }

    return store.QueryRow{
        RowData:      jsonData,
        RowSizeBytes: rowSize,
    }
}

// decodeColumnValue decodes a column value based on its type OID
func decodeColumnValue(data []byte, oid uint32) interface{} {
    // Text format: values are already string representations
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
```

### 6. Update Query Logging

**File**: `internal/proxy/intercept.go`

Update `logQuery()` to store captured rows:

```go
func (s *Session) logQuery(rowsAffected *int64, queryError *string, bytesTransferred int64) {
    if s.currentQuery == nil {
        return
    }

    duration := float64(time.Since(s.currentQuery.startTime).Milliseconds())

    query := &store.Query{
        ConnectionID: s.connectionID,
        SQLText:      s.currentQuery.sql,
        Parameters:   s.currentQuery.parameters,
        ExecutedAt:   s.currentQuery.startTime,
        DurationMs:   &duration,
        RowsAffected: rowsAffected,
        Error:        queryError,
    }

    // Capture rows from current query
    capturedRows := s.currentQuery.capturedRows

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

            if err := s.store.StoreQueryRows(s.ctx, createdQuery.ID, capturedRows); err != nil {
                s.logger.Error("failed to store query rows", "error", err)
            }
        }

        // Update connection stats
        if err := s.store.IncrementConnectionStats(s.ctx, s.connectionID, bytesTransferred); err != nil {
            s.logger.Error("failed to increment connection stats", "error", err)
        }
    }()

    // Update local grant state for in-session quota checks
    s.grant.QueryCount++
    s.grant.BytesTransferred += bytesTransferred
}
```

### 7. Helper to Get Current Pending Query

**File**: `internal/proxy/session.go`

```go
// getCurrentPendingQuery returns the query that should receive result data
// For Simple Query Protocol: s.currentQuery
// For Extended Query Protocol: last item in pendingQueries (most recent Execute)
func (s *Session) getCurrentPendingQuery() *pendingQuery {
    // Simple Query Protocol
    if s.currentQuery != nil {
        return s.currentQuery
    }

    // Extended Query Protocol - return the most recent pending query
    if len(s.extendedState.pendingQueries) > 0 {
        return s.extendedState.pendingQueries[len(s.extendedState.pendingQueries)-1]
    }

    return nil
}
```

### 8. Initialize Captured Rows

**File**: `internal/proxy/intercept.go`

Update `handleQuery()` to initialize capture state:

```go
func (s *Session) handleQuery(query *pgproto3.Query) error {
    sqlText := query.String

    if err := s.checkQuotas(); err != nil {
        return err
    }

    if s.grant.AccessLevel == "read" && isWriteQuery(sqlText) {
        return ErrWriteNotPermitted
    }

    s.currentQuery = &pendingQuery{
        sql:          sqlText,
        startTime:    time.Now(),
        capturedRows: make([]store.QueryRow, 0),  // Initialize for capture
    }

    return nil
}
```

Similarly for `handleExecute()`:

```go
func (s *Session) handleExecute(msg *pgproto3.Execute) error {
    if err := s.checkQuotas(); err != nil {
        return err
    }

    portal := s.extendedState.portals[msg.Portal]
    if portal == nil {
        s.logger.Warn("execute for unknown portal", "portal", msg.Portal)
        return nil
    }

    stmt := s.extendedState.preparedStatements[portal.stmtName]
    sqlText := ""
    if stmt != nil {
        sqlText = stmt.sql
    }

    query := &pendingQuery{
        sql:          sqlText,
        startTime:    time.Now(),
        parameters:   portal.parameters,
        capturedRows: make([]store.QueryRow, 0),  // Initialize for capture
    }
    s.extendedState.pendingQueries = append(s.extendedState.pendingQueries, query)

    return nil
}
```

## Testing

### Unit Tests

**File**: `internal/proxy/intercept_test.go`

```go
func TestDecodeColumnValue(t *testing.T) {
    tests := []struct {
        name     string
        data     []byte
        oid      uint32
        expected interface{}
    }{
        {"int4", []byte("42"), 23, int64(42)},
        {"int8", []byte("9223372036854775807"), 20, int64(9223372036854775807)},
        {"bool_true", []byte("t"), 16, true},
        {"bool_false", []byte("f"), 16, false},
        {"float8", []byte("3.14159"), 701, 3.14159},
        {"text", []byte("hello world"), 25, "hello world"},
        {"varchar", []byte("test"), 1043, "test"},
        {"timestamp", []byte("2024-01-01 00:00:00"), 1114, "2024-01-01 00:00:00"},
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            result := decodeColumnValue(tt.data, tt.oid)
            if result != tt.expected {
                t.Errorf("got %v (%T), want %v (%T)", result, result, tt.expected, tt.expected)
            }
        })
    }
}

func TestConvertDataRow(t *testing.T) {
    s := &Session{
        logger: slog.Default(),
    }

    values := [][]byte{
        []byte("1"),
        []byte("Test Name"),
        []byte("100"),
    }
    columnNames := []string{"id", "name", "value"}
    columnOIDs := []uint32{23, 25, 23} // int4, text, int4

    row := s.convertDataRow(values, columnNames, columnOIDs)

    var data map[string]interface{}
    if err := json.Unmarshal(row.RowData, &data); err != nil {
        t.Fatalf("failed to unmarshal row data: %v", err)
    }

    if data["id"] != float64(1) { // JSON numbers are float64
        t.Errorf("id: got %v, want 1", data["id"])
    }
    if data["name"] != "Test Name" {
        t.Errorf("name: got %v, want 'Test Name'", data["name"])
    }
}

func TestResultCaptureLimits(t *testing.T) {
    // Test that capture stops at configured limits
    query := &pendingQuery{
        sql:          "SELECT * FROM big_table",
        startTime:    time.Now(),
        capturedRows: make([]store.QueryRow, 0),
    }

    maxRows := 10

    // Simulate capturing rows
    for i := 0; i < 100; i++ {
        if query.rowNumber < maxRows {
            query.capturedRows = append(query.capturedRows, store.QueryRow{
                RowData: []byte(`{"id":1}`),
            })
            query.rowNumber++
        } else if !query.truncated {
            query.truncated = true
        }
    }

    if len(query.capturedRows) != maxRows {
        t.Errorf("captured %d rows, want %d", len(query.capturedRows), maxRows)
    }
    if !query.truncated {
        t.Error("truncated should be true")
    }
}
```

### Integration Test

**File**: `internal/proxy/session_test.go`

```go
func TestResultRowsStoredIntegration(t *testing.T) {
    store := setupTestStore(t)
    // ... setup proxy and execute SELECT query ...

    // Verify rows were stored
    queryWithRows, err := store.GetQueryWithRows(ctx, queryID)
    if err != nil {
        t.Fatalf("GetQueryWithRows: %v", err)
    }

    if len(queryWithRows.Rows) == 0 {
        t.Fatal("no rows stored")
    }

    // Verify row data structure
    var rowData map[string]interface{}
    if err := json.Unmarshal(queryWithRows.Rows[0].RowData, &rowData); err != nil {
        t.Fatalf("failed to unmarshal row data: %v", err)
    }

    // Should have column names as keys
    if _, ok := rowData["id"]; !ok {
        t.Error("row data missing 'id' column")
    }
}
```

## Files to Modify

| File | Changes |
|------|---------|
| `internal/config/config.go` | Add query storage config options |
| `internal/proxy/session.go` | Update `pendingQuery` struct, add `getCurrentPendingQuery()`, handle `RowDescription` |
| `internal/proxy/intercept.go` | Add `convertDataRow()`, `decodeColumnValue()`, update `logQuery()` |
| `internal/proxy/intercept_test.go` | Add tests for column decoding and row conversion |

No migration needed - the `query_result_rows` table already exists.

## Verification

1. **Run tests**: `go test ./...`

2. **Manual test**:
   ```bash
   # Execute SELECT query through proxy
   PGPASSWORD=testpass psql -h localhost -p 5001 -U testuser -d proxy_target \
     -c "SELECT * FROM test_data;"

   # Check stored results
   curl -s -u admin:admin "http://localhost:8080/api/queries?limit=1" | jq '.[0]'

   # Get query with rows
   curl -s -u admin:admin "http://localhost:8080/api/queries/1" | jq '.rows'
   ```

   Expected output:
   ```json
   {
     "id": 1,
     "sql_text": "SELECT * FROM test_data;",
     "rows_affected": 3,
     "rows": [
       {
         "row_number": 1,
         "row_data": {"id": 1, "name": "Test 1", "value": 100},
         "row_size_bytes": 24
       },
       {
         "row_number": 2,
         "row_data": {"id": 2, "name": "Test 2", "value": 200},
         "row_size_bytes": 24
       }
     ]
   }
   ```

## Risks and Mitigations

### Risk: Storage Bloat

Storing all result rows could quickly fill the database.

**Mitigation**:
- Default limit of 100 rows per query
- Default limit of 1MB per query
- Configuration options to tune or disable
- Add retention policy (future: auto-delete rows older than X days)

### Risk: Sensitive Data

Query results may contain PII, passwords, or other sensitive data.

**Mitigation**:
- Document this behavior clearly
- Provide configuration to disable result storage
- Future: Add per-database setting to disable result storage
- Future: Add column masking based on patterns

### Risk: Performance Impact

Converting and storing rows adds overhead.

**Mitigation**:
- Storage is asynchronous (goroutine)
- Limits prevent unbounded capture
- Only store when within limits
- Type decoding is simple string parsing

### Risk: Memory Pressure

Large result sets could consume memory before async storage.

**Mitigation**:
- Configurable limits cap memory usage
- Rows are stored as `json.RawMessage` (byte slices)
- Max 100 rows * 10KB average = ~1MB max per query

## Success Criteria

1. SELECT queries have result rows stored in `query_result_rows`
2. `GET /api/queries/:id` returns query with rows
3. Configurable limits are respected
4. Performance impact is negligible (< 5% latency increase)
5. All existing tests pass
6. New tests cover row capture and limits
