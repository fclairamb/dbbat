# Oracle API Protocol Support

## Goal

Expose the `protocol` and `oracle_service_name` fields in the REST API so that administrators can create and manage Oracle database configurations alongside PostgreSQL ones.

## Prerequisites

- Store model already has `Protocol` (default `"postgresql"`) and `OracleServiceName` fields (`internal/store/models.go:104-105`)
- Store constants `ProtocolPostgreSQL` and `ProtocolOracle` exist (`internal/store/models.go:86-87`)
- `DatabaseUpdate` struct already has `Protocol *string` and `OracleServiceName *string` fields (`models.go:120-121`)

## Outcome

- API accepts `protocol` field when creating/updating databases (`"postgresql"` or `"oracle"`)
- Oracle databases use `oracle_service_name` instead of `database_name` for routing
- Default port adapts to protocol: 5432 for PostgreSQL, 1521 for Oracle
- SSL mode defaults and validation are protocol-aware (Oracle doesn't use PG SSL modes)
- Response includes `protocol` field so UI can display it
- OpenAPI spec updated with all new fields and protocol enum

---

## Changes

### 1. Request Structs — `internal/api/databases.go`

#### CreateDatabaseRequest (line 17)

Add two fields:

```go
type CreateDatabaseRequest struct {
    Name              string `json:"name" binding:"required"`
    Description       string `json:"description"`
    Host              string `json:"host" binding:"required"`
    Port              int    `json:"port"`
    DatabaseName      string `json:"database_name"`      // Remove binding:"required" — optional for Oracle
    Username          string `json:"username" binding:"required"`
    Password          string `json:"password" binding:"required"`
    SSLMode           string `json:"ssl_mode"`
    Protocol          string `json:"protocol"`            // NEW: "postgresql" (default) or "oracle"
    OracleServiceName string `json:"oracle_service_name"` // NEW: Oracle SERVICE_NAME
}
```

#### UpdateDatabaseRequest (line 29)

Add two fields:

```go
type UpdateDatabaseRequest struct {
    Description       *string `json:"description"`
    Host              *string `json:"host"`
    Port              *int    `json:"port"`
    DatabaseName      *string `json:"database_name"`
    Username          *string `json:"username"`
    Password          *string `json:"password"`
    SSLMode           *string `json:"ssl_mode"`
    Protocol          *string `json:"protocol"`            // NEW
    OracleServiceName *string `json:"oracle_service_name"` // NEW
}
```

### 2. Response Struct — `internal/api/databases.go`

#### DatabaseResponse (line 40)

Add two fields:

```go
type DatabaseResponse struct {
    UID               uuid.UUID  `json:"uid"`
    Name              string     `json:"name"`
    Description       string     `json:"description"`
    Host              string     `json:"host,omitempty"`
    Port              int        `json:"port,omitempty"`
    DatabaseName      string     `json:"database_name,omitempty"`
    Username          string     `json:"username,omitempty"`
    SSLMode           string     `json:"ssl_mode,omitempty"`
    Protocol          string     `json:"protocol,omitempty"`            // NEW
    OracleServiceName string     `json:"oracle_service_name,omitempty"` // NEW
    CreatedBy         *uuid.UUID `json:"created_by,omitempty"`
}
```

### 3. handleCreateDatabase — `internal/api/databases.go` (line 59)

Update the handler:

```go
// Validate and default protocol
if req.Protocol == "" {
    req.Protocol = store.ProtocolPostgreSQL
}
if req.Protocol != store.ProtocolPostgreSQL && req.Protocol != store.ProtocolOracle {
    errorResponse(c, http.StatusBadRequest, "protocol must be 'postgresql' or 'oracle'")
    return
}

// Protocol-aware defaults
if req.Port == 0 {
    switch req.Protocol {
    case store.ProtocolOracle:
        req.Port = 1521
    default:
        req.Port = 5432
    }
}

// Validate required fields per protocol
switch req.Protocol {
case store.ProtocolOracle:
    if req.OracleServiceName == "" && req.DatabaseName == "" {
        errorResponse(c, http.StatusBadRequest, "oracle_service_name or database_name is required for Oracle databases")
        return
    }
    // Use DatabaseName as fallback for service name
    if req.OracleServiceName == "" {
        req.OracleServiceName = req.DatabaseName
    }
case store.ProtocolPostgreSQL:
    if req.DatabaseName == "" {
        errorResponse(c, http.StatusBadRequest, "database_name is required for PostgreSQL databases")
        return
    }
    if req.SSLMode == "" {
        req.SSLMode = "prefer"
    }
}
```

Update the `store.Database` construction to include:
```go
Protocol:          req.Protocol,
OracleServiceName: oracleServiceNamePtr(req.OracleServiceName), // helper: returns *string or nil
```

### 4. handleUpdateDatabase — `internal/api/databases.go` (line ~290)

Pass the new fields to the store update:
```go
updates := store.DatabaseUpdate{
    // ... existing fields ...
    Protocol:          req.Protocol,
    OracleServiceName: req.OracleServiceName,
}
```

### 5. Response mapping

Update `toDatabaseResponse` (or wherever `DatabaseResponse` is built from `store.Database`) to include:
```go
Protocol:          db.Protocol,
OracleServiceName: derefString(db.OracleServiceName),
```

### 6. OpenAPI Spec — `internal/api/openapi.yml`

#### Add to Database schema (~line 1559):
```yaml
protocol:
  type: string
  enum: [postgresql, oracle]
  description: Database protocol
oracle_service_name:
  type: string
  description: Oracle SERVICE_NAME (Oracle only)
```

#### Add to CreateDatabaseRequest schema (~line 1615):
```yaml
protocol:
  type: string
  enum: [postgresql, oracle]
  default: postgresql
  description: Database protocol
oracle_service_name:
  type: string
  description: Oracle SERVICE_NAME (required for Oracle)
```

#### Add to UpdateDatabaseRequest schema (~line 1651):
```yaml
protocol:
  type: string
  enum: [postgresql, oracle]
  description: Database protocol
oracle_service_name:
  type: string
  description: Oracle SERVICE_NAME
```

#### Update required fields in CreateDatabaseRequest:
Remove `database_name` from `required` list (it's optional for Oracle). Add validation note in description.

---

## Validation Rules

| Field | PostgreSQL | Oracle |
|-------|-----------|--------|
| `protocol` | `"postgresql"` (default) | `"oracle"` |
| `database_name` | Required | Optional (falls back to service name) |
| `oracle_service_name` | Ignored | Required (or use database_name) |
| `port` default | 5432 | 1521 |
| `ssl_mode` default | `"prefer"` | empty (not applicable) |

## Tests

### Unit tests to add in `internal/api/server_test.go` or new file:

1. `TestCreateDatabase_PostgreSQL_Default` — no protocol field → defaults to postgresql, port 5432
2. `TestCreateDatabase_Oracle` — protocol=oracle → port 1521, service_name set
3. `TestCreateDatabase_Oracle_MissingServiceName` — 400 error
4. `TestCreateDatabase_InvalidProtocol` — protocol=mysql → 400 error
5. `TestCreateDatabase_Oracle_DatabaseNameFallback` — database_name used as service_name
6. `TestUpdateDatabase_ChangeProtocol` — update protocol field
7. `TestGetDatabase_IncludesProtocol` — response includes protocol field

## Files

| File | Change |
|------|--------|
| `internal/api/databases.go` | Add protocol/oracle_service_name to request/response structs, validation logic |
| `internal/api/openapi.yml` | Add protocol enum, oracle_service_name to all database schemas |
| `internal/api/databases_test.go` | New: protocol-specific tests (or add to existing test file) |

## Acceptance Criteria

1. `POST /api/v1/databases` with `"protocol": "oracle"` creates an Oracle database config
2. Default port is 1521 for Oracle, 5432 for PostgreSQL
3. `oracle_service_name` is required for Oracle (or falls back from `database_name`)
4. `GET /api/v1/databases` response includes `protocol` field
5. Invalid protocol returns 400
6. Existing PostgreSQL databases continue to work unchanged (backward compatible)
7. OpenAPI spec is updated and `bun run generate-client` produces correct TypeScript types
