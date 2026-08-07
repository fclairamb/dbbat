# Fix the failing "approve and enable auto-approve" grant-requests E2E test

## Goal

Make `front/e2e/grant-requests.spec.ts` → "clicking the combined action
approves the request and enables auto-approve on its definition" pass again,
or fix the product behaviour it is (correctly) catching.

## Why

Found while running the full Playwright suite as QA for the
`eslint-plugin-react-hooks 7.1` spec on `feat/todos-batch-2026-08-06`:
94 tests pass, this one fails, and it fails on all three retries.

It is **not** a regression from that spec. It was reproduced against a build
of the source *before* those changes (`git checkout <parent> -- front/src/...`,
rebuild, rerun) and failed identically, so it is pre-existing on this branch.

Nothing else in the batch touches grants, so it has been sitting red.

## Implementation

The test (`front/e2e/grant-requests.spec.ts:45`) does:

1. create a manual (non-auto-approve) grant definition,
2. submit a request against it, which should land `pending`,
3. as admin, click `[data-testid^="approve-and-enable-auto-approve-"]` on the
   pending row,
4. switch the filter to "All" and assert the row now reads `approved` — **this
   assertion is what times out (10s)**,
5. assert the row's definition badge reads `auto-approve`,
6. assert the toggle on `grant-definitions` is on.

Things to check, roughly in order of likelihood:

- Does the combined action actually succeed server-side? Watch the network
  response for the approve+enable call while the test runs
  (`bunx playwright test e2e/grant-requests.spec.ts --headed --debug`).
- Enabling auto-approve on a definition **archives the current row and inserts
  a successor sharing its `lineage_uid`** (immutable versioning, see the root
  `CLAUDE.md`). If the request row then renders against the archived
  definition — or the list query filters archived definitions out — the row may
  disappear from, or never refresh in, the "All" view. That is the most likely
  real bug.
- Is it just a refetch/invalidateQueries gap in the frontend, where the "All"
  tab shows a stale cached page? If so the fix is in the grant-requests route
  (`front/src/routes/_authenticated/grant-requests/index.tsx`).

Key files:
- `front/e2e/grant-requests.spec.ts`
- `front/src/routes/_authenticated/grant-requests/index.tsx`
- `internal/api/` grant-request approval handler
- `internal/store/` grant definition versioning

No GitHub issue exists for this; file one if it is picked up as its own piece
of work.

## How to reproduce

The standard `make test-e2e` cannot run while a `make dev` instance holds the
proxy ports (and its global setup runs `docker compose down`, which removes the
dev PostgreSQL container). To run it against a dev machine without disturbing
that, start an isolated server and let Playwright attach to it with `CI=true`:

```bash
docker exec dbbat-postgres psql -U postgres -c "CREATE DATABASE dbbat_e2e"
make build-front && make build-binary
DBB_RUN_MODE=test DBB_LISTEN_API=":8080" DBB_LISTEN_PG=":5533" \
  DBB_LISTEN_ORA="" DBB_LISTEN_MYSQL="" DBB_LISTEN_MONGO="" DBB_LISTEN_MSSQL="" \
  DBB_DSN="postgres://postgres:postgres@localhost:5001/dbbat_e2e?sslmode=disable" \
  DBB_KEY="MDEyMzQ1Njc4OTAxMjM0NTY3ODkwMTIzNDU2Nzg5MDE=" \
  DBB_RATE_LIMIT_ENABLED=false ./dbbat serve &
cd front && CI=true bunx playwright test e2e/grant-requests.spec.ts
```

`CI=true` makes `e2e/global-setup.ts` skip the build/start/`docker compose`
steps and only wait for readiness, and makes the teardown a no-op.
