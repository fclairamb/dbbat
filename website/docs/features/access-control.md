---
sidebar_position: 1
---

# Access Control

DBBat provides fine-grained access control through **grants**. A grant gives a user permission to access a specific database for a limited time, under the rules of the **grant definition** it was issued from.

A grant carries no access rules of its own. Controls, quotas and approval gating live on the definition; the grant holds only who, where, when, and its revocation state. That split is what makes definitions trustworthy: the list of definitions *is* the list of access shapes in use, with no unauditable one-offs hiding among the grants.

## Assigning a Grant

Define the shape once:

```bash
curl -X POST http://localhost:4200/api/v1/grant-definitions \
  -H "Authorization: Bearer $DBBAT_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "Read-only 9h",
    "slug": "read-only-9h",
    "duration_seconds": 32400,
    "controls": ["read_only"],
    "max_query_counts": 1000,
    "max_bytes_transferred": 104857600
  }'
```

Then assign it — by slug or uid:

```bash
curl -X POST http://localhost:4200/api/v1/grants \
  -H "Authorization: Bearer $DBBAT_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "grant_definition_id": "read-only-9h",
    "user_id": "550e8400-e29b-41d4-a716-446655440000",
    "database_id": "660e8400-e29b-41d4-a716-446655440000",
    "starts_at": "2024-01-15T09:00:00Z"
  }'
```

Users can also request the same definition themselves — see [Grant requests](./grant-requests.md). Both paths produce the same thing.

## Grant Fields

| Field | Lives on | Description |
|-------|----------|-------------|
| `user_id` | grant | UID of the user |
| `database_id` | grant | The grant's **anchor** database — the one it was issued for. What an unbound grant covers, and what a bound one falls back to if its group is deleted. |
| `server_group_uid` | grant | The [server group](#server-groups) this grant is bound to, if any. A bound grant covers every server that group holds *right now*, and only those. `null` = anchor only. |
| `grant_definition_id` | grant | The definition *version* this grant was issued from |
| `starts_at` | grant | When the grant becomes active |
| `expires_at` | grant | When it expires — `starts_at` plus the definition's `duration_seconds` |
| `priority` | grant | Which grant wins when several are active at once. Derived from the definition's controls unless it pins one — see [Overlapping grants](#overlapping-grants). |
| `controls` | definition | Combination of `read_only`, `block_copy`, `block_ddl`. Empty = full write access. |
| `max_query_counts` | definition | Maximum number of queries allowed |
| `max_bytes_transferred` | definition | Maximum bytes transferred (response size) |
| `duration_seconds` | definition | How long a grant issued from it lasts |
| `user_group_uids` | definition | User groups whose members may request it. Empty = every user. |
| `server_group_uids` | definition | Server groups it may be issued against. Empty = every database. |

Definitions are immutably versioned, so editing one never changes a grant that is already live — the grant stays pinned to the version it was issued from. Deactivating a definition, on the other hand, **fails closed**: every grant issued from any of its versions stops authorising new connections.

The one thing that *does* change under a live grant is the membership of the [server group](#server-groups) it is bound to. That is deliberate, and it is the point of groups.

The grant model is the same across all engines (PostgreSQL, Oracle, MySQL/MariaDB, MongoDB, SQL Server).

## Controls

Controls are **independent** and **combinable**. A grant with `["read_only", "block_copy", "block_ddl"]` enforces all three. An empty array allows full write access — including DDL, COPY, and writes — within the grant's time window.

### `read_only`

Blocks every operation that mutates data, in **defense-in-depth**:

- **Layer 1 — SQL inspection** (all engines): regex blocks `INSERT`, `UPDATE`, `DELETE`, `MERGE`, `REPLACE`, `CREATE`, `ALTER`, `DROP`, `TRUNCATE`, `GRANT`, `REVOKE`, plus `COPY FROM` (PostgreSQL) and `LOAD DATA` / `SELECT … INTO OUTFILE` (MySQL).
- **Layer 2 — engine session flag**:
  - **PostgreSQL**: `SET SESSION default_transaction_read_only = on` at session start.
  - **MySQL/MariaDB**: regex inspection only — `SET SESSION TRANSACTION READ ONLY` only applies to the *next* transaction in MySQL and is trivially bypassable.
  - **Oracle**: regex inspection only.
- **Layer 3 — bypass prevention** (PostgreSQL): attempts to disable read-only mode are blocked (`SET default_transaction_read_only = off`, `RESET …`, `SET SESSION AUTHORIZATION`, `SET ROLE`).

`read_only` is defense in depth for **trusted users**, not a security boundary against malicious actors. For untrusted access, also limit privileges on the upstream database user (e.g. PostgreSQL `GRANT SELECT` only).

### `block_copy`

Blocks all bulk file-touching operations:

- **PostgreSQL**: `COPY … TO` and `COPY … FROM` (both directions).
- **MySQL/MariaDB**: `LOAD DATA INFILE`, `SELECT … INTO OUTFILE`, `SELECT … INTO DUMPFILE`. Note that `LOAD DATA LOCAL INFILE` is **always** refused (see the MySQL notes).

### `block_ddl`

Blocks schema changes: `CREATE`, `ALTER`, `DROP`, `TRUNCATE`.

Useful when you need write access (for support intervention, data fixes) but want to prevent accidental schema drift.

## Time Windows

Grants are only active within their time window:

- Before `starts_at`: connection refused
- Between `starts_at` and `expires_at`: access granted
- After `expires_at`: connection refused

This is useful for:
- Support engineers who need temporary access
- Contractors with limited engagement periods
- Scheduled maintenance windows

## Server groups

A **server group** is a named set of database servers — "the analytics
replicas", "all staging databases". Definitions scope on groups instead of
listing servers, and a grant binds to the group it was issued under, so growing
the fleet stops meaning "edit every relevant policy".

```bash
curl -X POST http://localhost:4200/api/v1/server-groups \
  -H "Authorization: Bearer $DBBAT_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "analytics-replicas",
    "description": "Read replicas the analysts self-serve against",
    "member_uids": ["660e8400-e29b-41d4-a716-446655440000"]
  }'
```

A definition then points at the group rather than at servers:

```json
{ "server_group_uids": ["<the group uid>"] }
```

An empty `server_group_uids` means every database, exactly as an empty scope
always did.

### Membership is live

:::warning Adding a server widens live grants immediately

Server groups are **not versioned**, and membership is **never snapshotted at
issuance**. The moment a server joins a group, every grant bound to that group
covers it — including grants issued weeks ago and sessions already running.
Removing a server narrows those grants the same way, immediately, with no
exceptions: a bound grant covers what its group holds *now*, so even the
database it was originally issued for stops being covered once it leaves the
group.

This is one of two deliberate exceptions to "a live grant's behaviour never
changes under it" — the other being the approver lists below, for the same
reason. It is what makes groups useful — a new replica inherits the fleet's
policy without touching a definition — and it is why membership is an
admin-only surface that reports how many live grants an edit moves before you
save it.
:::

### One budget for the whole group

Quotas and `priority` belong to the grant, and the grant now covers the whole
group. A single `max_query_counts` and a single `max_bytes_transferred` budget
is consumed across **every** database in the group, not one budget per
database. `priority` ranks group-bound grants against each other on the
databases where their groups overlap — see below.

Deleting a server group is the one case where the anchor comes back, and it
still narrows: grants bound to it unbind and fall back to the single database
they were issued for. Definitions scoped to it match no database at all (fail
closed) until an admin edits them.

## Approvers on servers and server groups

Who guards a database is a property of that **database**, not of the policy
shape used to reach it. Two prod databases with different on-call teams should
not force two clones of the same grant definition.

So a server — and, as a fallback, a server group — carries two lists of user
groups:

| Field | Decides |
|---|---|
| `access_approver_user_group_uids` | **Grant requests** targeting this server: `POST /grant-requests/:uid/approve` and `/deny` |
| `query_approver_user_group_uids` | **Approval holds** — a pattern-matched statement parked mid-flight against this server |

```bash
curl -X PUT http://localhost:4200/api/v1/databases/$DB_UID \
  -H "Authorization: Bearer $DBBAT_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "access_approver_user_group_uids": ["'$OPS_GROUP_UID'"],
    "query_approver_user_group_uids": ["'$LEAD_OPS_GROUP_UID'"]
  }'
```

The split matches the two decisions' actual shape. A grant request is an
asynchronous policy call: ops can take an hour over it. A hold blocks a live
wire-protocol connection and needs the fastest competent responder, which is
usually a smaller group. Different audiences, different latency.

### The fallback chain

For a **grant request**:

1. the target server's `access_approver_user_group_uids`, if non-empty;
2. otherwise the **union** of that field across every server group the server
   currently belongs to;
3. otherwise admins only.

For an **approval hold**, one step is prepended:

1. the grant definition's own `approver_user_group_uids`, if non-empty — an
   explicit policy choice, so it wins outright;
2. otherwise the server's `query_approver_user_group_uids`;
3. otherwise the **union** across the server's groups;
4. otherwise admins only.

Admins decide at every step. Levels do not union with each other: naming a group
on the server is how you *override* the group-level default for that one
database. Groups at the same level do union — several server groups holding the
same server contribute all their approvers, exactly as several approver groups
on one definition already do.

Configure none of it and you get today's behaviour unchanged: admin-only
decisions, everywhere.

### No hierarchy between the two kinds

A query approver gains no say over grant requests, and an access approver gains
no say over a held statement. Neither implies the other; an organisation that
wants the same people doing both lists the same user group in both fields.

### Resolution is live

:::warning Editing an approver list changes who can act on work already waiting

Both lists are read **at decision time** and never snapshotted onto a grant.
Editing one — or moving a server between groups — immediately changes who may
approve, including for requests already filed and statements already parked.

This is the **second** deliberate exception to "a live grant's behaviour never
changes under it", after live server-group membership, and for the same reason:
approver lists are operational data. When a lead leaves, their replacement has to
be able to release the hold that is blocking a connection *right now*, not from
the next grant issuance onwards. The definition's `approver_user_group_uids`
keeps the opposite, versioned behaviour.
:::

### Self-approval is never delegated

Being named as an approver never lets you decide your own request or release
your own held statement. That holds on every path — the API, the web UI, and
both Slack transports.

Each pending item reports `approver_role` for the calling user (`admin`,
`definition_approver`, `server_approver`, or empty), which the UI renders as the
hat you are wearing — so an approver can see *why* they can act on one row and
not the next.

## Overlapping grants

Nothing stops a user from holding several active grants on the same database at
once — a read-only one they requested this morning and a read/write one an
admin assigned for an incident, say, or two grants whose server groups happen
to overlap on that database. Exactly one of them applies to a session: its
controls, quotas and approval patterns are the ones enforced.

The winner is the grant with the **highest `priority`**. Ties break on the
latest `expires_at`, then the newest `created_at`.

`priority` is auto-calculated from the definition's controls, so the more capable
grant wins by default:

| Grant shape | Auto priority |
|---|---|
| Writable, no controls (full read/write) | 100 |
| Writable with controls (`block_copy` / `block_ddl`) | 50 |
| `read_only` (with or without other controls) | 10 |

The gaps between tiers are deliberate: set an explicit `priority` on the
[grant definition](./grant-requests.md) — every grant issued from it is stamped
with that value — to slot those grants between tiers. `75` ranks a
restricted-write grant above its tier without letting it beat full write
access. The definition form pre-fills the field from the selected controls and
leaves it alone once you edit it, showing the computed value as an `auto: N`
hint.

### A session lives and dies with its grant

Selection happens **once, at connection time**. The session stays pinned to the
grant it was admitted under for its whole life — there is no mid-session
failover to another still-active grant, because that would silently change a
live session's controls underneath the client.

So when the pinned grant expires or is revoked, the connection is terminated
even if a lower-priority grant would still allow access. The client reconnects,
selection runs again, and the next-best still-active grant admits the new
session — under *its* controls.

## Quotas

### Query Quota

Limit the number of queries a grant can execute:

```json
{ "max_query_counts": 100 }
```

When exceeded, subsequent queries return an error.

### Data Transfer Quota

Limit the volume of data returned through the proxy:

```json
{ "max_bytes_transferred": 104857600 }
```

When exceeded, subsequent queries return an error. The byte counter accumulates response sizes from the upstream database.

Counters (`query_count`, `bytes_transferred`) are exposed on the grant object so admins can see usage in real time, and the web UI renders them as usage bars (warning at ≥80%, destructive at ≥100%, explicit `unlimited` marker when no limit is set).

### Mid-stream enforcement

Time and bandwidth limits are enforced **mid-stream**, not only between commands. A single `SELECT` streaming far more data than the grant allows is cut off partway through rather than being allowed to complete — so one runaway query cannot blow past a byte quota, and a grant expiring mid-transfer stops that transfer.

The bytes already transferred by a query aborted this way are still persisted, so quota accounting stays accurate.

## Revoking Grants

Manually revoke a grant before expiration:

```bash
curl -X DELETE http://localhost:4200/api/v1/grants/$GRANT_UID \
  -H "Authorization: Bearer $DBBAT_API_KEY"
```

The grant record is preserved for audit (with `revoked_at` and `revoked_by` populated).

Revocation takes effect immediately across all proxied protocols: further queries are blocked **and sessions already connected under that grant are disconnected**. You do not have to wait for the user to reconnect for a revocation to bite.

## Listing Grants

List all grants:

```bash
curl -H "Authorization: Bearer $DBBAT_API_KEY" http://localhost:4200/api/v1/grants
```

Filter by user, database, or active state:

```bash
curl -H "Authorization: Bearer $DBBAT_API_KEY" \
  "http://localhost:4200/api/v1/grants?user_id=$USER_UID&active_only=true"

curl -H "Authorization: Bearer $DBBAT_API_KEY" \
  "http://localhost:4200/api/v1/grants?database_id=$DB_UID"
```

Connectors only see their own grants; admins and viewers see all.

## Audit Trail

All grant operations are logged in the audit log:
- Grant creation (who granted, to whom, which database, what controls and quotas)
- Grant revocation (who revoked, when)

View the audit log:

```bash
curl -H "Authorization: Bearer $DBBAT_API_KEY" http://localhost:4200/api/v1/audit
```
