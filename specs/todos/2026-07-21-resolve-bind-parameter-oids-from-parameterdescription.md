# Resolve PostgreSQL bind-parameter OIDs from ParameterDescription

## Goal

Log extended-protocol bind parameters as readable values instead of
`(oid:0)<base64>` when the client leaves `Parse.ParameterOIDs` empty.

## Why

`pgx` (and most modern drivers) send `Parse` with an empty `ParameterOIDs`
list, letting the server infer the parameter types. `handleBind` in
`internal/proxy/postgresql/intercept.go` only knows the OIDs the *client*
declared, so `decodeBinaryParameter` falls through to its unknown-type branch
and stores `"(oid:0)AAAAKg=="` instead of `"42"`.

That means the query log — dbbat's whole point — shows opaque base64 for the
bind values of every parameterised query issued by a pgx-based client. Found
while writing the PostgreSQL integration suite
(`internal/proxy/postgresql/integration_test.go`,
`TestIntegration_ExtendedProtocol_Capture`), which has to assert the opaque
form today.

## Implementation

- PostgreSQL answers `Describe(statement)` with a `ParameterDescription` ('t')
  message carrying the *resolved* parameter OIDs. The proxy already relays
  upstream messages back to the client, so intercept `*pgproto3.ParameterDescription`
  on the upstream path and store its `ParameterOIDs` on the matching
  `preparedStatement` in `extendedState` (keyed by the statement name from the
  preceding `Describe`).
- `handleBind` then uses those resolved OIDs when the client-declared list is
  empty. Falling back to the client list keeps the current behaviour for
  drivers that do declare types.
- Key files: `internal/proxy/postgresql/intercept.go` (`handleBind`,
  `decodeBinaryParameter`, `getTypeOID`), `internal/proxy/postgresql/session.go`
  (upstream→client relay, where the Describe/ParameterDescription pairing has to
  be tracked).
- Tighten `TestIntegration_ExtendedProtocol_Capture` to assert `"42"` once this
  lands, and drop the note pointing at this todo.

No GitHub issue exists for this yet — one should be filed.
