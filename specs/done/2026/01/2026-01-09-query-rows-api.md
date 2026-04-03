# Query Rows API

## Overview

Add a dedicated API endpoint to list query result rows separately from the queries API. This enables efficient retrieval of large query results with pagination support.

## Motivation

Currently, query result data is stored in the `query_rows` table but there's no dedicated API to retrieve this data. Users need a way to:
- Retrieve result rows for audit and replay purposes
- Handle large result sets efficiently without loading everything into memory
- Page through results incrementally

## API Design

### Endpoint

```
GET /api/v1/queries/:uid/rows
```

### Query Parameters

| Parameter | Type | Description |
|-----------|------|-------------|
| `cursor` | string | Opaque cursor for pagination (base64-encoded). Omit for first page. |
| `limit` | integer | Maximum number of rows to return (default: 100, max: 1000) |

### Response

```json
{
  "rows": [
    {
      "row_number": 1,
      "data": {...}
    },
    {
      "row_number": 2,
      "data": {...}
    }
  ],
  "next_cursor": "eyJvZmZzZXQiOjEwMH0=",
  "has_more": true,
  "total_rows": 5000
}
```

| Field | Type | Description |
|-------|------|-------------|
| `rows` | array | Array of row objects |
| `rows[].row_number` | integer | 1-based row number within the query result |
| `rows[].data` | object | The actual row data (column name → value) |
| `next_cursor` | string | Cursor for the next page (null if no more rows) |
| `has_more` | boolean | Whether more rows are available |
| `total_rows` | integer | Total number of rows for this query (if known) |

### Limits

The endpoint enforces two limits per request:

1. **Row count limit**: Maximum 1000 rows per response
2. **Data size limit**: Maximum 1MB of row data per response

The response will stop at whichever limit is reached first. If the data size limit is reached before the row count limit, fewer than the requested rows will be returned.

### Cursor Format

The cursor is a base64-encoded JSON object containing pagination state:

```json
{
  "offset": 100
}
```

This is opaque to the client - they should treat it as an opaque string and pass it back unchanged.

## Implementation

### Store Layer

Add to `internal/store/queries.go`:

```go
type QueryRowsResult struct {
    Rows       []QueryRow
    NextCursor string
    HasMore    bool
    TotalRows  int64
}

type QueryRowsCursor struct {
    Offset int64 `json:"offset"`
}

func (s *Store) GetQueryRows(ctx context.Context, queryUID string, cursor string, limit int) (*QueryRowsResult, error)
```

Implementation details:
- Decode cursor if provided (or start from offset 0)
- Query `query_rows` table for the given query UID
- Iterate through rows, accumulating data until either:
  - `limit` rows are collected, or
  - 1MB of serialized data is accumulated
- Encode next cursor if more rows exist
- Return result with pagination info

### API Layer

Add to `internal/api/observability.go`:

```go
func (s *Server) handleGetQueryRows(c *gin.Context)
```

Route registration:
```go
queries.GET("/:uid/rows", s.handleGetQueryRows)
```

### Authorization

- User must have access to view the query (same rules as `GET /api/v1/queries/:uid`)
- Admins can view all query rows
- Non-admins can only view rows from their own queries

## Error Responses

| Status | Error | Description |
|--------|-------|-------------|
| 400 | `invalid_cursor` | Cursor is malformed or invalid |
| 400 | `invalid_limit` | Limit exceeds maximum (1000) or is negative |
| 404 | `query_not_found` | Query with given UID does not exist |
| 403 | `access_denied` | User doesn't have permission to view this query |

## Example Usage

### First Page

```bash
curl -H "Authorization: Bearer $TOKEN" \
  "http://localhost:8080/api/v1/queries/abc123/rows?limit=100"
```

Response:
```json
{
  "rows": [...100 rows...],
  "next_cursor": "eyJvZmZzZXQiOjEwMH0=",
  "has_more": true,
  "total_rows": 5000
}
```

### Next Page

```bash
curl -H "Authorization: Bearer $TOKEN" \
  "http://localhost:8080/api/v1/queries/abc123/rows?limit=100&cursor=eyJvZmZzZXQiOjEwMH0="
```

## OpenAPI Specification

Add to `internal/api/openapi.yml`:

```yaml
/queries/{uid}/rows:
  get:
    summary: List query result rows
    description: Retrieve paginated result rows for a specific query
    tags:
      - Observability
    parameters:
      - name: uid
        in: path
        required: true
        schema:
          type: string
        description: Query UID
      - name: cursor
        in: query
        schema:
          type: string
        description: Pagination cursor from previous response
      - name: limit
        in: query
        schema:
          type: integer
          minimum: 1
          maximum: 1000
          default: 100
        description: Maximum number of rows to return
    responses:
      '200':
        description: Query rows retrieved successfully
        content:
          application/json:
            schema:
              type: object
              properties:
                rows:
                  type: array
                  items:
                    type: object
                    properties:
                      row_number:
                        type: integer
                      data:
                        type: object
                next_cursor:
                  type: string
                  nullable: true
                has_more:
                  type: boolean
                total_rows:
                  type: integer
      '400':
        description: Invalid cursor or limit
      '403':
        description: Access denied
      '404':
        description: Query not found
```

## Testing

### Unit Tests

- Test cursor encoding/decoding
- Test limit enforcement (row count)
- Test data size limit enforcement
- Test authorization rules

### Integration Tests

- Insert query with many rows, verify pagination works
- Verify data size limit stops iteration early
- Test cursor continuity across requests
- Test empty result set handling

## Future Considerations

- **Streaming**: For very large results, consider a streaming endpoint
- **Format options**: Support CSV/JSON Lines export formats
- **Compression**: Compress cursor or response for efficiency
- **Row filtering**: Allow filtering rows by column values
