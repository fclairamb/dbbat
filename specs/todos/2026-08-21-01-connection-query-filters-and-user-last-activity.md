---
model: opus
effort: high
---

# Connections and queries can't be filtered by user, server group, grant or grant provenance, and the users list doesn't show when anyone last connected

## Problem

### 1. The observability lists are effectively unfilterable from the UI

`GET /connections` and `GET /queries` already accept `user_id` and
`database_id` ([observability.go:26](internal/api/observability.go:26),
[observability.go:187](internal/api/observability.go:187)) and the store
honours them ([connections.go:847](internal/store/connections.go:847),
[queries.go:687](internal/store/queries.go:687)) — but **no UI ever sends
them**. `/app/connections` fetches with `{before, limit}` only
([connections/index.tsx:38](front/src/routes/_authenticated/connections/index.tsx:38))
and `/app/queries` with `{connection_id, before, limit}`
([queries/index.tsx:30](front/src/routes/_authenticated/queries/index.tsx:30)).
Investigating "what did Alice do on prod-eu last Tuesday" means paging through
an undifferentiated 50-row-at-a-time list.

Two filters are missing from the API outright:

- **Server group.** Grants are scoped and bound by server group
  (`server_group_members`, [models.go:1071](internal/store/models.go:1071)), so
  "everything that happened on the *prod* group" is the natural operational
  question — and it is not answerable at any layer today.
- **Grant.** `connections.grant_uid` has been stamped since
  [20260806010000_connections_grant_uid.up.sql](internal/migrations/sql/20260806010000_connections_grant_uid.up.sql),
  the connections table shows a grant column
  ([connections/index.tsx:103](front/src/routes/_authenticated/connections/index.tsx:103)),
  and the detail page resolves a full `GrantSummary`
  ([observability.go:122](internal/api/observability.go:122)) — but there is no
  way to ask "show me every session that ran under *this* grant", which is the
  first question after an approval or a revocation.
- **Grant provenance ("manually approved").** dbbat already distinguishes a
  grant a human signed off on from one the policy handed out by itself:
  `ApproveGrantRequest` stores the approver in `decided_by`, and
  `AutoApproveGrantRequest` deliberately leaves it **NULL** — "nobody decided,
  the definition's policy did"
  ([grant_requests.go:238](internal/store/grant_requests.go:238),
  [grant_requests.go:244](internal/store/grant_requests.go:244)). That
  distinction is invisible everywhere downstream: it lives on `grant_requests`,
  and the link to the session is one-directional
  (`grant_requests.resulting_grant_id → access_grants.uid`,
  [models.go:1108](internal/store/models.go:1108)) — `AccessGrant`
  ([models.go:650](internal/store/models.go:650)) carries `GrantedBy` but no
  back-reference to the request that produced it. So "show me only the sessions
  that ran under access a human approved" — the audit-review question — cannot
  be asked at any layer.

There is also a correctness trap to avoid repeating: the existing `active`
toggle filters **client-side, inside the already-fetched page**
([connections/index.tsx:86](front/src/routes/_authenticated/connections/index.tsx:86)).
That silently under-reports whenever the matching rows aren't in the current
50. New filters must be server-side.

### 2. The users list can't answer "is this account still in use?"

`/app/users` shows roles, groups and last SSO role sync
([users/index.tsx](front/src/routes/_authenticated/users/index.tsx)) but nothing
about activity. Deciding whether an account is dormant — the routine
offboarding/cleanup question on a proxy whose whole job is access — currently
means cross-referencing the connections list by hand.

Neither timestamp exists today:

- **Last SQL connection** is derivable (`MAX(connections.connected_at)` per
  user) but no store method or endpoint computes it, and there is no index
  supporting it — only `idx_connections_user_id`
  ([initial_schema.up.sql:82](internal/migrations/sql/20260107000000_initial_schema.up.sql:82)).
- **Last UI login is recorded nowhere at all.** `handleLogin`
  ([auth.go:51](internal/api/auth.go:51)) and `handleOAuthCallback`
  ([oauth.go:257](internal/api/oauth.go:257)) write no audit entry and touch no
  column; `store.User` ([models.go:34](internal/store/models.go:34)) has
  `created_at`/`updated_at` and nothing else. This half needs new capture, not
  just new reporting.

## Proposal

### API + store filters

Extend `ConnectionFilter` ([models.go:495](internal/store/models.go:495)) and
`QueryFilter` ([models.go:629](internal/store/models.go:629)) with:

| Param | Applies to | Semantics |
|---|---|---|
| `user_id` | both | already implemented — UI wiring only |
| `database_id` | both | already implemented — UI wiring only; label it **Server** in the UI, the API name is historical |
| `server_group_uid` | both | resolve members via `ListServerGroupMemberUIDs` ([server_groups.go:145](internal/store/server_groups.go:145)) and constrain `database_id IN (…)` |
| `grant_uid` | both | `connections.grant_uid = ?`; queries already `JOIN connections c` ([queries.go:681](internal/store/queries.go:681)), so it's a `c.grant_uid` predicate |
| `grant_definition_uid` | both | the **policy**, matched across the definition's `lineage_uid` (not uid-equality) so an edit-archival doesn't split the history — see *Resolved open questions* |
| `grant_provenance` | both | multi-valued: `approved` / `auto` / `direct` — see below |

Decisions the implementation should hold to:

- **Membership is resolved live, at query time**, matching how grant scope
  works everywhere else (see the server-group note in `CLAUDE.md`). A filtered
  view is therefore a view of *current* membership, not of membership at
  session time. Say so in the handler comment.
- **An empty group returns zero rows**, never "unfiltered". Build the `IN ()`
  as an impossible predicate rather than skipping the clause.
- **Filters compose with AND.** `server_group_uid` + `database_id` is an
  intersection (usually a single server, or empty).
- **Connector scoping is unchanged and non-negotiable**: `handleListConnections`
  overwrites `filter.UserID` with the caller's own UID for non-admin/non-viewer
  ([observability.go:58](internal/api/observability.go:58)). That overwrite must
  stay *after* all new parsing so a connector cannot widen scope with a crafted
  `user_id`. Add a test that asserts it.
- **Unparseable UUIDs are currently ignored silently** (`if err == nil`), which
  turns a typo into "no filter" — i.e. more rows than asked for. Prefer a 400
  for the new params, and note in the spec-of-record if the existing ones are
  left as-is for compatibility.

**Grant filtering — one judgment call to confirm.** Grants are time-boxed
*instances*; a busy instance has many per user, so a dropdown of grant UIDs is
close to unusable and "sessions under the read-only-prod policy" is the
question people actually mean. Recommended: implement `grant_uid` (exact
instance, which is what a connection row links to and what the detail page
deep-links from) **and** `grant_definition_uid`, matched across the
definition's `lineage_uid` so an edit-archival doesn't split the history. If
only one ships first, ship `grant_uid` — the deep-link from a connection row is
the concrete use case.

### Grant provenance — the "manually approved" filter

A grant reaches a session by one of three routes, and they are distinguishable
today only by reading `grant_requests`:

| Value | Predicate | What it means |
|---|---|---|
| `approved` | a request row with `resulting_grant_id = c.grant_uid` **and** `decided_by IS NOT NULL` | a named human approved the request |
| `auto` | a request row with `resulting_grant_id = c.grant_uid` **and** `decided_by IS NULL` | the definition's `auto_approve` policy issued it, no human in the loop ([grant_requests.go:244](internal/store/grant_requests.go:244)) |
| `direct` | **no** request row references `c.grant_uid` | an admin issued it straight through `POST /grants` — a human act, but never an *approval* |

Implement it as a **multi-valued enum** (`grant_provenance=approved`, or
`approved,direct`), not a `manually_approved=true` boolean. The boolean reads
cleanly until you ask where `direct` belongs: an admin-issued grant is
unquestionably manual, and just as unquestionably was never approved, so either
answer makes the flag mean something other than what half its readers expect.
The enum lets both audiences say precisely what they mean, and "all manually
approved grants" as literally requested is `grant_provenance=approved`.

Notes for the implementation:

- **`direct` is a negative predicate** (`NOT EXISTS (SELECT 1 FROM
  grant_requests …)`), so it also swallows any grant whose request row was
  deleted, and any grant predating `resulting_grant_id`. That is acceptable —
  it must not be labelled `auto`, which would assert "no human was involved" on
  no evidence — but say so in the doc comment: `direct` is *"no approval on
  record"*, not *"provably admin-issued"*.
- **`connections.grant_uid` is nullable** (sessions predating the stamp, plus
  any session that ran without one). A NULL grant matches *no* provenance
  value; it is not `direct`. Cover it with a test.
- **Index required**: `grant_requests(resulting_grant_id)` does not exist —
  [20260510000000_grant_requests.up.sql](internal/migrations/sql/20260510000000_grant_requests.up.sql)
  indexes only `(user_id, status)` and `(status, requested_at)`. Add it in the
  same migration as the others; the reverse lookup is unusable without it.
- Prefer `EXISTS`/`NOT EXISTS` over `grant_uid IN (SELECT …)` — `IN` with a
  subquery over a nullable column is the classic NULL-semantics footgun, and
  `EXISTS` keeps the plan sane as `grant_requests` grows.
- The UI should render this as three checkboxes (or a small multi-select)
  labelled *Approved by a human* / *Auto-approved* / *Directly issued*, not as
  the raw enum values.

**Related, and nearly free — but not the same thing.** "Manually approved" has
a second plausible reading in this codebase: the **approval-hold** feature,
where a second human releases an individual held statement
(`queries.approval_status`, [models.go:543](internal/store/models.go:543),
`docs/approvals.md`). That is a per-*statement* property, not a grant property,
and no filter exposes it either — `/queries` cannot be narrowed to
`approved` / `denied` / `pending` today despite the column and its partial
index ([20260802000000_query_approvals.up.sql:22](internal/migrations/sql/20260802000000_query_approvals.up.sql:22))
already existing. Add an `approval_status` filter to `/queries` while in the
area; it is a one-line predicate on a column that is already there. Keep the
two clearly distinct in the UI — one filters *which access* the session ran
under, the other *which statements* a human released.

**Index work** (one migration): `connections(grant_uid)` has no index today.
Add it, plus `connections(user_id, connected_at DESC)` for the last-activity
query below. Verify with `EXPLAIN` that the filtered `ORDER BY uid DESC LIMIT n`
pagination still uses an index rather than sorting the table.

Update [openapi.yml](internal/api/openapi.yml) (`openapi_parity_test.go`
enforces it), then regenerate the typed client with
`pnpm generate-client` in `front/`.

### Users view: last UI login + last SQL connection

**Last SQL connection** — new store method (`DISTINCT ON (user_id) user_id,
connected_at ORDER BY user_id, connected_at DESC`) exposed as a bulk endpoint,
modelled directly on `GET /users/role-syncs`
([users.go:115](internal/api/users.go:115)) — one row per user computed in the
database, admin-or-viewer, absent means *never*. Do **not** derive it in the
frontend from a page of `/connections`: that produces "never" for anyone whose
last session fell off the page, which is exactly the failure the role-syncs
handler comment already warns about. Note in the doc comment that connection
rows can be deleted, so this reads as "last connection *still on record*".

**Last UI login** — add `users.last_login_at` (nullable timestamptz), stamped
on *interactive* login only: `handleLogin` ([auth.go:51](internal/api/auth.go:51)),
`handleOAuthCallback` ([oauth.go:257](internal/api/oauth.go:257)) and
`handleOAuthExchange` ([oauth.go:406](internal/api/oauth.go:406)). Explicitly
**not** on JWT validation/refresh, not on API-key auth, not on MCP — otherwise
the column stops meaning "last seen in the UI" and starts meaning "last HTTP
request from anything". Serve it on the user JSON.

Rationale for a column rather than a `user.login` audit event: a login
timestamp is operational state, not evidence, and the audit chain is already
carrying enough volume that `connection.opened`/`closed` had to be excluded
from the unfiltered `GET /audit`. The trade-off is real — a column is mutable
and unsealed, so this timestamp is *not* tamper-evident. If an evidential login
history is wanted later, that is its own todo (chained `user.login` events plus
a `ListLatestEventPerUser` read), not a reason to block this.

The write must never fail a login: log and continue on error.

### Frontend

- A filter bar on `/app/connections` and `/app/queries` with User / Server /
  Server group / Grant selectors, reusing the existing `Select` + `MultiSelect`
  primitives and the `Badge` + `X` clear-chip pattern already used for the
  connection filter ([queries/index.tsx:77](front/src/routes/_authenticated/queries/index.tsx:77)).
- **Filters live in URL search params** via `validateSearch`
  ([connections/index.tsx:27](front/src/routes/_authenticated/connections/index.tsx:27),
  [queries/index.tsx:19](front/src/routes/_authenticated/queries/index.tsx:19)),
  so a filtered view is shareable and the back button works — consistent with
  how `connection_id`, `before` and `size` are already handled.
- **Changing any filter resets `before` to `undefined`.** A cursor from the
  unfiltered list is meaningless against a filtered one and silently hides the
  head of the result.
- Server-group and grant option lists come from the existing `useServerGroups` /
  `useGrants` hooks. Note that `useGrants` is deliberately fetched unfiltered
  ([connections/index.tsx:44](front/src/routes/_authenticated/connections/index.tsx:44)) —
  it is already scoped server-side for connectors.
- `/app/users`: two new columns, `Last login` and `Last SQL connection`,
  rendered with `formatDistanceToNow` like the rest of the app, showing
  **Never** (muted) when null. Both are viewer/admin data — a connector sees
  only their own row, matching `handleGetUser`'s scoping
  ([users.go:138](internal/api/users.go:138)).

### Tests

- Store: each new filter in isolation, combined, empty-group, and a group whose
  membership changes between two calls (asserting the live-membership
  semantics are what's documented).
- Provenance: one session per route (human-approved, auto-approved,
  admin-issued, and a NULL `grant_uid`), asserting each value selects exactly
  its own and that NULL matches none of them. Build the fixtures through
  `ApproveGrantRequest` / `AutoApproveGrantRequest` rather than by inserting
  rows by hand — the whole filter rests on the `decided_by` convention those
  two functions establish, so the test should break if that convention moves.
- API: param parsing, 400 on malformed UUIDs, and the connector-cannot-widen
  case for both endpoints.
- `last_login_at`: stamped by password login and by the OAuth callback; **not**
  moved by a `/auth/me` call or an API-key request.
- Frontend: filter → URL round-trip, and that changing a filter clears the
  pagination cursor.

## Out of scope

The client-side `active` toggle on `/app/connections`
([connections/index.tsx:86](front/src/routes/_authenticated/connections/index.tsx:86))
under-reports for the same reason the new filters must be server-side. Moving
it server-side (a `disconnected_at IS NULL` predicate; the partial index
already exists, [20260803010000](internal/migrations/sql/20260803010000_connections_disconnected_at_index.up.sql))
is a natural companion but is a separate behaviour change — file it as its own
todo if not folded in.

## Resolved open questions

> **Grant filtering — one judgment call to confirm.** [...] Recommended:
> implement `grant_uid` (exact instance, which is what a connection row links to
> and what the detail page deep-links from) **and** `grant_definition_uid`,
> matched across the definition's `lineage_uid` so an edit-archival doesn't
> split the history. If only one ships first, ship `grant_uid` [...]

**Decision: ship both.** Implement `grant_uid` *and* `grant_definition_uid` in
this spec — the fallback ("if only one ships first") does not apply, there is no
follow-up todo, and a delivery that carries only `grant_uid` is incomplete.

Directives for the implementer:

- `grant_uid` filters on the exact grant instance: `connections.grant_uid = ?`
  (a `c.grant_uid` predicate on `/queries`, which already joins `connections`).
- `grant_definition_uid` filters on the **policy**, matched across the
  definition's `lineage_uid`, not on the definition row's own uid: resolve the
  passed uid to its `lineage_uid`, then match every grant whose definition
  shares that lineage. An edit archives a definition and inserts a successor
  (`CLAUDE.md`, "Definitions are immutably versioned"), so a uid-equality match
  would silently drop every session that ran under an earlier version — which is
  exactly the history an audit review is looking for. A uid naming an archived
  version resolves to the same lineage and returns the same rows.
- Both params follow the rules already set out for the other new filters:
  server-side, composing with AND, and a **400 on a malformed UUID** rather than
  the existing silent-ignore.
- Both get store-level tests. `grant_definition_uid` specifically needs a
  fixture where a definition has been **edited at least once** (archived
  predecessor + live successor, same `lineage_uid`) with sessions under each,
  asserting that filtering by *either* version's uid returns *both* sessions.
- UI: the grant selector on `/app/connections` and `/app/queries` is the
  **definition** (policy) list — that is the usable dropdown, since a busy
  instance has many time-boxed instances per user. Exact-instance `grant_uid`
  filtering is reached by deep-link from a connection row, not by a dropdown of
  grant UIDs; both must round-trip through URL search params like the rest.

## Implementation Plan

1. **Migration** `20260822000000_observability_filters_and_last_login`
   - `CREATE INDEX idx_grant_requests_resulting_grant ON grant_requests(resulting_grant_id) WHERE resulting_grant_id IS NOT NULL`
   - `CREATE INDEX idx_connections_grant_uid ON connections(grant_uid) WHERE grant_uid IS NOT NULL`
   - `CREATE INDEX idx_connections_user_connected_at ON connections(user_id, connected_at DESC)`
   - `ALTER TABLE users ADD COLUMN last_login_at timestamptz` (nullable) + column comment
   - `.down.sql` drops all four.

2. **Store filters** (`internal/store/models.go`, `connections.go`, `queries.go`)
   - `ConnectionFilter` / `QueryFilter` gain `ServerGroupUID`, `GrantUID`,
     `GrantDefinitionUID`, `GrantProvenance []GrantProvenance`;
     `QueryFilter` also gains `ApprovalStatus *string`.
   - New `GrantProvenance` type + `GrantProvenanceApproved/Auto/Direct` constants
     and `ParseGrantProvenance`.
   - Shared predicate helpers in a new `internal/store/observability_filters.go`
     so both list queries apply *identical* SQL:
     - `applyServerGroupFilter` — resolves membership live through
       `ListServerGroupMemberUIDs`, empty group ⇒ `1 = 0`.
     - `applyGrantDefinitionFilter` — `EXISTS` over `access_grants` joined to
       `grant_definitions`, matched on `lineage_uid = (SELECT lineage_uid …)`.
     - `applyGrantProvenanceFilter` — OR-group of `EXISTS` / `NOT EXISTS` over
       `grant_requests.resulting_grant_id`; `direct` carries an explicit
       `IS NOT NULL` guard so a NULL `grant_uid` matches no value.
   - Column expression differs per table (`grant_uid` vs `c.grant_uid`), so the
     helpers take the qualified column name.

3. **Last activity store reads** (`internal/store/users.go`)
   - `UserLastConnection` model + `ListLastConnectionPerUser` (`DISTINCT ON
     (c.user_id)`, joined to `users` for the username).
   - `TouchUserLastLogin(ctx, uid)` — a targeted `UPDATE`, never a full-model
     write.
   - `User.LastLoginAt *time.Time` on the model, serialised as `last_login_at`.

4. **API** (`internal/api/observability.go`, `users.go`, `auth.go`, `oauth.go`)
   - Parse the new params with a strict `parseUUIDQuery` helper returning 400 on
     a malformed UUID; existing `user_id`/`database_id`/`before` keep their
     silent-ignore behaviour for compatibility (noted in the spec-of-record).
   - Connector scoping overwrite stays the last thing before the store call.
   - `GET /users/last-connections`, admin-or-viewer, modelled on
     `GET /users/role-syncs`.
   - `s.stampLastLogin` called from `handleLogin`, `handleOAuthCallback` and
     `handleOAuthExchange`; failures are logged, never fatal.

5. **OpenAPI + typed client** — document the six new query params on
   `/connections` and the seven on `/queries`, the new `/users/last-connections`
   path, `UserLastConnection` schema and `User.last_login_at`; regenerate
   `front/src/api/schema.ts`.

6. **Frontend**
   - New `ObservabilityFilterBar` shared component (User / Server / Server group
     / Grant policy / Provenance, plus Approval status on `/queries`).
   - `validateSearch` on both routes carries every filter; any filter change
     navigates with `before: undefined`.
   - `/app/users`: `Last login` and `Last SQL connection` columns, relative
     times, muted **Never** when null.

7. **Tests** — store filter matrix incl. live-membership and the edited-definition
   lineage fixture, provenance fixtures built through `ApproveGrantRequest` /
   `AutoApproveGrantRequest`, API parsing/400/connector-scoping on both
   endpoints, `last_login_at` capture and non-capture, and a Playwright
   filter→URL round-trip + cursor reset.
