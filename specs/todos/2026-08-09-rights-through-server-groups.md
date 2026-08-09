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
