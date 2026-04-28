# MySQL Proxy — Phase 3: Result Capture

> Parent spec: `2026-04-25-mysql-proxy.md`
> Depends on: `2026-04-25-mysql-phase2-query-interception.md`

## Goal

Capture text-protocol result rows from `COM_QUERY` responses and store them in `query_rows`. Apply the existing `query_storage.max_result_rows` and `query_storage.max_result_bytes` limits. Rows show up in the existing UI and `/api/v1/queries/{uid}/rows` endpoint without any front-end work.

## Non-Goals (deferred to v2)

- Binary-protocol row capture (`COM_STMT_EXECUTE` results)
- Stored procedure multi-result-set capture beyond first set
- Streaming row capture (we capture in-memory then flush at end-of-result)

## Outcome

### Files added
```
internal/proxy/mysql/result.go          # text-protocol response parser
internal/proxy/mysql/result_test.go
internal/proxy/mysql/types.go           # MySQL column type → JSON value mapping
```

### Files modified
```
internal/proxy/mysql/intercept.go       # relayResponse calls into result.go
internal/proxy/mysql/session.go         # currentQuery captures rows
```

## MySQL Text-Protocol Response Layout

After `COM_QUERY`, the upstream sends:

```
1. Column count packet (length-encoded int)
2. N column definition packets (one per column)
3. EOF packet (or, if CLIENT_DEPRECATE_EOF capability, OK packet with EOF flag)
4. M row packets — each row is N length-encoded strings, NULL = 0xFB
5. EOF packet (or OK with EOF flag)
```

Or, for non-SELECT statements:
```
1. OK packet (with affected_rows + last_insert_id)
```

Or, on error:
```
1. ERR packet (with error code + message)
```

## Capture flow

```go
// internal/proxy/mysql/result.go

// CaptureTextResult reads the response from upstream, captures rows up to limits,
// forwards everything to the client, and returns the capture summary.
func (s *Session) captureTextResult() (*captureResult, error) {
    first, err := s.upstreamConn.ReadPacket()
    if err != nil { return nil, err }

    // Forward to client always; only capture if we recognize the structure
    if err := s.serverConn.WritePacket(first); err != nil { return nil, err }

    switch first[0] {
    case mysql.OK_HEADER:
        return s.parseOK(first), nil
    case mysql.ERR_HEADER:
        return s.parseERR(first), nil
    case 0xFB:
        // LOCAL INFILE — already handled in Phase 2 (refused)
        return nil, ErrLocalInfileNotPermitted
    default:
        // Result set: first packet is column count
        return s.captureResultSet(first)
    }
}

func (s *Session) captureResultSet(colCountPkt []byte) (*captureResult, error) {
    colCount, _, _ := mysql.LengthEncodedInt(colCountPkt)
    columns := make([]*mysql.Field, 0, colCount)
    for i := uint64(0); i < colCount; i++ {
        pkt, err := s.upstreamConn.ReadPacket()
        if err != nil { return nil, err }
        if err := s.serverConn.WritePacket(pkt); err != nil { return nil, err }
        f, err := mysql.FieldData(pkt).Parse()
        if err != nil { return nil, err }
        columns = append(columns, f)
    }

    // EOF after columns (CLIENT_DEPRECATE_EOF aware)
    if err := s.relayPacket(); err != nil { return nil, err }

    // Row capture loop
    captured := captureResult{}
    for {
        pkt, err := s.upstreamConn.ReadPacket()
        if err != nil { return nil, err }
        if err := s.serverConn.WritePacket(pkt); err != nil { return nil, err }

        if isEOF(pkt) || isOKWithEOFFlag(pkt) {
            captured.affectedRows = parseEOFOrOK(pkt)
            return &captured, nil
        }

        // Row packet
        if captured.totalBytes < s.queryStorage.MaxResultBytes &&
           captured.rowCount < s.queryStorage.MaxResultRows {
            row, sz, err := decodeTextRow(pkt, columns)
            if err == nil {
                captured.rows = append(captured.rows, row)
                captured.totalBytes += sz
                captured.rowCount++
            } else {
                s.logger.WarnContext(s.ctx, "row decode failed", slog.Any("error", err))
            }
        } else {
            captured.truncated = true
        }
    }
}
```

## Text-protocol row decoding

Each value in a text-protocol row is a length-encoded string; NULL is `0xFB`. The string is the value's text representation (ASCII). Type interpretation is informed by the column definition for serialization to JSON:

| MySQL Type | JSON representation |
|------------|---------------------|
| `MYSQL_TYPE_TINY`/`SHORT`/`LONG`/`LONGLONG`/`INT24` | `number` (parsed via `strconv.ParseInt`) |
| `MYSQL_TYPE_FLOAT`/`DOUBLE`/`DECIMAL`/`NEWDECIMAL` | `number` |
| `MYSQL_TYPE_BIT` | `number` (0 or 1) for single-bit, else base64 string |
| `MYSQL_TYPE_DATE` | `string` ("YYYY-MM-DD") |
| `MYSQL_TYPE_DATETIME`/`TIMESTAMP` | `string` ("YYYY-MM-DD HH:MM:SS[.fff]") |
| `MYSQL_TYPE_TIME` | `string` ("HH:MM:SS[.fff]") |
| `MYSQL_TYPE_YEAR` | `number` |
| `MYSQL_TYPE_VARCHAR`/`STRING`/`VAR_STRING`/`ENUM`/`SET` | `string` |
| `MYSQL_TYPE_JSON` | parsed JSON if valid, else string |
| `MYSQL_TYPE_TINY_BLOB`/`BLOB`/`MEDIUM_BLOB`/`LONG_BLOB`/`GEOMETRY` | base64 `string` (with type marker in JSON: `{"$bytes":"...","$type":"blob"}`) |
| NULL | JSON `null` |

The serialized row is a JSON array of these values, stored as `query_rows.row_data`. Same structure as PG's row capture — the existing API and UI render it as-is.

## Limit enforcement

Once `rowCount >= max_result_rows` OR `totalBytes >= max_result_bytes`:
- Stop capturing (continue forwarding to client uninterrupted)
- Mark `captured.truncated = true`
- Phase 3 logs a `WARN` with the truncation reason; the existing `queries.metadata` JSONB (if added — check) or a separate log line records it

The relay to the client is **never** truncated — we always forward what upstream sends. Truncation only affects what we *store*.

## Storage

Reuse `store.QueryRow`. After successful capture, in `recordQueryComplete`:

```go
for i, row := range capturedRows {
    qr := &store.QueryRow{
        UID:          uuid.NewV7(),
        QueryID:      query.UID,
        RowNumber:    i,
        RowData:      row.json,
        RowSizeBytes: row.size,
    }
    s.store.InsertQueryRow(s.ctx, qr)
}
```

(Or batch insert — match PG's pattern.)

## Tests

### Unit
- `result_test.go`:
  - `TestDecodeTextRow_Strings`
  - `TestDecodeTextRow_Numbers`
  - `TestDecodeTextRow_NULLs`
  - `TestDecodeTextRow_Dates`
  - `TestDecodeTextRow_JSON`
  - `TestDecodeTextRow_Blob_Base64`
  - `TestCapture_RespectsMaxRows`
  - `TestCapture_RespectsMaxBytes`
  - `TestCapture_OKPacket_NoCapture`
  - `TestCapture_ERRPacket_RecordsError`

### Integration (testcontainers MySQL 8.4)
- `TestSelect_RowsCaptured` — `SELECT a, b FROM t LIMIT 5` → 5 rows in `query_rows`
- `TestUpdate_AffectedRowsRecorded` — `UPDATE` → `queries.rows_affected` set, no `query_rows`
- `TestSelect_LimitTruncatesCapture` — set `max_result_rows=10`, query for 100, verify only 10 captured but client got 100
- `TestSelect_AllTypes` — table with int/varchar/datetime/json/blob columns, verify each type decoded

## Verification checklist

- [ ] `make lint` clean, `make test` passes
- [ ] Manual: query something and check `/api/v1/queries/{uid}/rows` returns rows
- [ ] Manual: frontend Query Detail page shows rows with no MySQL-specific changes (already protocol-agnostic)
- [ ] Truncation works as expected (set low limits in config, run a `SELECT` against a big table)

## Out of scope (v2 follow-up)

- Binary-protocol row capture (`COM_STMT_EXECUTE`)
- Multi-result-set capture (stored procs)
- Streaming row insertion (currently buffer-then-flush)
