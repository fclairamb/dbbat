---
model: sonnet
effort: medium
---

# Flaky: `TestApprovalHoldMatchesAPreparedStatement` (mssql) under load

## Goal

Make `internal/proxy/mssql`'s `TestApprovalHoldMatchesAPreparedStatement`
deterministic. It fails intermittently in a full `make test` run and passes
every time on its own — including `-count=5`.

No GitHub issue filed yet — one should be.

## Why

Observed on 2026-08-11 during an unrelated run of `make test`:

```
--- FAIL: TestApprovalHoldMatchesAPreparedStatement (15.08s)
    intercept_test.go:1259:
        Error Trace: internal/proxy/mssql/intercept_test.go:1313
                     internal/proxy/mssql/intercept_test.go:1259
        Error:       Should be true
```

`go test -count=5 -run TestApprovalHoldMatchesAPreparedStatement
./internal/proxy/mssql/` is green, so it is a timing/contention flake, not a
logic bug: the whole package runs its testcontainers stores in parallel and the
15s wall time on the failing run (vs ~3s in isolation) says the machine was
saturated. A flaky test in the default `make test` gate is worse than a slow
one — it trains everyone to re-run instead of read.

## Implementation

- Read `internal/proxy/mssql/intercept_test.go` around lines 1250–1320: the
  assertion at 1313 is reached from a helper at 1259, so first identify what
  "should be true" actually is (almost certainly a hold reaching
  `ApprovalPending` within a fixed wait).
- Replace any bare sleep-then-assert with a `require.Eventually` whose budget is
  generous (the approval registry is cross-goroutine, and the store round trip
  is a container away), or wait on the approval registry's own signal instead of
  polling the store.
- Confirm with `go test -count=1 ./internal/proxy/mssql/` run while the machine
  is loaded (e.g. concurrently with `make test`), not just in isolation.
