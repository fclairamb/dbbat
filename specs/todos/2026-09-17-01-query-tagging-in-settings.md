---
model: opus
effort: medium
---

# Statement tagging can only be turned on by redeploying, so nobody turns it on

## Problem

`DBB_QUERY_TAGGING` is read once, at startup, in `main.go:442`, and pushed into
each proxy with `SetQueryTagging(true)`. Turning tagging on or off therefore
means editing a deployment and restarting every replica.

That is the wrong shape for what the feature is for. Tagging answers a question
an operator asks in the middle of an incident: *which dbbat identity is the load
in Performance Insights coming from?* By the time a values file has been edited,
merged and rolled out, the session that caused the load is gone. The two real
deployments show it: `dbbat.tools.stonal.io` ran 0.29.0 for a day with tagging
off, because turning it on is a PR against `deploy.sh` rather than a checkbox.

The reverse direction is worse. Tagging changes the bytes the target database
receives, which is exactly why it ships off by default. If it breaks a client
(an ORM that parses statement text, a proxy downstream, a checksum), the operator
wants it off in seconds. Today that is a rollback.

It is also now an asymmetry inside one release. 0.29.0 shipped the per-statement
time limit with three layers, the middle one being a `limits.statement_timeout`
parameter edited from the Settings page, precisely so an operator could set it
without a deploy. Tagging landed in the same release with none of that, and the
two features have the same operator and the same urgency.

## Proposal

A `tagging` parameter group, store-backed, editable from Settings, winning over
the environment. Same shape and the same precedence rule as `public.*` over
`DBB_LISTEN_*` (`store.ResolvePublicEndpoints`) and `limits.statement_timeout`
over `DBB_STATEMENT_TIMEOUT`.

| Layer | Where | Semantics |
|---|---|---|
| Global, store | new group `tagging`, keys `enabled` and `oracle` in `global_parameters`, edited from the Settings page | wins over the env var when set |
| Global, env | `DBB_QUERY_TAGGING`, `DBB_QUERY_TAGGING_ORACLE` | the deployment default |

No per-grant-definition layer. The tag identifies a session, it does not grant
or restrict anything, so there is nothing for a definition to override and no
bypass to protect against. Keeping it out also keeps definitions from gaining a
field that only affects observability.

`store.Tagging` mirrors `store.Limits`: raw strings, not a `bool` and an enum,
so "unset" and "explicitly false" stay distinguishable. That distinction is the
whole point of the env fallback. `SetTagging` deletes a parameter rather than
storing a blank one, for the same reason `SetLimits` does.

```go
// Tagging holds the operator-configured statement-tagging settings. Raw
// parameter values: empty means the operator never set one, and the
// environment variable decides.
type Tagging struct {
    Enabled string // "true" / "false" / ""
    Oracle  string // "off" / "user" / ""
}
```

### Resolve per session, not by poking the servers

The tempting implementation is for `handleUpdateInstanceTagging` to call
`SetQueryTagging` on each proxy. **Do not.** `dbbat.tools.stonal.io` and the
demo both run one replica today, but the API handler runs on whichever replica
served the request, and `SetQueryTagging` mutates that replica's own
`atomic.Bool`. On two replicas, a settings change would take effect on one and
silently not on the other, and which one you got would depend on the load
balancer.

That is the same bug 0.29.0 just fixed for grant revocation, where revoking only
killed sessions on the replica that served the API call. Re-introducing its shape
in the next feature would be a poor trade for a saved store read.

So resolve at auth time, from the store, exactly where the statement timeout
already resolves:

```go
s.statementLimit = s.statementTimeouts.For(s.ctx, grant)   // exists today
```

Add a sibling resolver with the same caching, and have it decide instead of the
server-level flag:

```go
// NewQueryTaggingResolver resolves the store parameter over the env default,
// with the same cached read as NewStatementTimeoutResolver, so this is not one
// extra query per connection.
func NewQueryTaggingResolver(st *store.Store, cfg *config.Config) *QueryTaggingResolver

// Enabled reports whether this session should carry the tag.
func (r *QueryTaggingResolver) Enabled(ctx context.Context) bool

// OracleMode returns "off" or "user".
func (r *QueryTaggingResolver) OracleMode(ctx context.Context) string
```

The wire path needs no change at all. The tagger is already built once per
session at `internal/proxy/postgresql/auth.go:131`, guarded by a boolean, and
every component of it is already fixed for the session's lifetime. Only the
source of that boolean moves, from a server-level atomic set at startup to a
resolved per-session value.

Per-session granularity is also the right behaviour and should be documented as
deliberate: **a live session keeps the tagging decision it authenticated under.**
Sessions are what the tag identifies, and a statement tagged on some executions
and not others gets two digests in `pg_stat_statements`, which is the opposite of
what the feature is for. It matches how a live grant keeps `duration_seconds` and
the quotas it was issued under. An operator who needs tagging on a session that
is already running can terminate it now (`POST /connections/{uid}/terminate`,
0.29.0) and let it reconnect.

Keep `SetQueryTagging` and the `atomic.Bool`, now carrying the env default that
the resolver falls back to. That keeps the proxies usable without a store, which
the unit tests rely on.

### Oracle belongs on the page, with its price visible

`DBB_QUERY_TAGGING_ORACLE` is a separate setting for a real reason: `V$SQL` keys
on statement text, so each distinct tag holds its own shared-pool cursor, and an
operator who wanted attribution on PostgreSQL never consented to that. Measured
on 23ai: bounded at one cursor and ~48KB per dbbat *user*, versus 200 cursors and
9.6MB for a per-connection tag.

Expose it anyway, as its own field, never folded into the main toggle. A settings
page that quietly covers three protocols out of four sends the operator back to
the env var without telling them, and "which door did you use" is the asymmetry
0.28.0's blast-radius work existed to remove. Put the cursor cost in the field's
help text rather than leaving it in the docs.

One behaviour must change in the move. An invalid `DBB_QUERY_TAGGING_ORACLE` is a
**startup failure** today, which is correct for an env var: fail loudly at deploy
time. Through the API it must be a `400`, validated in the handler against
`config.QueryTaggingOracleOff` and `config.QueryTaggingOracleUser`. A settings
write that can crash every replica on the next restart is a worse failure than
the one it is modelled on. Validate on write, and have the resolver treat a
stored value it does not recognise as `off` with a `WARN`, the way
`ParseStatementTimeout` folds malformed into "no limit".

### API

Follow `PUT /instance/limits` (`internal/api/server.go:509`) exactly:

- `PUT /instance/tagging`, `s.requireAdmin()`, body `{"enabled": bool, "oracle": string}`
- `GET /instance` grows `tagging` (the raw stored values) and `resolved_tagging`
  (`enabled`, `enabled_source`, `oracle`, `oracle_source`), mirroring
  `resolved_limits` and its `statement_timeout_source`
- `resolveInstanceTagging(tagging store.Tagging, cfg *config.Config)` next to
  `resolveInstanceLimits` at `internal/api/parameters.go:170`
- OpenAPI schema, and the MCP surface if it exposes instance settings

The `*_source` fields matter more here than for limits. "Tagging is on" and
"tagging is on because someone set it in the UI, not because the deployment sets
it" are different facts during an incident, and the second is the one that tells
an operator whether a redeploy will silently revert it.

### UI

One section on the Settings page
(`front/src/routes/_authenticated/settings/index.tsx`, alongside the statement
timeout field at `:111`):

- a checkbox for PostgreSQL / MySQL / MongoDB tagging, showing the resolved value
  and its source, with the env default named when nothing is stored
- a separate Oracle select (`off` / `user`) with the cursor cost in its help text
- the exact tag shape rendered as a sample, so an operator can see what the
  target will receive before turning it on:
  `/*dbbat='0.29.0',user='alice',conn='a1b2c3d4e5f6',grant='readonly-prod'*/`
- a note that it applies to new sessions, with a link to the connections page

### Tests

- store: round-trip `GetTagging` / `SetTagging`, empty value deletes the
  parameter, unset falls back to env
- resolver: store wins over env, unset falls through, unrecognised Oracle value
  resolves to `off` and logs, cached read does not query per call
- API: `PUT /instance/tagging` requires admin, rejects a bad `oracle` with 400,
  `GET /instance` reports both raw and resolved with correct sources
- proxy: a session authenticating while the stored value is true carries the tag
  with the store off and env on, and the reverse; and an already-authenticated
  session keeps its decision after the parameter flips
- the existing tagging wire tests keep passing unchanged, which is the check that
  this moved the decision and not the mechanism

### Documentation

`CLAUDE.md` and `website/docs/configuration/index.md` both describe
`DBB_QUERY_TAGGING` as the switch. Both need the precedence sentence the limits
rows already carry: the store parameter wins, the env var is the deployment
default. `docs/postgresql.md`, `docs/mysql.md`, `docs/mongodb.md` and
`docs/oracle.md` each describe turning tagging on by env var and need the same
note.

### Out of scope, noted for later

- Per-grant-definition tagging. No use case yet, and see above.
- SQL Server, which has no tagging at all yet.
- Tagging a live session retroactively. Terminate and reconnect instead.
- An audit entry for the settings change itself. Worth having for every
  `/instance/*` write, not just this one, so it belongs in its own spec.

## Implementation Plan

1. `store.Tagging`, `GetTagging`, `SetTagging`, `GroupTagging`,
   `KeyTaggingEnabled`, `KeyTaggingOracle` next to the `limits` block at
   `internal/store/global_parameters.go:335`.
2. `shared.QueryTaggingResolver`, modelled on `NewStatementTimeoutResolver`,
   sharing its cached read.
3. Resolve at auth in all five proxies, next to `statementTimeouts.For`. Oracle
   takes the mode, the other three take the boolean. Leave `SetQueryTagging` as
   the env-default fallback.
4. `resolveInstanceTagging`, `PUT /instance/tagging`, the `GET /instance`
   additions, validation, OpenAPI.
5. Settings page section.
6. Tests per the list above.
7. Docs: the five files named above.
