# Only one of two read-only refusals reaches the query history (SQL Server)

## Goal

Make `TestProxyEnforcesReadOnlyOnBothStatementPaths` in
`internal/proxy/mssql/integration_test.go` pass: when a read-only grant refuses
both a plain `SQLBatch` write and the same write wrapped in `sp_executesql`,
**both** refusals must land in the query history. Today the store holds one.

## Why

Found while working on the TLS 1.3 spec, and confirmed **pre-existing**: the
test fails identically at commit `012ad34`, before any of that work. It is not
a regression, but it is a real hole in the audit trail — a blocked write that
is not recorded is a blocked write nobody can review afterwards, which is the
whole point of the proxy.

The enforcement itself is fine. Both `ExecContext` calls do fail with a
"read-only" error, and the row count on the upstream is unchanged; the
assertions that check those pass. Only the history count is wrong:

```
Error:    Not equal: expected: 2, actual: 1
Messages: both refusals must be logged
```

## Implementation

- Reproduce: `go test -tags integration ./internal/proxy/mssql/ -timeout 40m
  -count=1 -run TestProxyEnforcesReadOnlyOnBothStatementPaths` (needs Docker;
  the SQL Server image is amd64). It reproduces on every run, so this is a
  determinism bug, not a flake.
- Find out **which** of the two paths is missing. The plain `SQLBatch` path and
  the `sp_executesql` RPC path record refusals from different places —
  `internal/proxy/mssql/intercept.go` and `rpc.go` — so the first question is
  whether one path never records, or whether both do and one is overwritten or
  deduplicated.
- Check the write side too: `shared.RowWriter` / the query-insert path may be
  asynchronous, in which case the test needs a wait rather than the code needing
  a fix. If so, prefer fixing the test with an `assert.Eventually` — but only
  after proving the row really does arrive, rather than assuming it.
- Compare with the equivalent MySQL/PostgreSQL enforcement tests, which do not
  show this.

Unrelated note for whoever runs this suite on an arm64 Mac: several other tests
in it (`TestUpstreamSQLServerIsReachable`, `TestProxyAccountsForResults`,
`TestProxyHoldsAStatementForApproval`,
`TestProxyReleasesAHoldWhenTheClientCancels`) intermittently fail with `Login
failed for user 'sa'` because the emulated SQL Server container logs "ready"
before the SA password is usable. Those pass on a retry and are environmental —
do not confuse them with this one, which does not.

No GitHub issue exists yet — one should be filed.
