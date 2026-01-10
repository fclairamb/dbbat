---
sidebar_position: 2
---

# Query Logging

DBBat logs all queries executed through the proxy, providing complete visibility into database activity.

## What's Logged

For each query, DBBat records:

- **SQL text**: The complete query as sent by the client
- **User**: Which DBBat user executed the query
- **Database**: Which target database the query ran against
- **Connection**: The connection ID (links to connection metadata)
- **Start time**: When the query started
- **Duration**: How long the query took (in milliseconds)
- **Rows affected**: Number of rows returned or modified
- **Result data**: Optionally, the actual result rows

## Viewing Queries

List recent queries:

```bash
curl -u admin:admin http://localhost:8080/api/queries
```

### Filtering

By user:

```bash
curl -u admin:admin "http://localhost:8080/api/queries?user_id=2"
```

By database:

```bash
curl -u admin:admin "http://localhost:8080/api/queries?database_id=1"
```

By time range:

```bash
curl -u admin:admin "http://localhost:8080/api/queries?from=2024-01-15T00:00:00Z&to=2024-01-16T00:00:00Z"
```

Combined filters:

```bash
curl -u admin:admin "http://localhost:8080/api/queries?user_id=2&database_id=1&from=2024-01-15T00:00:00Z"
```

## Query Details

Get full details including result rows:

```bash
curl -u admin:admin http://localhost:8080/api/queries/123
```

Response:

```json
{
  "id": 123,
  "connection_id": 45,
  "sql": "SELECT id, name FROM users WHERE active = true",
  "started_at": "2024-01-15T10:30:00Z",
  "duration_ms": 42,
  "rows_affected": 5,
  "user": {
    "id": 2,
    "username": "analyst"
  },
  "database": {
    "id": 1,
    "name": "production"
  },
  "rows": [
    {"id": 1, "name": "Alice"},
    {"id": 2, "name": "Bob"},
    {"id": 3, "name": "Charlie"},
    {"id": 4, "name": "Diana"},
    {"id": 5, "name": "Eve"}
  ]
}
```

## Connection Tracking

Queries are linked to connections. View connection details:

```bash
curl -u admin:admin http://localhost:8080/api/connections
```

Connection metadata includes:
- Source IP address
- Connected user
- Target database
- Connection start/end time

## Use Cases

### Security Auditing

Track all database access for compliance:
- Who accessed what data
- When queries were executed
- What SQL was run

### Performance Analysis

Identify slow queries:
- Sort by duration
- Find patterns in slow queries
- Analyze query frequency

### Debugging

Troubleshoot application issues:
- See exactly what queries your application runs
- Verify query parameters
- Check timing and sequencing

### Data Access Review

Regular reviews of who accessed sensitive data:
- Filter by specific tables or keywords
- Time-boxed reports
- User activity summaries
