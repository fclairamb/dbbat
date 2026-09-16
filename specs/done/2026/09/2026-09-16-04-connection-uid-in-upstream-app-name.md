---
model: sonnet
effort: medium
---

# A DBA looking at pg_stat_activity cannot get from a dbbat session to the dbbat connection page

## Problem

Every proxied upstream session already identifies itself as
`dbbat/<version> @<dbbat user> for <client app>` (`shared.BuildUpstreamName`,
`internal/proxy/shared/appname.go`) on all five protocols: PostgreSQL
`application_name`, MySQL `program_name` connect attribute, Oracle
`V$SESSION.PROGRAM`, SQL Server `APP_NAME()`, MongoDB
`client.application.name`. Two gaps showed during the 2026-09-15 incident and
the survey for this spec:

1. The name carries the **user** but not the **connection**. A user with
   three open sessions is three identical rows in `pg_stat_activity`; there is
   no way to jump from the row that is hurting to the dbbat connection page
   (its queries, its grant, and soon its Terminate button). The incident was
   attributed by searching Slack for the grant's title.
2. **MongoDB drops the client's own application name.** The other four fold
   the client-declared name in; Mongo passes `""`
   (`internal/proxy/mongodb/upstream.go:223`), although the client's `hello`
   carries it.

## Proposal

### Format

```
dbbat/0.28.1 @florent c=3f9a1c7b2e4d for psql
```

`c=` is the **last 12 hex characters** of the connection UUID (the random
part of a UUIDv7; the first characters are a timestamp and collide across
sessions opened in the same millisecond). Twelve characters is 48 bits, enough
to be unique among every connection an instance will ever see.

Truncation order when the protocol's cap bites (PG 63 bytes, Oracle 48,
MySQL/MSSQL/Mongo 128): the client app name is cut first, as today; then the
version; the `@user` and `c=` fields are never cut. On Oracle (48) a long
username plus the tag leaves little room, which is acceptable: the tag is what
makes the row actionable.

`BuildUpstreamName(version, username, clientAppName string, maxLen int)`
becomes `BuildUpstreamName(version, username string, connUID uuid.UUID,
clientAppName string, maxLen int)`; the five call sites (`postgresql/upstream.go:138`,
`mysql/upstream.go:31`, `oracle/upstream_auth_client.go:126`,
`mssql/upstream.go:400`, `mongodb/upstream.go:223`) pass the session's
connection uid. The connection uid is generated in Go (UUIDv7) so it exists
before the upstream dial; where a protocol currently creates the connection
row only after the upstream is up, generate the uid first and pass it to
`CreateConnection`. The connectivity probe keeps its own name
(`probeAppName`, `internal/proxy/conncheck/probes.go:53`).

### Lookup

- `GET /api/v1/connections?uid_suffix=3f9a1c7b2e4d` filters on the suffix
  (`WHERE right(uid::text, 12) = $1` with a matching expression index; the
  column is a uuid, so compare on the text form). 404-free: an empty list when
  nothing matches.
- The connections page search box accepts the 12-character form and the full
  `c=...` token pasted from a `pg_stat_activity` row, and opens the detail
  page directly when exactly one row matches.

### MongoDB passthrough

Capture `client.application.name` from the client's `hello` /
`isMaster` (the handshake is already parsed for SCRAM), keep it on the
session the way PG keeps `clientApplicationName`
(`internal/proxy/postgresql/session.go:138`), and pass it instead of `""`.

### Documentation

`docs/postgresql.md`, `mysql.md`, `oracle.md`, `mssql.md`, `mongodb.md`: one
paragraph each showing the query a DBA runs to find the dbbat connection
(`SELECT pid, application_name, state, query FROM pg_stat_activity WHERE
application_name LIKE 'dbbat/%'`) and the URL to paste the suffix into.

### Tests

- `appname_test.go`: the new field, the truncation order, the cap on each
  protocol's limit, and that the suffix is the *last* 12 characters.
- Store: the suffix filter matches exactly one of two connections whose uids
  share a prefix.
- One integration assertion per protocol suite reading the name back from the
  upstream (`pg_stat_activity.application_name`,
  `performance_schema.session_connect_attrs`, `V$SESSION.PROGRAM`,
  `APP_NAME()`, `currentOp().appName`) and matching the session's uid suffix.
- Mongo: a client that declares `appName=mongosh` is seen upstream as
  `... for mongosh`.
