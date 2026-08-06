---
model: opus
effort: xhigh
---

# Remove ad-hoc "Create Grant" — grants become instances of a Grant Definition, with no shape data of their own

## Problem

There are currently two ways a grant comes into existence, and they don't share
a model:

1. **Grant requests** — a user requests a `GrantDefinition`
   ([models.go:557](internal/store/models.go:557)); on approval the system
   *materializes* an `AccessGrant` by copying the definition's shape onto the
   grant row.
2. **Direct admin creation ("Create Grant")** — an admin invents a grant from
   scratch, typing controls/quotas/window ad hoc, bypassing definitions
   entirely. The bypass is even documented as intentional
   ([models.go:555](internal/store/models.go:555)). Entry points:
   `handleCreateGrant` ([grants.go:38](internal/api/grants.go:38)), route
   `POST /api/v1/grants` ([server.go:339](internal/api/server.go:339)), and the
   Create Grant dialog on
   [grants/index.tsx](front/src/routes/_authenticated/grants/index.tsx).

Because path 2 exists, `AccessGrant`
([models.go:483](internal/store/models.go:483)) must carry a full copy of the
behavioral shape: `Controls` (:489), `MaxQueryCounts`/`MaxBytesTransferred`
(:495–496), `Priority` (:505), `ApprovalPatterns` (:512), `ApproverGroupUIDs`
(:515). Consequences:

- Definitions can't be trusted as the policy source of truth — any grant may
  be an unauditable one-off.
- Every definition field must be mirrored onto grants at materialization time;
  each new definition feature (priority, approval patterns, approver groups…)
  has had to add a copy column + copy code, and a missed mirror is a silent
  policy hole.
- Editing a definition never affects grants already issued from it, which
  surprises operators tightening a policy.

The direction: **remove "Create Grant" and remove grants' own shape data
entirely.** A grant becomes an *instance* of a definition: `{definition, user,
database, starts_at/expires_at, granted_by, revocation state}` and nothing
else. Instance-lifecycle data (window, revocation, usage counters) stays on
the grant; behavioral shape (controls, quotas, priority, approval patterns,
approver groups) lives only on the definition.

## Proposal

1. **Schema migration** (`internal/migrations/sql/`):
   - Add `grant_definition_id` (FK → `grant_definitions`, not null) to
     `access_grants`.
   - Backfill: for each distinct legacy shape (controls, quotas, priority,
     approval patterns, approver groups) present on existing grants that
     doesn't match an existing definition, synthesize an inactive
     `legacy-…`-slugged definition and point the grants at it.
   - Drop the copied columns from `access_grants`: `controls`,
     `max_query_counts`, `max_bytes_transferred`, `priority`,
     `approval_patterns`, `approver_group_uids`.
2. **Store / proxy hot path**: `GetGrant`/auth-time grant listing joins (or
   eager-loads) the definition; `AccessGrant` methods (`HasControl`,
   `ShouldBlockDDL`, priority ranking in the multi-grant selection) delegate to
   the definition. The auth cache (`internal/cache/`) must be invalidated when
   a **definition** is edited, since edits now change live grants' behavior.
3. **API**: delete the ad-hoc `POST /api/v1/grants` body. Replace the admin
   flow with a "direct assign" that takes `{grant_definition_id, user_id,
   database_id, starts_at?}` — effectively an admin-initiated, instantly
   approved materialization (can reuse the grant-request approval path in
   `internal/api/grant_requests.go` to keep one code path). Update
   `internal/api/openapi.yml` and the route-parity test.
4. **UI**: remove the Create Grant dialog from
   [grants/index.tsx](front/src/routes/_authenticated/grants/index.tsx);
   replace with an "Assign grant" flow that picks a definition + user +
   database. Grants list/detail screens display shape fields read from the
   definition (and link to it, as the connections UI already links grants).
5. **Approvals / Slack**: hold-resolution approver checks and the approval
   registry read patterns/approver groups from the definition instead of the
   grant row (`internal/approval/`, `internal/api/slack_interactions.go`).
6. **Tests**: many tests build grants directly with inline controls
   (`grants_test.go`, `approvals_test.go`, `stream_approvals_test.go`, store
   tests…). Add a test helper that creates a definition and materializes a
   grant from it, and migrate call sites.

### Open questions

- **Live reference vs snapshot semantics.** Pure reference (proposed) means
  editing a definition retroactively changes every active grant issued from
  it — powerful for tightening policy, but loosening a definition silently
  widens live access. If that's unacceptable, definitions need to become
  immutable-versioned (edit = new version; existing grants pin the old one).
  Decide explicitly and document it.
- **Definition deletion/deactivation** with live grants attached: forbid,
  cascade-revoke, or fail closed at auth time? (Deactivation failing closed is
  most consistent with the `GroupUIDs` fail-closed philosophy at
  [models.go:585](internal/store/models.go:585).)
- **Per-grant priority pinning** goes away (priority comes from the
  definition, per [models.go:573](internal/store/models.go:573)); confirm the
  multi-grant selection spec (2026-08-06-02) is compatible.
- Breaking API change → the PR should be `feat!:` (major bump), and the
  release notes need a migration note for API consumers of `POST /grants`.

## Resolved open questions

Answered by the repository owner, 2026-08-06. These decisions are binding: implement them
as written rather than re-deriving them from the Proposal above, which predates them.

**Live reference vs snapshot semantics → immutable versioning, not live reference.**
Any edit to a `GrantDefinition` archives the current row (`archived_at = now()`) and
inserts a new row carrying the change. A slug is unique only among live rows: replace the
existing unique constraint on `slug` with a partial unique index `WHERE archived_at IS
NULL`, so one slug has exactly one live version and any number of archived ones.

Consequences to honour:

- Grants keep pointing at the exact definition row they were issued from, so an edit never
  retroactively changes a live grant's behaviour. Snapshot safety comes from the version
  pin, not from copying shape data onto the grant — the spec's core goal (grants carry no
  shape of their own) is unaffected.
- **This supersedes the Proposal's line** "The auth cache (`internal/cache/`) must be
  invalidated when a **definition** is edited, since edits now change live grants'
  behavior." Under versioning an edit does not change any live grant, so that invalidation
  is unnecessary. Invalidate on the events that genuinely change a grant's resolution —
  issuing, revoking, deactivating — not on definition edits.
- Addressing a definition by slug (added in #291) must resolve to the live row
  (`archived_at IS NULL`); addressing a specific historical version is by id.
- Listings and pickers show live definitions only. Archived rows stay readable so a grant
  issued from one can still display its shape.

**Definition deletion/deactivation with live grants → delegated; decision below.**

- **Hard deletion is forbidden** while any grant references the row. The new
  `grant_definition_id` FK is `not null`, so deleting would orphan grants; return a 409
  naming the blocking grant count.
- **Explicit deactivation fails closed at auth time**: a grant whose definition is
  deactivated stops authorising new connections, consistent with the fail-closed
  `GroupUIDs` handling at [models.go:585](internal/store/models.go:585). Show the affected
  grant count in the UI before the operator confirms.
- **Archival-by-supersession is not deactivation.** A row archived because it was edited
  keeps authorising the grants pinned to it; only an explicit deactivation fails closed.
  Keep the two states distinct in the model and in the UI.

**Per-grant priority pinning → delegated; keep it.**
`access_grants.priority` stays. It is instance-level ranking ("which of this user's
overlapping grants wins"), not behavioural policy, so it does not violate the "no shape
data on grants" rule. It also shipped in
[2026-08-06-02-multi-grant-selection-priority.md](../done/2026/08/2026-08-06-02-multi-grant-selection-priority.md)
(migration `20260806000000_grants_priority`) as a nullable admin override over the
definition-derived `AutoPriority` — see [models.go:499](internal/store/models.go:499) —
so dropping it would revert a feature that is already live. Proposal step 1 must therefore
drop `controls`, `max_query_counts`, `max_bytes_transferred`, `approval_patterns` and
`approver_group_uids` from `access_grants`, but **not** `priority`.
