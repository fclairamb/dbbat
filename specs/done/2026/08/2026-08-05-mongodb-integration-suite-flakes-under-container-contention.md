# MongoDB integration suite flakes when Docker is under load

## Goal

Make `go test -tags integration ./internal/proxy/mongodb/...` deterministic, so
a failure means a regression rather than a busy laptop.

## Why

Individual tests in the suite intermittently fail at the *first* proxied
connection with:

```
connection() error occurred during connection handshake: auth error:
unable to authenticate using mechanism "PLAIN":
(AuthenticationFailed) dbbat: upstream MongoDB connection failed
```

Observed on `TestIntegration_Compression` and `TestIntegration_ProxyAuth_Password`
(2026-08-05), always while other testcontainers suites were competing for
Docker. Each fails in ~3s — far below any timeout — and the log shows a *second*
session on the same fixture reaching "MongoDB session ready", so the proxy and
the credentials are fine. Re-running the same test alone passes 5/5.

The likely cause is the fixture treating "container port is listening" as
"mongod is ready to authenticate": the wait strategy is
`ForListeningPort` + `ForLog("Waiting for connections")`, and under load the
first upstream dial can land in the window before the root user from
`MONGO_INITDB_ROOT_USERNAME` exists. The MySQL suite shows the same shape from
the other side — `TestIntegration_MySQLContainer`, which does not involve dbbat
code at all, fails its readiness wait under the same conditions.

This costs real time: a flake in a suite that guards the proxy's hot path is
indistinguishable from a regression until someone re-runs it.

## Implementation

- `internal/proxy/mongodb/integration_test.go`, `runMongoContainerWith`: replace
  the log/port wait with an actual authenticated round trip — e.g.
  `wait.ForExec([]string{"mongosh", "--quiet", "--eval", "db.adminCommand({ping:1})", "-u", rootUser, "-p", rootPass})`,
  or a `wait.ForAll` that keeps the current strategies and adds the exec check.
  That is the only signal that means "the initdb root user exists".
- Check whether `internal/proxy/mysql/integration_test.go`'s
  `runMySQLContainer` needs the same treatment; its wait matches a log line
  whose timing is equally load-dependent.
- Verify by running the mongo suite concurrently with another integration suite
  (that is what reproduces it) rather than alone.

No GitHub issue filed yet — one should be opened.
