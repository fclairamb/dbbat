# Spec: Capture Query Parameters in Extended Query Protocol

**Date**: 2026-01-08
**Status**: Draft
**Author**: Claude

## Summary

PgLens currently logs the SQL template for prepared statements (e.g., `UPDATE test_data SET name=$1 WHERE id=$2`) but does not capture the actual parameter values. This makes audit logs incomplete and prevents query replay.

This specification outlines how to capture and store parameter values from the Extended Query Protocol's `Bind` message.

## Problem Statement

### Observed Behavior

When a client executes a parameterized query, the logs show:

```json
{"msg":"received message from client","message":{"Type":"Parse","Name":"","Query":"UPDATE public.test_data\n\tSET name=$1\n\tWHERE id=$2","ParameterOIDs":[1043,20]}}
{"msg":"received message from client","message":{"Type":"Bind","DestinationPortal":"","PreparedStatement":"","ParameterFormatCodes":[0,1],"Parameters":["TmV3TmFtZQ==","AAAAAAAAAC0="],...}}
```

But in the database, only the SQL template is stored:

```sql
SELECT sql_text FROM queries WHERE id = 123;
-- Result: "UPDATE public.test_data SET name=$1 WHERE id=$2"
```

The actual values (`$1='NewName'`, `$2=45`) are lost.

### Why This Matters

1. **Audit/Compliance**: Auditors need to know exactly what data was accessed or modified
2. **Debugging**: When investigating issues, seeing `WHERE id=$1` is less useful than `WHERE id=45`
3. **Query Replay**: Cannot reproduce the exact query without parameter values

### PostgreSQL Protocol Background

In the Extended Query Protocol:

1. **Parse**: Contains SQL template with `$1, $2, ...` placeholders and `ParameterOIDs` (type info)
2. **Bind**: Contains actual parameter values as `[][]byte` and `ParameterFormatCodes` (text=0, binary=1)
3. **Execute**: Runs the bound query

Current implementation (`internal/proxy/intercept.go:58-61`):

```go
func (s *Session) handleBind(msg *pgproto3.Bind) {
    // Map portal to prepared statement - parameters are IGNORED
    s.extendedState.portals[msg.DestinationPortal] = msg.PreparedStatement
}
```

## Design

### Goals

1. **Audit visibility**: Human-readable parameter values in API/logs
2. **Query replay**: Store enough data to re-execute the exact query

### Storage Format

Add a `parameters` JSONB column to the `queries` table:

```json
{
  "values": ["NewName", "45"],
  "raw": ["TmV3TmFtZQ==", "AAAAAAAAAC0="],
  "format_codes": [0, 1],
  "type_oids": [1043, 20]
}
```

| Field | Purpose |
|-------|---------|
| `values` | Human-readable decoded values (for audit) |
| `raw` | Base64-encoded original bytes (for replay) |
| `format_codes` | 0=text, 1=binary per parameter |
| `type_oids` | PostgreSQL type OIDs from Parse message |

### Why Store Both `values` and `raw`?

- **Text format** (`format_code=0`): Value is already a string, but we keep raw for consistency
- **Binary format** (`format_code=1`): Value requires type-aware decoding; raw preserves exact bytes for replay

### State Tracking Changes

Current state structure tracks SQL but not parameter info:

```go
type extendedQueryState struct {
    preparedStatements map[string]string  // stmt -> SQL only
    portals            map[string]string  // portal -> stmt name only
}
```

New structure tracks full context:

```go
type preparedStatement struct {
    sql      string
    typeOIDs []uint32
}

type portalState struct {
    stmtName   string
    parameters *store.QueryParameters
}

type extendedQueryState struct {
    preparedStatements map[string]*preparedStatement
    portals            map[string]*portalState
    pendingQueries     []*pendingQuery
}
```

## Implementation

### 1. Database Migration

**File**: `internal/migrations/sql/20260108100000_add_query_parameters.up.sql`

```sql
ALTER TABLE queries ADD COLUMN parameters JSONB;
```

**File**: `internal/migrations/sql/20260108100000_add_query_parameters.down.sql`

```sql
ALTER TABLE queries DROP COLUMN parameters;
```

### 2. Model Changes

**File**: `internal/store/models.go`

```go
// QueryParameters stores parameter values for prepared statements
type QueryParameters struct {
    Values      []string `json:"values"`                 // Decoded string representation
    Raw         []string `json:"raw,omitempty"`          // Base64-encoded raw bytes
    FormatCodes []int16  `json:"format_codes,omitempty"` // 0=text, 1=binary
    TypeOIDs    []uint32 `json:"type_oids,omitempty"`    // PostgreSQL type OIDs
}

type Query struct {
    // ... existing fields ...
    Parameters *QueryParameters `bun:"parameters,type:jsonb" json:"parameters,omitempty"`
}
```

### 3. State Structure Changes

**File**: `internal/proxy/session.go`

```go
type pendingQuery struct {
    sql        string
    startTime  time.Time
    parameters *store.QueryParameters
}

type preparedStatement struct {
    sql      string
    typeOIDs []uint32
}

type portalState struct {
    stmtName   string
    parameters *store.QueryParameters
}

type extendedQueryState struct {
    preparedStatements map[string]*preparedStatement
    portals            map[string]*portalState
    pendingQueries     []*pendingQuery
}
```

### 4. Intercept Handler Changes

**File**: `internal/proxy/intercept.go`

#### handleParse - Store OIDs

```go
func (s *Session) handleParse(msg *pgproto3.Parse) error {
    sqlText := msg.Query

    if s.grant.AccessLevel == "read" && isWriteQuery(sqlText) {
        return ErrWriteNotPermitted
    }

    s.extendedState.preparedStatements[msg.Name] = &preparedStatement{
        sql:      sqlText,
        typeOIDs: msg.ParameterOIDs,
    }

    return nil
}
```

#### handleBind - Capture Parameters

```go
func (s *Session) handleBind(msg *pgproto3.Bind) {
    stmt := s.extendedState.preparedStatements[msg.PreparedStatement]

    var typeOIDs []uint32
    if stmt != nil {
        typeOIDs = stmt.typeOIDs
    }

    params := &store.QueryParameters{
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

    s.extendedState.portals[msg.DestinationPortal] = &portalState{
        stmtName:   msg.PreparedStatement,
        parameters: params,
    }
}
```

#### handleExecute - Include Parameters

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
        sql:        sqlText,
        startTime:  time.Now(),
        parameters: portal.parameters,
    }
    s.extendedState.pendingQueries = append(s.extendedState.pendingQueries, query)

    return nil
}
```

### 5. Binary Parameter Decoder

**File**: `internal/proxy/intercept.go`

```go
import (
    "encoding/base64"
    "encoding/binary"
    "math"
    "strconv"
)

// decodeBinaryParameter decodes a binary-format PostgreSQL parameter
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

func getTypeOID(oids []uint32, idx int) uint32 {
    if idx < len(oids) {
        return oids[idx]
    }
    return 0 // Unknown
}
```

### 6. Update Query Logging

**File**: `internal/proxy/intercept.go`

```go
func (s *Session) logQuery(rowsAffected *int64, queryError *string, bytesTransferred int64) {
    if s.currentQuery == nil {
        return
    }

    duration := float64(time.Since(s.currentQuery.startTime).Milliseconds())

    query := &store.Query{
        ConnectionID: s.connectionID,
        SQLText:      s.currentQuery.sql,
        ExecutedAt:   s.currentQuery.startTime,
        DurationMs:   &duration,
        RowsAffected: rowsAffected,
        Error:        queryError,
        Parameters:   s.currentQuery.parameters, // NEW
    }

    // ... rest unchanged ...
}
```

## Testing

### Unit Tests

**File**: `internal/proxy/intercept_test.go`

```go
func TestDecodeBinaryParameter(t *testing.T) {
    tests := []struct {
        name     string
        data     []byte
        oid      uint32
        expected string
    }{
        {"int4", []byte{0, 0, 0, 42}, 23, "42"},
        {"int8", []byte{0, 0, 0, 0, 0, 0, 0, 45}, 20, "45"},
        {"bool_true", []byte{1}, 16, "true"},
        {"bool_false", []byte{0}, 16, "false"},
        {"text", []byte("hello"), 25, "hello"},
        {"varchar", []byte("world"), 1043, "world"},
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            result := decodeBinaryParameter(tt.data, tt.oid)
            if result != tt.expected {
                t.Errorf("got %q, want %q", result, tt.expected)
            }
        })
    }
}

func TestHandleBindCapturesParameters(t *testing.T) {
    s := &Session{
        grant: &store.Grant{AccessLevel: "write"},
        extendedState: &extendedQueryState{
            preparedStatements: map[string]*preparedStatement{
                "": {sql: "SELECT $1, $2", typeOIDs: []uint32{25, 23}},
            },
            portals: make(map[string]*portalState),
        },
    }

    s.handleBind(&pgproto3.Bind{
        DestinationPortal:    "",
        PreparedStatement:    "",
        ParameterFormatCodes: []int16{0, 1}, // text, binary
        Parameters:           [][]byte{[]byte("hello"), {0, 0, 0, 42}},
    })

    portal := s.extendedState.portals[""]
    if portal == nil {
        t.Fatal("portal not created")
    }

    params := portal.parameters
    if params.Values[0] != "hello" {
        t.Errorf("param 0: got %q, want %q", params.Values[0], "hello")
    }
    if params.Values[1] != "42" {
        t.Errorf("param 1: got %q, want %q", params.Values[1], "42")
    }
}
```

### Integration Test

```go
func TestParameterCaptureIntegration(t *testing.T) {
    store := setupTestStore(t)
    // ... setup proxy session ...

    // Simulate extended query protocol
    s.handleParse(&pgproto3.Parse{
        Name:          "",
        Query:         "UPDATE test SET name = $1 WHERE id = $2",
        ParameterOIDs: []uint32{1043, 20},
    })

    s.handleBind(&pgproto3.Bind{
        DestinationPortal:    "",
        PreparedStatement:    "",
        ParameterFormatCodes: []int16{0, 1},
        Parameters:           [][]byte{[]byte("NewName"), {0, 0, 0, 0, 0, 0, 0, 45}},
    })

    s.handleExecute(&pgproto3.Execute{Portal: ""})

    // Simulate CommandComplete + ReadyForQuery
    rowsAffected := int64(1)
    s.currentQuery = s.extendedState.pendingQueries[0]
    s.logQuery(&rowsAffected, nil, 0)

    // Verify in database
    queries, _ := store.ListQueries(ctx, store.QueryFilter{Limit: 1})
    if queries[0].Parameters == nil {
        t.Fatal("parameters not stored")
    }
    if queries[0].Parameters.Values[0] != "NewName" {
        t.Errorf("got %q, want %q", queries[0].Parameters.Values[0], "NewName")
    }
}
```

## Files to Modify

| File | Changes |
|------|---------|
| `internal/migrations/sql/20260108100000_add_query_parameters.up.sql` | New migration |
| `internal/migrations/sql/20260108100000_add_query_parameters.down.sql` | New migration |
| `internal/store/models.go` | Add `QueryParameters` struct, update `Query` |
| `internal/proxy/session.go` | Update state structures |
| `internal/proxy/intercept.go` | Update handlers, add decoder |
| `internal/proxy/intercept_test.go` | Add parameter capture tests |

## Verification

1. **Run migrations**: `./pglens db migrate`
2. **Run tests**: `go test ./...`
3. **Manual test**:
   ```bash
   # Execute parameterized query through proxy
   PGPASSWORD=testpass psql -h localhost -p 5001 -U testuser -d proxy_target \
     -c "PREPARE upd(text, int) AS UPDATE test_data SET name = \$1 WHERE id = \$2; EXECUTE upd('NewValue', 1);"

   # Check logged query
   curl -s -u admin:admin http://localhost:8080/api/queries | jq '.[-1]'
   ```

   Expected output:
   ```json
   {
     "sql_text": "UPDATE test_data SET name = $1 WHERE id = $2",
     "parameters": {
       "values": ["NewValue", "1"],
       "raw": ["TmV3VmFsdWU=", "AAAAAAAAAAAAQ="],
       "format_codes": [0, 1],
       "type_oids": [1043, 20]
     }
   }
   ```

## Risks and Mitigations

### Risk: Large Parameter Values

Binary blobs or large text values could bloat the queries table.

**Mitigation**:
- Consider truncating `values` for display (e.g., `"<1024 bytes>"`)
- Keep `raw` for replay but consider compression for large values
- Add configuration option for max parameter size to store

### Risk: Sensitive Data in Parameters

Passwords or PII in parameters would be logged.

**Mitigation**:
- Document this behavior clearly
- Consider optional parameter masking based on query patterns
- Future: Add per-database setting to disable parameter logging

### Risk: Performance Impact

Encoding/decoding adds CPU overhead.

**Mitigation**:
- Binary decoding is simple arithmetic operations
- Base64 encoding is fast
- Already logging queries asynchronously

## Success Criteria

1. Parameterized queries show actual values in API responses
2. `raw` + `format_codes` + `type_oids` sufficient for query replay
3. All existing tests pass
4. New tests cover parameter capture and decoding
5. No measurable performance regression in proxy throughput
