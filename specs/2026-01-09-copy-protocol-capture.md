# COPY Protocol Data Capture

## Problem Statement

When a user executes a `COPY ... TO stdout` query (commonly used by `pg_dump`), the actual data being exported is not captured. The proxy logs the COPY query itself but misses all the data that flows through the COPY protocol.

### Example from logs

```
{"time":"2026-01-09T21:30:15.156136+01:00","level":"INFO","msg":"received message from client","message":{"Type":"Query","String":"COPY public.test_data (id, name, value, created_at) TO stdout;"}}
{"time":"2026-01-09T21:30:15.156732+01:00","level":"INFO","msg":"received message from client","message":{"Type":"Terminate"}}
```

The proxy sees the COPY query, but the actual table contents that were exported are completely invisible.

## Background: PostgreSQL COPY Protocol

The COPY command uses a special sub-protocol within the PostgreSQL wire protocol:

### COPY TO (data export from server)

```
Client                          Server
  |                                |
  |------ Query (COPY TO) -------->|
  |                                |
  |<----- CopyOutResponse ---------|  (indicates server will send data)
  |<----- CopyData ----------------|  (row 1)
  |<----- CopyData ----------------|  (row 2)
  |<----- CopyData ----------------|  ...
  |<----- CopyDone ----------------|  (transfer complete)
  |<----- CommandComplete ---------|
  |<----- ReadyForQuery -----------|
```

### COPY FROM (data import to server)

```
Client                          Server
  |                                |
  |------ Query (COPY FROM) ------>|
  |                                |
  |<----- CopyInResponse ----------|  (indicates server ready to receive)
  |------ CopyData --------------->|  (row 1)
  |------ CopyData --------------->|  (row 2)
  |------ CopyData --------------->|  ...
  |------ CopyDone --------------->|  (transfer complete)
  |<----- CommandComplete ---------|
  |<----- ReadyForQuery -----------|
```

### CopyData Message Format

Each `CopyData` message contains raw bytes in the format specified by the COPY command:
- Default: Tab-separated values with newline row terminators
- With `FORMAT csv`: CSV format
- With `FORMAT binary`: Binary format (PostgreSQL-specific)

## Proposed Solution

### 1. Track COPY Operation State

Add state tracking to detect when we're in a COPY operation:

```go
type copyState struct {
    inProgress    bool
    direction     string // "out" (TO) or "in" (FROM)
    format        string // "text", "csv", or "binary"
    tableName     string // Extracted from COPY query
    dataChunks    [][]byte // Captured data chunks
    totalBytes    int64
    rowCount      int64 // Approximate for text/csv (count newlines)
    truncated     bool
}
```

### 2. Intercept COPY-Related Messages

In `proxyUpstreamToClient()`:

```go
case *pgproto3.CopyOutResponse:
    // Server is about to send COPY data
    s.startCopyCapture("out", m.OverallFormat)

case *pgproto3.CopyInResponse:
    // Server is ready to receive COPY data
    s.startCopyCapture("in", m.OverallFormat)

case *pgproto3.CopyData:
    // Capture the data chunk
    s.captureCopyData(m.Data)

case *pgproto3.CopyDone:
    // COPY operation complete - finalize capture
    s.finalizeCopyCapture()
```

In `proxyClientToUpstream()` (for COPY FROM):

```go
case *pgproto3.CopyData:
    // Client sending data to server
    s.captureCopyData(m.Data)

case *pgproto3.CopyDone:
    // Client finished sending
    s.finalizeCopyCapture()

case *pgproto3.CopyFail:
    // Client aborted the COPY
    s.abortCopyCapture(m.Message)
```

### 3. Storage Considerations

#### Option A: Store as Query Result Rows (Recommended)

Parse the COPY data and store as regular query result rows. This provides:
- Consistent storage format with regular queries
- Searchable/queryable results
- Works well for text/CSV formats

For the example COPY query `COPY public.test_data (id, name, value, created_at) TO stdout`:
- Parse column names from the query
- Parse each line as a row
- Store in `query_rows` table

Challenges:
- Binary format requires PostgreSQL type knowledge to decode
- Large exports could exceed storage limits quickly

#### Option B: Store as Raw Blob

Store the raw COPY data as a binary blob:
- Add `copy_data BYTEA` column to `queries` table
- Simple implementation
- Preserves exact format
- Can be re-imported directly

Challenges:
- Not searchable
- May need separate viewer in frontend
- Storage size concerns

#### Option C: Hybrid Approach

- For text/CSV: Parse and store as rows (Option A)
- For binary: Store as blob (Option B)
- Apply same limits as regular queries (max rows, max bytes)

**Decision: Option A** - Store COPY data as query rows, reusing the existing `query_rows` table.

### 4. Schema Changes

Add to `queries` table:

```sql
ALTER TABLE queries ADD COLUMN copy_format TEXT;     -- 'text', 'csv', 'binary', or NULL
ALTER TABLE queries ADD COLUMN copy_direction TEXT;  -- 'in', 'out', or NULL
```

COPY data is stored in the existing `query_rows` table (renamed from `query_result_rows`):
- For text/CSV formats: Parse each line and store as a row with column values
- For binary format: Store raw chunks as rows (one row per CopyData message, or parsed if feasible)
- Same storage limits apply (`max_result_rows`, `max_result_bytes`)

The rename from `query_result_rows` to `query_rows` reflects that this table now stores both:
- Regular query result rows (from SELECT statements)
- COPY data rows (from COPY TO/FROM statements)

### 5. Configuration

COPY data capture reuses the existing `QueryStorageConfig` settings:

```go
type QueryStorageConfig struct {
    StoreResults   bool  // Also controls COPY data capture
    MaxResultRows  int   // Max rows to capture (applies to COPY too)
    MaxResultBytes int64 // Max bytes to capture (applies to COPY too)
}
```

No additional configuration needed - COPY operations follow the same storage rules as regular queries.

### 6. Security Considerations

- COPY operations can transfer large amounts of sensitive data
- Audit logging should always record that a COPY occurred, even if data isn't stored
- Consider rate limiting or alerting for large COPY operations
- Binary format may contain data that's harder to audit/review

### 7. Frontend Display

Add UI to view COPY operations:
- Show in query list with special indicator (e.g., "COPY OUT" badge)
- Display row count and bytes transferred
- For text/CSV: Show as table (same as query results)
- For binary: Show hex dump or "Binary format - X bytes"
- Download button to export captured data

## Implementation Plan

1. **Phase 1: Detection and Logging**
   - Add COPY message handling in proxy
   - Log COPY operations with metadata (direction, format, bytes, rows)
   - No data capture yet

2. **Phase 2: Text/CSV Capture**
   - Parse and store text/CSV COPY data as query result rows
   - Apply existing storage limits
   - Frontend displays like regular query results

3. **Phase 3: Binary Support**
   - Store binary COPY data as blob
   - Add dedicated viewer in frontend
   - Consider compression for storage efficiency

4. **Phase 4: COPY FROM Support**
   - Capture data being imported (client-to-server direction)
   - Same storage approach as COPY TO

## Alternative: Just Log Metadata

A minimal implementation could:
- Detect COPY operations
- Log: direction, format, bytes transferred, row count
- NOT store the actual data

This provides visibility into what was copied without storage overhead. The data itself would need to be recovered from the target database if needed.

## References

- [PostgreSQL Protocol - COPY](https://www.postgresql.org/docs/current/protocol-flow.html#PROTOCOL-COPY)
- [pgproto3 COPY messages](https://pkg.go.dev/github.com/jackc/pgx/v5/pgproto3)
