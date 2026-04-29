---
sidebar_position: 2
---

# Query Logging

DBBat logs every query executed through the proxy — across all supported engines (PostgreSQL, Oracle, MySQL, MariaDB) — providing complete visibility into database activity.

## What's Logged

For each query, DBBat records:

- **SQL text**: the complete query as sent by the client (or the prepared statement text for binary protocols)
- **Parameters**: bound parameters for prepared/extended-query statements (PostgreSQL extended query, MySQL `COM_STMT_EXECUTE`)
- **User**: which DBBat user executed the query
- **Database**: which target database the query ran against
- **Connection**: the connection UID (links to connection metadata)
- **Started at**: when the query started
- **Duration**: how long the query took (milliseconds)
- **Rows affected**: number of rows returned or modified
- **Error**: error text if the query failed
- **Result rows**: optionally captured up to `query_storage.max_result_rows` / `max_result_bytes`

### Engine-specific notes

- **PostgreSQL**: both Simple Query (`Q`) and Extended Query (`P`/`B`/`E`) are logged. Parameter values are stored as JSONB.
- **MySQL / MariaDB**: text protocol (`COM_QUERY`) and binary protocol (`COM_STMT_EXECUTE`) are decoded and stored uniformly. `COM_INIT_DB` is logged as `USE <db>`. `COM_PING` / `COM_QUIT` are not logged.
- **Oracle**: SQL is parsed out of TTC `Execute` (function `0x03`, sub-op `0x5e`) packets. Row capture works for `SELECT` results decoded from the first response and continuation packets; DML row counts are not captured from v315+ responses.

## Viewing Queries

List recent queries:

```bash
curl -H "Authorization: Bearer $TOKEN" \
  "http://localhost:4200/api/v1/queries"
```

### Filtering

```bash
# By user
curl -H "Authorization: Bearer $TOKEN" \
  "http://localhost:4200/api/v1/queries?user_id=$USER_UID"

# By database
curl -H "Authorization: Bearer $TOKEN" \
  "http://localhost:4200/api/v1/queries?database_id=$DB_UID"

# By time range (RFC 3339)
curl -H "Authorization: Bearer $TOKEN" \
  "http://localhost:4200/api/v1/queries?start_time=2024-01-15T00:00:00Z&end_time=2024-01-16T00:00:00Z"

# By connection
curl -H "Authorization: Bearer $TOKEN" \
  "http://localhost:4200/api/v1/queries?connection_id=$CONN_UID"
```

## Query Details

Get a single query (without rows):

```bash
curl -H "Authorization: Bearer $TOKEN" \
  http://localhost:4200/api/v1/queries/$QUERY_UID
```

Response:

```json
{
  "uid": "550e8400-e29b-41d4-a716-446655440000",
  "connection_id": "660e8400-e29b-41d4-a716-446655440000",
  "sql_text": "SELECT id, name FROM users WHERE active = $1",
  "parameters": {
    "values": ["true"],
    "format_codes": [0],
    "type_oids": [16]
  },
  "executed_at": "2024-01-15T10:30:00Z",
  "duration_ms": 12.5,
  "rows_affected": 5,
  "error": null
}
```

## Query Result Rows

Result rows are stored separately and fetched on demand with cursor-based pagination — capped at 1000 rows or 1 MB per response, whichever comes first.

```bash
curl -H "Authorization: Bearer $TOKEN" \
  "http://localhost:4200/api/v1/queries/$QUERY_UID/rows?limit=100"
```

Response:

```json
{
  "rows": [
    { "row_number": 0, "row_data": {"id": 1, "name": "Alice"}, "row_size_bytes": 32 }
  ],
  "next_cursor": "eyJvZmZzZXQiOjEwMH0=",
  "has_more": true,
  "total_rows": 500
}
```

Pass the `next_cursor` value back as `?cursor=…` to fetch the next page.

## Connection Tracking

Queries are linked to connections. View connection details:

```bash
curl -H "Authorization: Bearer $TOKEN" \
  http://localhost:4200/api/v1/connections
```

Connection metadata includes:
- Source IP address
- Connecting user
- Target database
- Connection start, last-activity, and disconnect timestamps
- Aggregated query count and bytes transferred

## Use Cases

### Security Auditing

Track all database access for compliance:
- Who accessed what data
- When queries were executed
- What SQL was run

### Performance Analysis

Identify slow queries:
- Sort by `duration_ms`
- Find patterns in slow queries
- Analyze query frequency

### Debugging

Troubleshoot application issues:
- See exactly what queries your application runs
- Verify parameter values for prepared statements
- Check timing and sequencing across engines

### Data Access Review

Regular reviews of who accessed sensitive data:
- Filter by table or keyword in `sql_text`
- Time-boxed reports
- User activity summaries
