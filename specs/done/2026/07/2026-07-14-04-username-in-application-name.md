---
model: sonnet
effort: medium
---

# Include the dbbat user name in the upstream application/program name

Originating issue: [#250](https://github.com/fclairamb/dbbat/issues/250)

## Problem
When dbbat connects to the upstream database it sets a generic application /
program name (e.g. PostgreSQL `buildApplicationName` at
`internal/proxy/postgresql/upstream.go:327`, MySQL `program_name: "dbbat-" +
version.Version` at `internal/proxy/mysql/upstream.go:78`, Oracle
`AUTH_PROGRAM_NM` in `internal/proxy/oracle/upstream_auth_client.go`). Because the
dbbat user is not encoded there, DBAs looking at the target's session views
(`pg_stat_activity.application_name`, `V$SESSION.PROGRAM`, MySQL process list)
cannot attribute a session to the human who initiated it.

## Proposal
Set the upstream application/program name to a canonical dbbat-branded string
that encodes the dbbat version and the authenticated dbbat user, and appends the
client's own application name when one was declared and intercepted.

**Format:**
```
dbbat/$version @$username
```
and, when the client declared an application name that dbbat was able to
intercept:
```
dbbat/$version @$username for $appName
```
where `$version` is `version.Version`, `$username` is the authenticated dbbat
user, and `$appName` is the client-supplied application/program name. When the
client supplies no app name (or it can't be intercepted), use the base
`dbbat/$version @$username` form.

Apply across all three protocols, respecting each protocol's length limits —
truncate safely so we never exceed the limit, preferring to truncate `$appName`
first and keeping the `dbbat/$version @$username` prefix intact:
- **PostgreSQL**: build the string in `buildApplicationName`
  (`internal/proxy/postgresql/upstream.go:327`); the client's `application_name`
  startup parameter is the intercepted `$appName`. It already truncates to
  `maxAppNameLen` (`upstream.go:322`). Update `upstream_test.go` accordingly.
- **MySQL**: replace the current `program_name: "dbbat-" + version.Version`
  (`internal/proxy/mysql/upstream.go:78`); the client's `program_name` connection
  attribute is the intercepted `$appName`.
- **Oracle**: set/augment `AUTH_PROGRAM_NM`
  (`internal/proxy/oracle/upstream_auth_client.go:733`); the client program name
  is the intercepted `$appName`.

## Implementation Plan

1. **Shared helper** — add `internal/proxy/shared/appname.go` with
   `BuildUpstreamName(dbbatVersion, username, clientAppName string, maxLen int) string`.
   Produces `dbbat/$version @$username`, or `dbbat/$version @$username for $appName`
   when `clientAppName` (trimmed) is non-empty. Truncates to `maxLen`, preferring to
   truncate `$appName` first (keeping the `dbbat/$version @$username` prefix intact);
   falls back to truncating the prefix itself only if it alone exceeds `maxLen`. Unit
   tests cover: no app name, with app name, truncation of app name only, and the
   prefix-truncation edge case.

2. **PostgreSQL** (`internal/proxy/postgresql/upstream.go`) — change
   `buildApplicationName(clientAppName string)` to
   `buildApplicationName(username, clientAppName string)`, delegating to
   `shared.BuildUpstreamName(version.Version, username, clientAppName, maxAppNameLen)`.
   Update the call site at `upstream.go:66` to pass `s.user.Username`. Update
   `upstream_test.go` expectations to the new `dbbat/$version @$username [for
   $appName]` format.

3. **MySQL** (`internal/proxy/mysql/upstream.go`) — the client's `program_name`
   connection attribute is available on `s.serverConn.Attributes()["program_name"]`
   once the handshake completes (`s.serverConn` is set in `Run` before
   `connectUpstream`/`applyUpstreamOptions` run). In `applyUpstreamOptions`, replace
   the hardcoded `"dbbat-" + version.Version` with
   `shared.BuildUpstreamName(version.Version, s.user.Username, s.serverConn.Attributes()["program_name"], maxProgramNameLen)`
   (new local constant, documented assumption since MySQL has no hard protocol
   limit — pick a generous conservative cap). Add a unit/integration-style test
   verifying the attribute value.

4. **Oracle** (`internal/proxy/oracle/*.go`) — the production path forwards the
   client's *actual* Phase 1 / Phase 2 AUTH packets upstream with only specific
   KV values swapped (username, session key, password, speedy key) via
   `replaceAuthKVValue`/`replaceAuthKVValueWide`; the synthetic builders
   (`buildClientAuthPhase1`/`buildClientAuthPhase2`, driven by `driverIdentity`)
   are only a fallback for when the client's raw packet wasn't captured. Both
   paths need `AUTH_PROGRAM_NM` to carry the canonical name:
   - Add `authKeyProgramNM = "AUTH_PROGRAM_NM"` const (`ttc_auth.go`).
   - Add `session.oracleDbbatUsername()`: parses the dbbat username fresh from
     `s.clientAuthPhase1Pkt` via the already-hardened `parseAuthPhase1`, falling
     back to `s.username`. Needed because for OCI/wide-encoding clients,
     `beginUpstreamAuth` (Phase 1) runs *before* `authenticateClient` resolves
     `s.user` (see `Run`'s Step 4b) — `s.user` cannot be relied on at that point.
   - Add `clientDeclaredProgramName(pkt *TNSPacket) string`: extracts the
     client's own `AUTH_PROGRAM_NM` value via `findKVByKeyBytes`/
     `findKVByKeyBytesWide` (picking by `payloadUsesWideKVEncoding`).
   - Add `session.buildUpstreamProgramName()` combining the two via
     `shared.BuildUpstreamName(version.Version, ..., maxProgramNameLen)`.
   - In `beginUpstreamAuth`/`finishUpstreamAuth`, set
     `identity.ProgramName = s.buildUpstreamProgramName()` after
     `defaultDriverIdentity()` — this fixes the synthetic-builder fallback path.
   - In `sendUpstreamAuthPhase1`/`sendUpstreamAuthPhase2`, after the existing
     username/secret rewrite produces `rewritten`, post-process with
     `replaceAuthKVValue(rewritten, authKeyProgramNM, identity.ProgramName, wide)`
     — this fixes the production relay/forward path. Reuses the existing
     generic, already-tested KV-value-replace helper instead of adding new
     low-level parsing; a no-op (returns input unchanged) if the key isn't
     found, so it's a safe addition.
   - `maxProgramNameLen` constant: Oracle's `V$SESSION.PROGRAM` is historically
     `VARCHAR2(48)`; use 48 with a comment noting the assumption.
   - Unit tests: `buildUpstreamProgramName`/`oracleDbbatUsername` combinations,
     and that `sendUpstreamAuthPhase1`/`sendUpstreamAuthPhase2` rewrite
     `AUTH_PROGRAM_NM` in the forwarded packet for both thin (compressed) and
     wide (OCI) encodings.

5. **QA** — `go build ./...`, `make lint`, `make test`. This spec is backend-only
   (no frontend surface), so no `front/` build/lint pass is needed.
