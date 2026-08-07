# The SQL Server integration suite connects before the SA password works

## Goal

Make `internal/proxy/mssql/integration_test.go` wait for the container to
*accept a login* rather than for a log line, so the suite stops failing with
`mssql: login error: Login failed for user 'sa'.` on slow hosts.

## Why

`startUpstreamSQLServer` waits for the log line `SQL Server is now ready for
client connections`. On the emulated amd64 image (any Apple Silicon laptop, and
occasionally a loaded CI runner) that line is printed before the SA password
finishes being applied, so the very next connection is refused. Roughly half the
runs on an arm64 Mac fail this way, in `createE2ETable` or in the first
`sql.Open` of a test — `TestUpstreamSQLServerIsReachable`,
`TestProxyAccountsForResults`, `TestProxyHoldsAStatementForApproval`,
`TestProxyReleasesAHoldWhenTheClientCancels` and
`TestProxyEnforcesReadOnlyOnBothStatementPaths` have all been seen doing it.

They pass on a retry, so nothing is wrong with the proxy — but a suite that
needs a coin flip to run costs a full container startup every time it loses, and
it trains whoever runs it to ignore red.

## Implementation

- In `startUpstreamSQLServer` (`internal/proxy/mssql/integration_test.go`), keep
  the log wait and add a login probe after it: open `dsn(addr, "disable")` and
  poll `PingContext` until it succeeds, with a generous bound (the emulated
  image can take a further minute). Close the probe connection before returning.
- Doing it in the fixture rather than in each test covers `createE2ETable` and
  every `sql.Open` that follows, since they all start from the address this
  function returns.
- `testcontainers`' `wait.ForSQL` is the off-the-shelf form of the same idea and
  may be cleaner than a hand-rolled poll; either is fine as long as the wait is
  on a successful login.

No GitHub issue exists yet; one should be filed if this is picked up.

## Resolved open questions

**Should a GitHub issue be filed for this spec?**

Decision (2026-08-07, repository owner): **no.** Do not run `gh issue create`.
The spec file is the record. This carries forward the same decision the owner
made for the two preceding batches.
