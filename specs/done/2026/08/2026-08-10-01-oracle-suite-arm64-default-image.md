# `make test-e2e-oracle` cannot run out of the box on Apple Silicon

**No GitHub issue filed yet — one should be.**

## Goal

Make `make test-e2e-oracle` runnable on an arm64 developer machine without the
developer having to know an environment variable, while keeping CI on the image
it actually runs.

## Why

The suite's default image is `gvenzl/oracle-xe:18.4.0-slim`, published for
`linux/amd64` only. Under Docker Desktop's emulation on an M-series Mac the
container starts and then dies during instance startup:

```
ORA-27300: OS system dependent operation:open failed with status: 0
ORA-27301: OS failure message: Error 0
ORA-27302: failure occurred at: sxecheck4
```

(seen as `ORA-00442` / exit 186 by the tests). Every containered test in the
package fails, so the suite gives no signal at all — which is how the two
failures in `2026-08-09-oracle-integration-suite-two-failures.md` went unnoticed
for a while. `docs/oracle.md` documents the `ORACLE_TEST_IMAGE=gvenzl/oracle-free:23-slim`
workaround and the Makefile now repeats it in a comment, but a developer only
finds it after a wasted 10-minute run.

The default cannot simply be flipped: `.github/workflows/integration.yml` runs
the Oracle job on `ubuntu-24.04` (amd64) with no `ORACLE_TEST_IMAGE`, so it
would silently switch CI onto the 23ai Free image — a coverage change that
should be a deliberate decision, not a side effect.

## Implementation

Two options, pick one:

1. **Auto-select by host architecture in the test helper.** In
   `oracleTestImage()` (`internal/proxy/oracle/integration_test.go`), when
   `ORACLE_TEST_IMAGE` is unset, return `gvenzl/oracle-free:23-slim` on
   `runtime.GOARCH == "arm64"` and keep the XE image otherwise. CI is unaffected
   (amd64), local runs just work, and the override still wins. Smallest change.
2. **Move the default to `gvenzl/oracle-free:23-slim` everywhere** and pin CI to
   the XE image explicitly if 18c coverage is still wanted. This is the cleaner
   end state — 23ai is the version the proxy work is validated against (see the
   client-compatibility tables in `docs/oracle.md`) — but it changes what CI
   exercises, so it needs a call on whether 18c XE coverage still earns its
   keep.

Either way, update the `test-e2e-oracle` comment in the `Makefile` and the
"Integration tests" section of `docs/oracle.md` to match.

Key files: `internal/proxy/oracle/integration_test.go` (`oracleTestImage`),
`Makefile` (`test-e2e-oracle`), `.github/workflows/integration.yml` (the
`oracle` job), `docs/oracle.md`.

## Resolved open questions

> Two options, pick one: 1. Auto-select by host architecture in the test
> helper. 2. Move the default to `gvenzl/oracle-free:23-slim` everywhere and
> pin CI to the XE image explicitly if 18c coverage is still wanted.

**Decision: option 2.** Make `gvenzl/oracle-free:23-slim` the default in
`oracleTestImage()` for every host and every environment, so a local run and a
CI run exercise the same image by default — 23ai is the version the proxy work
is validated against. `ORACLE_TEST_IMAGE` keeps overriding it.

> …it changes what CI exercises, so it needs a call on whether 18c XE coverage
> still earns its keep.

**Decision: 18c XE coverage is kept, but explicitly.** Do not simply let CI
drift onto 23ai. In `.github/workflows/integration.yml`, run the Oracle suite
against **both** images — a matrix over `ORACLE_TEST_IMAGE` with the 23ai Free
default and an explicit `gvenzl/oracle-xe:18.4.0-slim` entry (a second job
pinned to the XE image is equally acceptable if that reads better in the
workflow's existing shape). Both entries stay on `ubuntu-24.04` (amd64) and
keep the existing `-timeout 40m`. The XE entry is what preserves 18c coverage
as a deliberate choice rather than an inherited default.

Update the `test-e2e-oracle` comment in the `Makefile` and the "Integration
tests" section of `docs/oracle.md` to state the new default, the override, and
that CI additionally pins an 18c XE run.
