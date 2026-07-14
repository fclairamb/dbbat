---
model: sonnet
effort: low
---

# Replace the "PostgreSQL Proxy" subtitle — it's now a generic DB proxy

Originating issue: [#254](https://github.com/fclairamb/dbbat/issues/254)

## Problem
The UI still labels dbbat as a "PostgreSQL Proxy"
(`front/src/components/layout/AppSidebar.tsx:123`). dbbat now proxies
PostgreSQL, Oracle, and MySQL/MariaDB, so the subtitle is inaccurate.

## Proposal
Replace the "PostgreSQL Proxy" string with a protocol-agnostic catch phrase.

**Recommended wording.** Whoever sees the sidebar is already inside the app, so
the subtitle is brand/personality space, not a place to define the product
category. Lean into the voice the project already has ("Every query logged.
Every connection tracked."). Ranked recommendation:
1. **"Every query, tracked"** — condensed brand line; short, distinctive, fits a
   narrow sidebar. Preferred.
2. **"Every query. Every connection."** — the fuller rhythm, if the layout has
   horizontal room.
3. **"Database proxy, observed"** — keeps the category word with a bit of wit.
4. **"Database observability proxy"** — plain descriptor fallback if the team
   prefers a category label over a tagline.

Avoid "Multi-database proxy" (reads like a limitation/workaround, sounds like
generic infra) and note that "observability proxy" alone silently drops the
access-control + safety half of what dbbat does.

**Mechanical steps.**
- Update the subtitle at `front/src/components/layout/AppSidebar.tsx:123`.
- Grep for other occurrences of the "PostgreSQL Proxy" wording across the
  frontend (and website/docs copy) and update them consistently.
- Confirm the chosen phrase fits the sidebar layout at the current width (the
  longer options may need a check on narrow/collapsed states).

## Implementation Plan

Grep audit results (`grep -rn "PostgreSQL Proxy\|PostgreSQL Observability Proxy"`
across the repo, excluding `.claude/worktrees/` copies and build output):

1. `front/src/components/layout/AppSidebar.tsx:123` — the sidebar subtitle
   itself, `PostgreSQL Proxy`. This is the brand/personality tagline slot the
   spec is about.
2. `front/index.html:7` — `<title>DBBat - PostgreSQL Observability Proxy</title>`,
   the browser tab title. Same staleness, different slot (SEO/categorical, not
   tagline space).
3. `internal/api/openapi.yml:5` — `REST API for DBBat PostgreSQL Observability
   Proxy.`, the OpenAPI description shown in Swagger UI.
4. `internal/api/resources/index.html` — gitignored build artifact copied from
   `front/index.html` by `make build-front`; no manual edit needed, it
   regenerates from step 2.

Occurrences intentionally left alone: `docs/postgresql.md`,
`charts/dbbat/README.md`, `website/docs/**`, `internal/api/schema` comments,
etc. — these say "PostgreSQL proxy" to mean the *PostgreSQL protocol listener*
specifically (one of three: PostgreSQL/Oracle/MySQL), which is still accurate
and not the generic app-level tagline this spec targets.

**Chosen wording**, per the spec's ranked recommendations:
- Sidebar subtitle (tagline slot, narrow width, personality-appropriate):
  **"Every query, tracked"** — recommendation #1, condensed brand line
  consistent with the existing "Every query logged. Every connection
  tracked." voice from the root `CLAUDE.md`.
- Browser tab title and OpenAPI description (categorical/SEO slots, not
  tagline space): **"Database Observability Proxy"** — recommendation #4,
  the plain descriptor, matching the phrase already used as the project's
  canonical one-liner in the root `CLAUDE.md` ("DBBat - Database
  Observability Proxy").

**Steps:**
1. Update `front/src/components/layout/AppSidebar.tsx:123` to `Every query,
   tracked`.
2. Update `front/index.html:7` `<title>` to `DBBat - Database Observability
   Proxy`.
3. Update `internal/api/openapi.yml:5` description to `REST API for DBBat
   Database Observability Proxy.`
4. `gofmt`/build/lint per QA gate; `internal/api/resources/index.html`
   regenerates on the next `make build-front`, not edited by hand.
5. Search `front/e2e/*.spec.ts` for any assertion on the old "PostgreSQL
   Proxy" sidebar text and update it; if none exists, add a small assertion
   to the existing navigation/sidebar test coverage.
