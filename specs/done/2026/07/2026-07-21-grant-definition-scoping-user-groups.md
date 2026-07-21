# Scope grant definitions to user groups and databases

## Goal

Let an admin restrict a grant definition to (a) a set of user groups — a new
first-class concept — and (b) a set of databases, so that e.g. "all data-analysts
get a read-only auto-approve on these two warehouses" is one definition. A
definition with no scope keeps applying to every user and every database, so all
existing definitions behave exactly as today.

## Why

Grant definitions are currently global: any authenticated user can request any
active definition against any database. That makes `auto_approve` an
all-or-nothing lever — enabling it on a read-only definition hands instant access
to *every* database to *every* user, which is too broad for anything but demo
setups. Scoping is what turns auto-approve into a real policy tool
("data-analysts self-serve R/O on the warehouses; everything else goes through a
human").

## Design decisions (settled in discussion, 2026-07-21)

- **Scope lives on the definition itself.** No separate policy/binding object
  (k8s Role/RoleBinding style). Definitions number in the tens; duplicating one
  per scope ("Analyst R/O — staging" vs "Analyst R/O — prod") is cheaper and more
  legible in Slack notifications and the audit trail than a third grant concept.
- **Groups are a new entity, not an extension of `users.roles`.** Roles are
  functional (admin/viewer/connector); groups are organizational
  (data-analysts, SRE). Keep them apart.
- **Scope stored as UUID arrays on `grant_definitions`, not join tables — for
  fail-closed safety.** With a join table + `ON DELETE CASCADE`, deleting a group
  would silently empty a definition's scope, and empty scope means *everyone*:
  fail-open. With arrays, a dangling group UID matches no user, so the definition
  fails closed until an admin fixes it. Arrays also match house style
  (`users.roles`, `controls`) and keep updates a single-row write. If join tables
  are preferred later, they must use `ON DELETE RESTRICT`.
- **Explicit database UID lists, not server tags.** Tags scale better under
  server churn but are a second new concept and fail open (a mistagged server
  silently joins an auto-approve scope). Revisit if list maintenance hurts.
- **Approve-time scope re-check hard-blocks** (conflict error), so a pending
  request can't sneak through after an admin tightens scope. Admins can always
  create a direct grant — definitions bound self-service, never admins
  (existing invariant, see comment on `GrantDefinition` in
  `internal/store/models.go`).
- **Membership is admin-managed for v1.** Users arrive via Slack SSO, so syncing
  membership from Slack user groups is the natural v2; leave room for an
  `external_ref` column on `user_groups` but do not build the sync now.

## Implementation

### Migration

```sql
CREATE TABLE user_groups (
    uid         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name        text NOT NULL,
    description text NOT NULL DEFAULT '',
    created_by  uuid,
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX user_groups_name_uniq ON user_groups (lower(name));

CREATE TABLE user_group_members (
    group_uid uuid NOT NULL REFERENCES user_groups(uid) ON DELETE CASCADE,
    user_uid  uuid NOT NULL REFERENCES users(uid) ON DELETE CASCADE,
    PRIMARY KEY (group_uid, user_uid)
);

ALTER TABLE grant_definitions
    ADD COLUMN group_uids    uuid[] NOT NULL DEFAULT '{}',
    ADD COLUMN database_uids uuid[] NOT NULL DEFAULT '{}';
```

Membership is a join table (queried both directions: a user's groups for
eligibility, a group's members for the admin UI). Cascades on group/user
deletion are safe *because* definition scope does not live there.

### Store (`internal/store/`)

- `user_groups.go`: CRUD for groups + membership
  (`AddUserToGroup`/`RemoveUserFromGroup`/`ListGroupMembers`/`ListUserGroupUIDs`).
- `GrantDefinition` model + `CreateGrantDefinition`/`UpdateGrantDefinition`
  gain `GroupUIDs`/`DatabaseUIDs` (bun `,array` tags, nil → `{}` like
  `Controls`).
- Eligibility helper, e.g.
  `def.AppliesTo(userGroupUIDs []uuid.UUID, databaseUID uuid.UUID) bool`:
  empty list = unrestricted, otherwise require overlap / membership.

### Enforcement (three points, all in `internal/api/`)

1. **List** — `handleListGrantDefinitions` for non-admin callers filters to
   definitions whose `group_uids` is empty or overlaps the caller's groups
   (invisible, not greyed out). Admin management listing stays unfiltered.
2. **Create request** — `handleCreateGrantRequest`
   (`internal/api/grant_requests.go`): after loading the definition, reject
   out-of-scope user/database with 403. This automatically covers the
   auto-approve path — the security-critical one, since no human reviews it.
3. **Approve** — `ApproveGrantRequest` path re-checks scope; out-of-scope
   pending requests get a conflict error with a clear message.

The proxies and auth cache are untouched: grants are already materialized per
user+database; scoping only gates the request/approval funnel.

### API + OpenAPI (`internal/api/openapi.yml`)

- `GET/POST/PATCH/DELETE /api/v1/user-groups` (admin), member add/remove
  endpoints, groups included in user detail responses.
- Grant-definition request/response schemas gain `group_uids`,
  `database_uids`.
- Audit events for group create/delete and membership changes — membership is
  now access-relevant, same standing as grant decisions.

### Frontend (`front/`)

- Groups admin page (list, create, edit, members).
- User editor: group multi-select.
- Definition editor: two multi-selects, "Restrict to groups (empty = everyone)"
  and "Restrict to databases (empty = all databases)".
- Request form: definitions already filtered server-side; intersect the
  listable-servers dropdown with the selected definition's `database_uids`
  when non-empty.

No GitHub issue exists for this yet — one should be filed.

## Implementation Plan

1. **Migration** `20260721000000_user_groups` — `user_groups` (+ `external_ref`
   left out for v2, unique index on `lower(name)`), `user_group_members`
   (cascades both ways), and `grant_definitions.group_uids` /
   `.database_uids` as `uuid[] NOT NULL DEFAULT '{}'`. Working `.down.sql`.
2. **Store models** (`internal/store/models.go`) — `UserGroup`,
   `UserGroupMember`; `GrantDefinition.GroupUIDs` / `.DatabaseUIDs` with
   `bun:",array"`; `GrantDefinition.AppliesTo(groupUIDs, databaseUID)` +
   `AppliesToDatabase` / `AppliesToGroups` helpers — empty slice =
   unrestricted (backwards compatibility).
3. **Store CRUD** (`internal/store/user_groups.go`) — create/get/list/update/
   delete groups, add/remove members, `ListGroupMembers`, `ListUserGroupUIDs`,
   `ListGroupsForUser`. Duplicate-name sentinel like grant definitions.
4. **Store grant definitions** — persist the two arrays on create and update
   (nil → `{}`, same treatment as `Controls`).
5. **API user groups** (`internal/api/user_groups.go` + routes) — admin-only
   CRUD under `/api/v1/user-groups`, member add/remove, audit events
   (`user_group.created|updated|deleted`, `user_group.member_added|removed`).
   User detail response gains `groups`.
6. **API grant definitions** — create/update accept + validate `group_uids`
   and `database_uids` (existence check); non-admin list filters to
   definitions in scope for the caller's groups.
7. **API grant requests** — `handleCreateGrantRequest` rejects out-of-scope
   user/database with 403 (covers auto-approve); approve path re-checks scope
   and returns 409 conflict.
8. **OpenAPI** (`internal/api/openapi.yml`) — new paths, `UserGroup` schema,
   scope fields on grant-definition schemas.
9. **Frontend** — regenerate `front/src/api/schema.ts`; groups admin page
   (list/create/edit/members), group multi-select in the user editor, two
   scope multi-selects in the definition editor, request-form database list
   intersected with the definition's `database_uids`.
10. **Tests** — store tests for `AppliesTo` (incl. the unscoped =
    everyone case, groups-only, databases-only, dangling group UID fails
    closed, group deletion cascade), API tests for list filtering, request
    403, approve 409; e2e coverage for the groups page.
