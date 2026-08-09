---
model: opus
effort: high
---

# Scope rights through server groups, and make "groups" unambiguously "user groups"

**No GitHub issue filed yet — one should be.**

## Goal

Introduce **server groups** as the unit rights are scoped on, instead of
enumerating individual servers, and finish disambiguating the existing
"groups" concept as **user groups** everywhere a bare "group" still survives —
because once two kinds of groups exist, an unqualified `group_uids` is
ambiguous.

## Why

Today a grant definition scopes its target databases by listing individual
server rows: `GrantDefinition.DatabaseUIDs`
([models.go:690](internal/store/models.go:690), column `database_uids`).
Every grant then binds one user to one database
(`AccessGrant.DatabaseID`). Consequences:

- Adding a server that should be covered by an existing policy means editing
  every relevant definition — and definitions are immutably versioned, so each
  edit archives the row and inserts a successor
  ([grant_definitions.go:378](internal/store/grant_definitions.go:378)).
  Fleet growth is O(definitions) toil.
- There is no way to express "the analytics replicas" or "all staging
  databases" as a stable named set that policies reference.

Meanwhile the *user* side already has exactly this shape: `user_groups` /
`user_group_members` ([models.go:779](internal/store/models.go:779),
[user_groups.go](internal/store/user_groups.go)), route `/api/v1/user-groups`,
UI "User Groups". But the definition fields that reference them are still
bare: `group_uids` ([models.go:689](internal/store/models.go:689)) and
`approver_group_uids` ([models.go:711](internal/store/models.go:711)), plus
store method names like `ListGroupMembers` / `SetGroupMembers`. The moment
server groups land, "group" alone stops meaning anything.

## Implementation

### 1. Server groups entity

Mirror the user-groups shape:

- Migration in `internal/migrations/sql/`: `server_groups` (uid, name,
  description, created_by, created_at) + `server_group_members`
  (group_uid, server_uid join table, cascading deletes safe for the same
  reason as user-group membership).
- `internal/store/server_groups.go` modeled on
  [user_groups.go](internal/store/user_groups.go) (CRUD, membership,
  Set/Add/Remove members).
- API handlers + routes under `/api/v1/server-groups`, admin-gated like
  `/user-groups` ([server.go:321](internal/api/server.go:321)); OpenAPI
  schemas in `internal/api/openapi.yml`.
- Frontend page modeled on
  [front/src/routes/_authenticated/user-groups/index.tsx](front/src/routes/_authenticated/user-groups/index.tsx),
  sidebar entry next to "User Groups" in
  [AppSidebar.tsx](front/src/components/layout/AppSidebar.tsx).

### 2. Definitions scope by server group

Replace `database_uids` scoping with `server_group_uids` on
`GrantDefinition` (empty = any database, same semantics as today's empty
list). Touches the create/version paths and the change-detection equality in
[grant_definitions.go:415](internal/store/grant_definitions.go:415), the
request/approval validation in `internal/api/grant_definitions.go` and
`internal/api/grant_requests.go`, and the definition form in the frontend.

**Migration of existing definitions**: for each definition with a non-empty
`database_uids`, auto-create a server group holding exactly those servers
(named after the definition) and point the definition at it — no behavior
change, and admins can then merge/rename groups at their leisure.

### 3. Grant semantics — decide, then implement

The description says "handle rights through server groups", which leaves the
grant itself underspecified. Open questions to settle before coding:

- Does an `AccessGrant` still materialize per-database at issuance (request
  picks a database *within* the group's coverage — smallest change, auth path
  untouched), or does a grant bind to the group so that adding a server to
  the group extends live grants (bigger change: auth-time membership lookup
  in `internal/proxy/shared`, and quota semantics)?
- If group-bound: quotas (`max_query_counts`, `max_bytes_transferred`) and
  `priority` ranking are currently per user+database — do they span the
  group?
- Immutable-versioning philosophy says a live grant's behavior never changes
  under it; live group membership deliberately breaks that for scope. Is that
  acceptable (membership is operational data, like user-group membership
  already is for requestability), or should membership be snapshotted at
  issuance?

Recommendation: definitions scope by server group with **live** membership
(consistent with how `group_uids` user scoping already resolves live), grants
keep materializing per-database. Revisit group-bound grants as a follow-up if
per-database issuance proves too chatty.

### 4. Rename bare "groups" → "user groups"

- Column + JSON field `group_uids` → `user_group_uids`;
  `approver_group_uids` → `approver_user_group_uids` (or keep, arguing
  "approver" already implies users — decide once, apply everywhere).
- Store methods `ListGroupMembers`, `ListGroupMemberUIDs`,
  `SetGroupMembers`, etc. in [user_groups.go](internal/store/user_groups.go)
  gain a `UserGroup` prefix.
- OpenAPI (`openapi.yml` references at lines ~1047, ~1508, ~3900), the
  regenerated `front/src/api/schema.ts`, and any UI copy still saying just
  "groups".
- API compatibility: `/api/v1` JSON field renames are breaking for API
  consumers — accept the old field names on input for one release, emit the
  new ones, and note the rename in the changelog.

### Suggested sequencing

1 (entity) and 4 (rename) are independent and low-risk; land them first.
2 depends on 1. 3's decision gates 2's final shape — settle it at review
time, not mid-implementation.

## Resolved open questions

> Does an `AccessGrant` still materialize per-database at issuance (request
> picks a database *within* the group's coverage — smallest change, auth path
> untouched), or does a grant bind to the group so that adding a server to
> the group extends live grants (bigger change: auth-time membership lookup
> in `internal/proxy/shared`, and quota semantics)?

**Decision — group-bound grants.** The `Recommendation:` line in section 3
above is **superseded — do not follow it**. An `AccessGrant` binds to a server
group rather than to a single database, so adding a server to the group extends
every already-live grant bound to that group. Implement the auth-time
group-membership resolution on the proxy path (`internal/proxy/shared`): when a
connection authenticates against a database, the grant lookup must match any
live grant whose server group *currently* contains that database, not only
grants naming the database directly.

> If group-bound: quotas (`max_query_counts`, `max_bytes_transferred`) and
> `priority` ranking are currently per user+database — do they span the
> group?

**Decision — they span the group.** Quotas and `priority` are properties of the
grant, and the grant now covers the whole group: one `max_query_counts` budget
and one `max_bytes_transferred` budget are consumed across every database in the
group (not one budget per database), and `priority` ranks group-bound grants
against each other on the databases where their groups overlap. Adjust the quota
accounting and the `LimitGuard` bookkeeping accordingly, and cover the
overlapping-groups ranking with tests.

> Immutable-versioning philosophy says a live grant's behavior never changes
> under it; live group membership deliberately breaks that for scope. Is that
> acceptable (membership is operational data, like user-group membership
> already is for requestability), or should membership be snapshotted at
> issuance?

**Decision — live membership, and the break is accepted.** Do **not** snapshot
membership at issuance. Server-group membership is operational data, exactly as
user-group membership already is. Document the consequence explicitly wherever
the grant model is described (`CLAUDE.md` "Access Control", the grants docs):
adding a server to a group immediately widens every live grant bound to that
group. Surface the same warning in the admin UI at the point where server-group
membership is edited, so the blast radius is visible before the change is saved.

> Column + JSON field `group_uids` → `user_group_uids`;
> `approver_group_uids` → `approver_user_group_uids` (or keep, arguing
> "approver" already implies users — decide once, apply everywhere).

**Decision — rename both.** `group_uids` → `user_group_uids` and
`approver_group_uids` → `approver_user_group_uids`, applied everywhere: the DB
columns, the JSON fields, `internal/api/openapi.yml`, the regenerated
`front/src/api/schema.ts`, the store method names in
`internal/store/user_groups.go` (`ListGroupMembers` → `ListUserGroupMembers`,
etc.), and any UI copy still saying just "groups". Accept the **old** field
names on input for one release (emit only the new ones) and note the rename in
the changelog.

### Sequencing consequence of these decisions

Because grants are group-bound, section 3 is no longer a small tail on section
2 — the auth path and quota accounting both change. Land 1 (entity) and 4
(rename) first, then 2 (definitions scope by server group), then 3 (group-bound
grants: auth-time membership resolution, group-spanning quotas, priority across
overlapping groups) as its own step with its own tests.

## Implementation Plan

Four commits-worth of work, in the order the "Sequencing consequence" section
above mandates. Every step keeps `make test` green on its own.

### Step A — server groups entity + the `user_group` rename (spec §1 and §4)

Migration `20260809000000_server_groups_and_user_group_rename`:

- `server_groups` (uid, name, description, created_by, created_at) with a
  `lower(name)` unique index, and `server_group_members` (group_uid → server_groups
  ON DELETE CASCADE, server_uid → servers ON DELETE CASCADE, PK on both) —
  a literal mirror of `user_groups` / `user_group_members`.
- `ALTER TABLE grant_definitions RENAME COLUMN group_uids TO user_group_uids`
  and `approver_group_uids TO approver_user_group_uids`. Both columns exist only
  on `grant_definitions` (20260806020000 already dropped the `access_grants`
  copy), so the rename is two statements and a working `down`.

Go:

- `internal/store/server_groups.go` + `ServerGroup` / `ServerGroupMember` models,
  modeled line-for-line on `user_groups.go`, plus
  `ListServerGroupUIDsForServer` — the lookup the auth path needs.
- Field renames `GrantDefinition.GroupUIDs → UserGroupUIDs`,
  `ApproverGroupUIDs → ApproverUserGroupUIDs`, accessor
  `AccessGrant.ApproverGroupUIDs() → ApproverUserGroupUIDs()`,
  `AppliesToGroups → AppliesToUserGroups`.
- Store method renames in `user_groups.go`: `ListGroupMembers →
  ListUserGroupMembers`, `ListGroupMemberUIDs → ListUserGroupMemberUIDs`,
  `SetGroupMembers → SetUserGroupMembers`, `ListGroupsForUser →
  ListUserGroupsForUser`, `AddUserToGroup → AddUserToUserGroup`,
  `RemoveUserFromGroup → RemoveUserFromUserGroup`.

API:

- `/api/v1/server-groups` CRUD + `/members` sub-routes, admin-gated exactly like
  `/user-groups`; OpenAPI paths, `ServerGroup`/`CreateServerGroupRequest`
  schemas, a `Server Groups` tag.
- JSON renames `group_uids → user_group_uids`,
  `approver_group_uids → approver_user_group_uids` on the grant-definition
  create/update bodies and `group_uids → user_group_uids` on the update-user
  body. **One-release input compatibility**: each request struct keeps a
  `Legacy*` field bound to the old JSON name; a non-nil legacy value is used
  only when the new field is absent. Responses emit the new names only.

Frontend: `/server-groups` route + sidebar entry next to "User Groups",
`canManageServerGroups` in `lib/permissions.ts`, hooks in `api/queries.ts`,
regenerated `api/schema.ts`, and the renamed fields threaded through the
grant-definition form / grants / users pages.

### Step B — definitions scope by server group (spec §2)

- `GrantDefinition.DatabaseUIDs` → `ServerGroupUIDs` (`server_group_uids`),
  empty = every database (unchanged semantics).
- `AppliesToDatabase(dbUID)` → `AppliesToServerGroups(serverGroupUIDs)`, fed by
  `ListServerGroupUIDsForServer`. `AppliesTo` takes both group axes.
- Migration `20260809010000_grant_definitions_server_group_scope`: add
  `server_group_uids`, then a `DO` block that, for every definition row with a
  non-empty `database_uids` and an empty `server_group_uids`, reuses-or-creates
  one server group per *distinct set* of databases (named after the definition
  that first used it, uniquified), fills the membership, and points the
  definition at it. Idempotent by construction (the guard is
  `cardinality(server_group_uids) = 0`).
  `database_uids` is **kept, not dropped**: it is the pre-migration source of
  truth and dropping it in the same migration would make the mirroring
  irreversible. A follow-up todo covers retiring the column.
- API: `database_uids` on a create/update body is now a **400**, not a silent
  no-op — dropping a scope restriction on the floor would fail open.

### Step C — group-bound grants (spec §3 + Resolved open questions)

- `access_grants.server_group_uid uuid NULL REFERENCES server_groups(uid) ON
  DELETE SET NULL` (migration `20260809020000_access_grants_server_group`).
  A grant keeps its anchor `database_id` (the database it was issued for) and
  additionally covers every server currently in `server_group_uid`.
- Materialization (`BuildGrantFromDefinition`, both the admin-assign and the
  request-approval path) binds the grant to whichever of the definition's
  server groups currently contains the target database; an unscoped definition
  yields an anchor-only grant, exactly as today.
- `GetActiveGrant` matches `database_id = $db OR server_group_uid IN (SELECT
  group_uid FROM server_group_members WHERE server_uid = $db)`. All five
  protocols funnel through this one function, so that is the whole auth-path
  change. Ordering (`priority DESC, expires_at DESC, created_at DESC`) is
  untouched, which is what ranks overlapping groups against each other.
- Quotas span the group: `populateGrantCounters`' stamped-connection branch
  already spans it (it keys on `grant_uid`); the unstamped fallback widens from
  `database_id = $anchor` to "the anchor or any current member of the group".
  `LimitGuard` therefore inherits a group-wide `baseBytes` with no signature
  change; its doc comment says so.
- `ListGrants(DatabaseID)` widens the same way, so the UI lists the grants the
  proxy would actually pick for that database.

### Step D — docs + UI warning

Root `CLAUDE.md` "Access Control", `website/docs` grants page and the
server-group edit dialog all state the accepted consequence: server-group
membership is **live**, so adding a server to a group immediately widens every
live grant bound to it. The dialog shows it as a warning at the point of edit.

### Tests

- `internal/store/server_groups_test.go` — CRUD + membership, mirroring
  `user_groups_test.go`.
- `internal/store/grants_test.go` — a grant bound to a group authorizes a
  database added to that group *after* issuance; priority across two
  overlapping groups; quota counters spanning two databases of one group.
- `internal/api/grant_definitions_test.go` — legacy `group_uids` /
  `approver_group_uids` still accepted on input, responses emit the new names,
  `database_uids` is rejected.
- `internal/api/server_groups_test.go` — admin gating + CRUD.
- Migration tests for the definition-scope backfill.
