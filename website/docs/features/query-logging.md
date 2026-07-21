---
sidebar_position: 3
---

# Query Logging

DBBat logs every query or command executed through the proxy — across all supported engines (PostgreSQL, Oracle, MySQL, MariaDB, MongoDB) — providing complete visibility into database activity.

## What's Logged

For each query, DBBat records:

- **SQL text**: the complete query as sent by the client (or the prepared statement text for binary protocols)
- **Parameters**: bound parameters for prepared/extended-query statements (PostgreSQL extended query, MySQL `COM_STMT_EXECUTE`)
- **User**: which DBBat user executed the query
- **Database**: which target server the query ran against
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

The global list resolves and returns **user**, **server** and **connection** columns alongside each query, so a single call is enough to see who ran what, where, and under which session — no follow-up lookups needed to make the list readable.

### Filtering

```bash
# By user
curl -H "Authorization: Bearer $TOKEN" \
  "http://localhost:4200/api/v1/queries?user_id=$USER_UID"

# By server
curl -H "Authorization: Bearer $TOKEN" \
  "http://localhost:4200/api/v1/queries?database_id=$SERVER_UID"

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

Every query names its owning connection through `connection_id`. In the web UI the query-detail page surfaces that link in its breadcrumb, so you can walk from a single statement back up to the session that issued it.

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

A single connection can also be fetched directly:

```bash
curl -H "Authorization: Bearer $TOKEN" \
  http://localhost:4200/api/v1/connections/$CONN_UID
```

The web UI has a matching connection detail page.

Connection metadata includes:
- Source IP address
- Connecting user
- Target server
- Connection start, last-activity, and disconnect timestamps
- Aggregated query count and bytes transferred

## Upstream Identity

DBBat does not only log queries on its own side — it also tags the upstream connection with the DBBat username, so the target database's own monitoring attributes activity to the real human instead of to the shared credentials DBBat connects with:

| Engine | Field carrying the DBBat username |
|--------|-----------------------------------|
| PostgreSQL | `application_name` |
| MySQL / MariaDB | `program_name` |
| Oracle | `AUTH_PROGRAM_NM` |

This makes DBBat's query log correlatable with what a DBA sees in `pg_stat_activity`, `v$session`, `SHOW PROCESSLIST`, or engine-level audit logs — useful when someone spots a heavy query upstream and needs to know who to talk to.

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
