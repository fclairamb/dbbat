# MySQL API Protocol Support

> Parent spec: `2026-04-25-mysql-proxy.md`
> Can be implemented in parallel with Phase 1.

## Goal

Surface `mysql` as a valid `protocol` value in the REST API and OpenAPI spec, and update database create/update validation to:
- Accept `protocol = "mysql"`
- Require `port` in create requests (the SQL default `5432` is dropped in Phase 1's migration)
- Provide protocol-aware error messages suggesting the conventional default port

## Files modified

```
internal/api/openapi.yml                # protocol enum + examples
internal/api/databases.go               # validation + response shaping
internal/api/databases_test.go          # add MySQL test cases
```

## OpenAPI changes

In `openapi.yml`, three locations have `enum: [postgresql, oracle]` for the `protocol` field. Update each to:

```yaml
protocol:
  type: string
  enum: [postgresql, oracle, mysql]
```

Remove the `oracle_service_name` requirement note for MySQL. Add an example for a MySQL database:

```yaml
- name: prod-orders
  description: Orders database
  protocol: mysql
  host: orders.internal
  port: 3306
  database_name: orders
  username: app
  password: ********
  ssl_mode: prefer
```

## Validation changes (`internal/api/databases.go`)

### Create request

The current code checks `req.Protocol != store.ProtocolPostgreSQL && req.Protocol != store.ProtocolOracle`. Extend:

```go
switch req.Protocol {
case store.ProtocolPostgreSQL, store.ProtocolOracle, store.ProtocolMySQL:
    // OK
default:
    writeError(c, http.StatusBadRequest, ErrCodeValidationError,
        "protocol must be one of: postgresql, oracle, mysql")
    return
}
```

### Port required

Currently the SQL column has `default 5432`. After Phase 1's migration drops that default, the API layer must validate:

```go
if req.Port == 0 {
    suggested := defaultPortFor(req.Protocol)  // 5432/1521/3306
    writeError(c, http.StatusBadRequest, ErrCodeValidationError,
        fmt.Sprintf("port is required (suggested default for %s: %d)", req.Protocol, suggested))
    return
}
```

A small helper:

```go
func defaultPortFor(protocol string) int {
    switch protocol {
    case store.ProtocolPostgreSQL: return 5432
    case store.ProtocolOracle:     return 1521
    case store.ProtocolMySQL:      return 3306
    default:                       return 0
    }
}
```

### Oracle service name not required for MySQL

The current code requires `oracle_service_name` (or falls back to `database_name`) when `protocol == ProtocolOracle`. For MySQL, `oracle_service_name` should be empty in the request and stays nil in storage. No new branch needed — the `case store.ProtocolOracle:` block already handles Oracle-specific logic; MySQL takes no action there.

## Tests

In `databases_test.go`, mirror the Oracle test cases for MySQL:

- `TestCreateDatabase_MySQL` — create with all valid fields, verify response
- `TestCreateDatabase_MySQL_PortRequired` — missing port → 400 with suggestion
- `TestCreateDatabase_MySQL_NoOracleFields` — `oracle_service_name` ignored
- `TestUpdateDatabase_MySQL` — update protocol from postgresql to mysql works
- `TestListDatabases_IncludesMySQL` — list response includes MySQL databases

## Verification checklist

- [ ] `make lint` clean, `make test ./internal/api/...` passes
- [ ] `curl -X POST .../api/v1/databases -d '{"protocol":"mysql",...}'` succeeds
- [ ] `curl -X POST .../api/v1/databases -d '{"protocol":"mysql","port":0,...}'` returns helpful error
- [ ] Swagger UI at `/api/docs` shows `mysql` in the protocol enum
- [ ] Existing PG and Oracle tests still pass (no regression)

## Out of scope

- Frontend changes (separate UI spec)
- Backend proxy implementation (Phase 1)
