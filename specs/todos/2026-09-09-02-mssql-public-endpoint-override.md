---
model: sonnet
effort: low
---

# SQL Server has no per-protocol public endpoint override

## Goal

Give the SQL Server listener the same public-endpoint overrides the other four
protocols have: `mssql.host` / `mssql.port` in the `public` parameter group,
surfaced through `GET`/`PUT /api/v1/parameters/public` and the Settings page.

## Why

`BuildConnectionURL` gained a SQL Server branch
(`internal/api/connection_url.go`), and it needs a host and a port to
advertise. `store.ResolvedEndpoints` was given `MSSQLHost` / `MSSQLPort`, but
only half-resolved:

```go
MSSQLHost: pe.Host,                          // no per-protocol override
MSSQLPort: resolvePort(nil, cfg.ListenMSSQL) // no override either
```

PostgreSQL, Oracle, MySQL and MongoDB all read `pe.<Proto>Host` first and fall
back to `pe.Host`, and all four accept a port override. A deployment that puts
its SQL Server listener behind a different load balancer — which is exactly the
shape the other four overrides exist for — currently cannot say so, and the URL
the UI hands out is wrong for it.

This was left out of the connection-URL spec on purpose: it is a settings-surface
change (store parameters, API DTOs, OpenAPI, the Settings form) with no bearing
on the resolution ladder that spec was about.

## Implementation

1. `internal/store/global_parameters.go` — add `KeyPublicMSSQLHost` /
   `KeyPublicMSSQLPort`, the `PublicEndpoints.MSSQLHost` / `MSSQLPort` fields,
   their `GetPublicEndpoints` cases and `SetPublicEndpoints` pairs, then make
   `ResolvePublicEndpoints` use `resolve(pe.MSSQLHost, pe.Host)` and
   `resolvePort(pe.MSSQLPort, cfg.ListenMSSQL)`.
2. `internal/api/parameters.go` — add `mssql_host` / `mssql_port` to the three
   DTOs (resolved, raw, and the PUT request) exactly as `mongo_*` are done.
3. `internal/api/openapi.yml` — same three schemas, then
   `cd front && npm run generate-client`.
4. `front/src/routes/_authenticated/settings/index.tsx` — one more
   override block, copied from the MongoDB one.
5. Extend the `ResolvePublicEndpoints` unit test with the SQL Server fallback
   chain, and assert `BuildConnectionURL` honours an `mssql_host` override.
