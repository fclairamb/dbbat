# Grants list doesn't show the applied limits, so you can't tell if you're over them

## Problem

On the grants page (e.g. https://dbbat.tools.stonal.io/app/grants), the **Usage**
column only shows raw consumption — `9 queries` / `169.8 MB` — with no indication
of the limits that apply to the grant (`max_query_counts`, `max_bytes_transferred`).
As a result you cannot tell whether a grant is close to, at, or over its quota.
This is effectively a bug: limits are a core access-control feature but are
invisible once the grant exists.

Current state of the plumbing:

- The frontend *does* append the limit when present:
  `front/src/routes/_authenticated/grants/index.tsx:218-229` renders
  `{query_count} / {max_query_counts} queries` and
  `{bytes} / {max_bytes}` — but only when the fields are truthy.
- The API serializes the store model directly (`internal/api/grants.go:92-125`
  returns `store.Grant` as-is; `internal/store/models.go:287-288` has the JSON
  tags), and grant-definition limits are copied onto materialized grants in
  `BuildGrantFromDefinition` (`internal/store/grants.go:17-33`).

So on the production instance either (a) these grants genuinely have `NULL`
limits — in which case the UI silently shows nothing instead of saying
"unlimited", which is indistinguishable from "limits not displayed" — or
(b) limits exist but are lost somewhere on this path. To investigate as step 1.

## Proposal

1. **Investigate**: check on a grant that *does* have limits (create one via the
   API or from a grant definition with quotas) that the list endpoint returns
   `max_query_counts` / `max_bytes_transferred` and that the UI renders them.
2. **Always show the limit state** in the Usage column:
   - With a limit: `9 / 100 queries`, `169.8 MB / 1 GB`, plus a visual
     progress indicator (e.g. subtle bar or percentage).
   - Near/over the limit: highlight (warning color ≥ ~80%, destructive when
     over/exhausted) so an over-quota grant is visible at a glance.
   - Without a limit: an explicit `unlimited` marker instead of nothing.
3. Apply the same treatment anywhere grant usage appears (grant detail view if
   any, grant-definitions page `front/src/routes/_authenticated/grant-definitions/index.tsx`
   which shows the configured quotas).
4. E2E: extend `front/e2e/grants.spec.ts` to assert the limit rendering for a
   granted quota and the `unlimited` fallback (test mode seeds sample grants).

Open questions:
- Should an exhausted quota also change the grant **Status** badge (e.g.
  `exhausted`), given the proxy blocks queries past the quota? Nice-to-have,
  can be split out.

No GitHub issue filed yet — one should be created and linked here.

## Implementation Plan

### Step 1 — Backend investigation (result)
Confirmed the API already serializes the limits: `store.AccessGrant` carries
`MaxQueryCounts *int64` / `MaxBytesTransferred *int64` with JSON tags
`max_query_counts` / `max_bytes_transferred` (`internal/store/models.go:235-236`),
and `handleListGrants` returns the store models as-is
(`internal/api/grants.go:118-124`). `BuildGrantFromDefinition` /
`CreateGrant` both copy the limits (`internal/store/grants.go:30-31,50-51`).
**No backend serialization gap.** The only backend gap is that test-mode seed
data (`main.go:provisionTestData`) creates grants with NO quotas, so E2E has no
"with limit" grant to assert against — fix by seeding one quota grant.

### Step 2 — Shared usage/limit rendering (frontend)
Add a small presentational helper `UsageMeter`
(`front/src/components/shared/UsageMeter.tsx`):
- Props: `used`, `limit` (nullable), `format` (identity / bytes), `unit` label.
- No limit → explicit `unlimited` marker (muted).
- With limit → `used / limit unit` text + a thin progress bar; ratio ≥ 0.8 →
  warning color, ratio ≥ 1 (over/exhausted) → destructive color.
Include `formatBytes` (shared) so bytes format consistently.

### Step 3 — Grants list Usage column
Replace the ad-hoc `{count}{ / max}` rendering
(`grants/index.tsx:216-230`) with two `UsageMeter`s (queries, bytes). Always
shows limit state incl. `unlimited`.

### Step 4 — Grant-definitions quotas
`grant-definitions/index.tsx` Max Queries / Max Data columns: render the limit
via the same `unlimited` marker + formatting (definitions have no live usage, so
just the configured limit with the consistent unlimited marker).

### Step 5 — Test-mode seed quota grant
Add a third seeded grant in `provisionTestData` (admin user on `proxy_target`)
with `MaxQueryCounts` and `MaxBytesTransferred` set, so the grants list has both
a limited and an unlimited grant for E2E. No uniqueness constraint blocks a
second grant for a distinct user (`CreateGrant` has no dedup).

### Step 6 — E2E
Extend `front/e2e/grants.spec.ts`: assert the list shows a `/ N queries` limit +
`unlimited` fallback and a progress meter for the quota grant.

### QA
`gofmt` (no `make fmt` target), `make build-binary`, `make test`, `make lint`
for Go; `make build-front` + `cd front && bun run lint` for frontend.
