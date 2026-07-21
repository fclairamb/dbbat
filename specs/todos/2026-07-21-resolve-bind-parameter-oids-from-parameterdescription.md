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

## Implementation Plan

1. **State** (`internal/proxy/postgresql/session.go`)
   - Add `resolvedOIDs []uint32` to `preparedStatement` — the OIDs the *server*
     reported, distinct from the client-declared `typeOIDs`.
   - Add `pendingDescribes []string` to `extendedQueryState`: a FIFO of statement
     names for which the client issued `Describe('S', name)` and whose
     `ParameterDescription` has not come back yet.
   - Add a `sync.Mutex` guarding `preparedStatements` / `pendingDescribes`.
     Needed because `preparedStatements` is now written from the
     upstream→client goroutine as well as read/written from the
     client→upstream one.

2. **Client path** (`proxyClientToUpstream`)
   - Intercept `*pgproto3.Describe`; when `ObjectType == 'S'`, push the
     statement name onto `pendingDescribes`. Portal describes ('P') never
     produce a `ParameterDescription`, so they are ignored.

3. **Upstream path** (`proxyUpstreamToClient`)
   - Intercept `*pgproto3.ParameterDescription`; pop the head of
     `pendingDescribes` and store a **copy** of `msg.ParameterOIDs` as the
     matching statement's `resolvedOIDs`. The copy matters: pgproto3 reuses
     message buffers across `Receive` calls. `handleParse` gets the same
     defensive copy for `msg.ParameterOIDs`.

4. **Bind decoding** (`handleBind`)
   - Prefer the client-declared `typeOIDs`; when that list is empty, fall back
     to `resolvedOIDs`. Drivers that declare types keep today's behaviour;
     pgx-style clients now get decoded values.
   - The effective OID list is what goes into `store.QueryParameters.TypeOIDs`,
     so the stored record reflects the types actually used for decoding.

5. **Tests**
   - Unit: extend `intercept_test.go` — Describe/ParameterDescription/Bind
     sequence decodes an int4 `42`; client-declared OIDs still win; unmatched
     `ParameterDescription` is a no-op.
   - Integration: tighten `TestIntegration_ExtendedProtocol_Capture` to assert
     the INSERT logs `"42"` and the SELECT's single parameter is `"42"`, and
     drop the note pointing at this todo.
