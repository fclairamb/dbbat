# Fold the SQL Server test fixtures onto the shared grant helper

No GitHub issue yet — file one when picking this up.

## Goal

Have the SQL Server tests mint their grants through the same shared fixture
the other four protocol suites now use, instead of two more hand-rolled
copies of the same "definition first, then grant" dance.

## Why

`2026-08-10-02` hoisted `createGrantWithControls` out of the PostgreSQL,
MySQL, MongoDB and Oracle suites into `internal/proxy/testsupport`, so the
next schema change touches one definition rather than four. SQL Server was
left out of that move and still carries two open-coded variants:

- `internal/proxy/mssql/auth_test.go` — a local helper that also takes
  `approvalPatterns`, in a file with **no** `integration` build tag.
- `internal/proxy/mssql/integration_test.go` (~line 1129) — the read-only
  swap in the MCP end-to-end test, written inline.

Both are currently *correct* (they already name `GrantDefinitionID`), so this
is tidiness, not a live break — which is why it sits at the back of the queue.

## Implementation

The blocker is the build tag: `internal/proxy/testsupport/grants.go` is
`//go:build integration`, and `mssql/auth_test.go` is not tagged, so it cannot
import the helper as things stand. Pick one:

- Drop the tag from `grants.go` (the package is only ever imported by test
  files, so nothing ships in the binary — verify with
  `go build ./...` and that `make test` stays green), or
- Keep the tagged helper for the integration suites and give `auth_test.go`
  an untagged sibling in the same package.

Either way, widen the shared helper to carry `approvalPatterns` (the mssql
auth tests need it) without forcing the other four call sites to pass it —
a second `CreateGrantWithApprovalPatterns` entry point, or a variadic option,
rather than a fifth parameter on every existing call.

Then delete both local copies and re-verify with
`go vet -tags integration ./internal/proxy/...` plus `make test`.
