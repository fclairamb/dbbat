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

## Implementation Plan

Ordered so each step leaves the tree building. The migration is step 1 because
everything else keys off the final column set.

### 1. Migration `20260806020000_grants_reference_definitions`

`grant_definitions` first (versioning), then `access_grants` (the FK + backfill),
then the column drops. Statements split with `--bun:split`.

1. `ALTER TABLE grant_definitions ADD COLUMN archived_at TIMESTAMPTZ` — NULL = live.
2. `ADD COLUMN lineage_uid UUID`; backfill `lineage_uid = uid`; `SET NOT NULL`; index it.
   The lineage is what ties every version of one definition together. It is needed
   because a slug alone cannot: the slug is only unique *among live rows* from now on,
   so a brand-new definition may legally reuse a slug whose archived rows belong to an
   unrelated lineage. Deactivation and hard-deletion act on a lineage, not a row.
3. Drop `grant_definitions_slug_uniq` (a UNIQUE constraint, so `DROP CONSTRAINT`),
   replace with a partial unique index `WHERE archived_at IS NULL`.
4. Rebuild `grant_definitions_active_name_uniq` as `WHERE is_active AND archived_at IS NULL`
   — otherwise the first edit of any definition violates it (old + new version share the
   name and both are `is_active`).
5. `ALTER TABLE access_grants ADD COLUMN grant_definition_id UUID REFERENCES
   grant_definitions(uid)` — nullable at first so the backfill can run.
6. **Backfill, three passes, most-trustworthy first:**
   1. *Provenance.* `grant_requests.resulting_grant_id` names the grant a definition
      materialized. Those grants get that exact definition — no guessing.
   2. *Shape match.* For each still-unlinked grant, find a definition with the same
      shape: `controls`, `max_query_counts`, `max_bytes_transferred`,
      `approval_patterns`, `approver_group_uids`. Arrays are compared **sorted**
      (`ARRAY(SELECT unnest(x) ORDER BY 1)`) so ordering is not a false mismatch;
      quotas compare with `IS NOT DISTINCT FROM` so NULL = NULL. Ties broken by
      `created_at, uid` so the pick is deterministic. `duration_seconds` is
      deliberately *not* matched: the window lives on the grant and is unaffected.
   3. *Synthesis.* Every remaining distinct shape gets one inactive definition,
      slug `legacy-grant-shape-<n>`, name `Legacy grant shape <n>`, `is_active = false`,
      `duration_seconds` = the median-free simple choice of that shape's first grant's
      window length (clamped to ≥ 1). Grants sharing a shape collapse onto one
      synthesized row. `created_by` = that shape's first grant's `granted_by`.
      Inactive on purpose: these authorise nothing new, and their existing grants keep
      working because auth only checks `is_active` — see §3.
      *(Note: `is_active = false` also means those legacy grants stop authorising new
      connections, which is the fail-closed direction; deliberately called out in the
      release note.)*
7. `ALTER COLUMN grant_definition_id SET NOT NULL`; index it.
8. Drop `controls`, `max_query_counts`, `max_bytes_transferred`, `approval_patterns`,
   `approver_group_uids` from `access_grants` (this also drops
   `idx_access_grants_controls`). **`priority` stays.**

Down migration reverses genuinely: re-add the five columns, repopulate each grant from
its definition, drop `grant_definition_id`, delete the synthesized
`legacy-grant-shape-%` definitions, restore the original slug constraint and name index,
drop `archived_at` / `lineage_uid`.

Tested by a dedicated migration test (same harness as
`grant_definition_slug_migration_test.go`): a database seeded pre-migration with
duplicate shapes that must collapse, a shape that already matches a real definition,
a request-materialized grant, and a shape nothing matches.

### 2. Model (`internal/store/models.go`)

- `AccessGrant`: drop the five shape fields, add `GrantDefinitionID uuid.UUID` and a
  computed `Definition *GrantDefinition` (`bun:"-"`, JSON `definition`). Keep
  `Priority`.
- The shape is read through accessors that delegate to the definition and **fail closed**
  when it is absent: `Controls()` returns every control (so `HasControl` is true for
  all), `MaxQueryCounts()`/`MaxBytesTransferred()` return a zero quota (everything
  denied), `ApprovalPatterns()`/`ApproverGroupUIDs()` return empty. A shapeless grant is
  therefore unusable rather than unrestricted.
- `GrantDefinition`: add `ArchivedAt *time.Time`, `LineageUID uuid.UUID`, and a computed
  `ActiveGrantCount int64`.
- No bun relations: the definition is attached by an explicit batched second query
  (`attachDefinitions`). The repo uses no bun relations anywhere today and the auth
  path is too important for ORM join-alias surprises.

### 3. Store

- `GetActiveGrant` restricts to grants whose definition `is_active`, then attaches the
  definition; a grant whose definition cannot be attached is reported as
  `ErrNoActiveGrant`. **Deactivation therefore fails closed; archival does not** —
  `archived_at` is never consulted at auth time.
- `GetGrantByUID` / `ListGrants` attach definitions unfiltered, so history stays readable.
- `BuildGrantFromDefinition` pins `GrantDefinitionID` and copies nothing but the window
  and priority. `CreateGrant` rejects a grant with no definition.
- `GetGrantDefinitionBySlug` resolves the **live** row (`archived_at IS NULL`);
  `GetGrantDefinition(uid)` still returns archived rows so a pinned version can be
  displayed. `ListGrantDefinitions` is live-only.
- `UpdateGrantDefinition` becomes versioning: in one transaction, lock the row, archive
  it (`archived_at = now()`), insert the successor with the same `lineage_uid`, and
  return the new row. A no-op edit (nothing actually changed) skips versioning.
- `DeactivateGrantDefinition` / a new `ReactivateGrantDefinition` flip `is_active`
  across the whole **lineage** — an operator deactivates a definition, not one of its
  versions, and grants pinned to older versions must stop authorising too.
- New `DeleteGrantDefinition` hard-deletes a lineage but refuses with
  `ErrGrantDefinitionInUse{Grants, Requests}` when anything references it.
- `CountActiveGrantsForLineage` feeds the 409 message and the UI's confirm dialog.

### 4. API

- `POST /api/v1/grants` becomes **assign**: `{grant_definition_id (uid or slug),
  user_id, database_id, starts_at?}`. It resolves the live definition, requires it
  active, enforces the definition's **database** scope (a shape scoped to db A must not
  authorise db B) but not its group scope (group scope gates *self-service requests*;
  an admin assigning is the four-eyes authority for who), and materializes through the
  same `BuildGrantFromDefinition` the approval path uses. Window = definition duration.
- `GET /grant-definitions` gains `active_grant_count`; `DELETE /grant-definitions/{uid}`
  keeps deactivating and reports the affected grant count, with `?hard=true` for the
  hard delete that 409s when grants reference the lineage.
- `GrantSummary` (connection detail) reads controls from the definition and gains
  `grant_definition_id` + `grant_definition_slug`.
- `openapi.yml` + the route-parity test updated; `front/src/api/schema.ts` regenerated.

### 5. Approvals

`internal/api/approvals.go` (`mayApproveQuery`), `internal/proxy/shared/approval.go`
and `internal/store/approvals.go` all read patterns/approver groups through the new
accessors, i.e. from the definition the grant pins. No behaviour change beyond the
source of the data. `docs/approvals.md` updated — patterns are no longer "mirrored onto
the grant".

### 6. Auth cache

Nothing to do: `internal/cache` caches password/key verification only, never grant
resolution, and grants are re-read per connection. Per the resolved questions, a
definition edit must *not* invalidate anything anyway (it changes no live grant).
`RevocationRegistry` already covers revoke.

### 7. UI + tests

- `grants/index.tsx`: Create Grant dialog → **Assign Grant** (definition picker + user +
  database + optional start); shape columns read `grant.definition` and link to it.
- `grant-definitions/index.tsx`: edit dialog warns it creates a new version; deactivate
  confirm shows the affected grant count.
- Store/API/proxy tests build grants from definitions; proxy tests set the shape by
  constructing `&store.Grant{Definition: &store.GrantDefinition{…}}`.
