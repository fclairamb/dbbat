# The only end-to-end OCI client test skips in CI

**No GitHub issue filed yet — one should be.**

## Goal

Give `TestIntegration_SqlplusLoginThroughSyntheticAuth` (and the two other
sqlplus-driven tests) an OCI client in CI, so the wide/OCI AUTH paths are
actually exercised there instead of skipping.

## Why

`internal/proxy/oracle/synthetic_auth_integration_test.go` now drives a real
sqlplus login through the wide synthetic AUTH builders, and it is the only
end-to-end proof that an OCI client can authenticate through dbbat at all —
the premise was measured: with the wide dispatch disabled, that login dies with
ORA-03113.

It opens with `exec.LookPath("sqlplus")` and skips when absent, which is the
case on every CI runner today. So does
`TestIntegration_BlockedStatementRefusesSQLPlus`, which is the only automated
cover for the unlearned fallback in `nextOERFrame`. Both are green in CI by
skipping.

That is the same shape of gap the forced-synthetic CI leg was created to close:
a suite that passes while proving nothing about the path in question. The
byte-level unit tests (`upstream_auth_client_wide_test.go`) carry real evidence
against a captured sqlplus login, but they cannot catch an upstream that stops
accepting the body — only a live login can.

The gap is widest exactly where the risk is: dbbat fronts multiple Oracle
versions and OCI builds, and the measurement in `docs/oracle.md` shows Oracle
23ai *tolerates* several deltas (thin data flags, plain length fields) that
another version may not.

## Implementation

- Install the Oracle Instant Client (basic + sqlplus packages) in the Oracle
  integration job of `.github/workflows/integration.yml`. Oracle publishes
  linux-x64 zips; the download needs no licence click-through for Instant
  Client, but it is a large artifact, so cache it.
- Alternatively, and probably cheaper: run sqlplus *from the Oracle container
  already started by the suite* (`gvenzl/oracle-free:23-slim` bundles the
  23.26 OCI client) via `docker exec`, pointed back at the proxy's host port.
  That also gets the **bundled** flavor — plain length fields, pair-count pad —
  which no test currently exercises live; only the Instant Client flavor is in
  `testdata`. Note the container would need a route back to the host
  (`host.docker.internal` or `--network host`).
- Once a client is present, make the skip loud: fail rather than skip when an
  env var like `ORACLE_TEST_REQUIRE_OCI_CLIENT=1` is set, and set it in CI. A
  silent skip is what let this gap exist.
- Consider running the OCI leg in both AUTH modes, as the thin legs already are.

Key files: `.github/workflows/integration.yml`,
`internal/proxy/oracle/synthetic_auth_integration_test.go`,
`internal/proxy/oracle/blocked_integration_test.go`, `docs/oracle.md`.
