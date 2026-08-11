---
model: sonnet
effort: low
---

# Run the Oracle integration suite a second time with the AUTH rewrite off

**No GitHub issue filed yet — one should be.**

## Goal

Make `.github/workflows/integration.yml` run the Oracle suite twice: once
normally, once with `DBBAT_ORACLE_FORCE_SYNTHETIC_AUTH=1`, so the synthetic
AUTH fallback cannot rot again.

## Why

`sendUpstreamAuthPhase1` / `sendUpstreamAuthPhase2` forward the client's own
packet with the username swapped whenever they can, which in practice is
always. The synthetic builders behind them are the fallback for a missing or
unrewritable client packet — and because nothing ever ran them, they shipped a
go-ora **v2** preamble (`03 76 00 01`) for months. Against a modern
client/23ai negotiation that is one byte short: the upstream answers two break
markers plus `ORA-03120`. Fixed 2026-08-10
(`specs/todos/2026-08-10-08-oracle-synthetic-phase1-preamble-drift.md`), which
also added the seam that makes the fallback runnable:

```bash
DBBAT_ORACLE_FORCE_SYNTHETIC_AUTH=1 go test -tags integration -timeout 40m \
  -count=1 ./internal/proxy/oracle/
```

The seam exists but nothing in CI uses it, so the next drift is invisible
again — exactly the state that let this one ship.

## Implementation

- Add a matrix dimension (or a second step) to the Oracle job in
  `.github/workflows/integration.yml` that re-runs `make test-e2e-oracle` with
  `DBBAT_ORACLE_FORCE_SYNTHETIC_AUTH=1` exported. Both legs must be green.
- The variable is read once by the `TestMain` in
  `internal/proxy/oracle/synthetic_auth_integration_test.go`; nothing in
  production writes the flag it sets.
- Consider whether the second leg is worth its container-startup cost on every
  run or belongs on a nightly schedule — the suite is dominated by Oracle
  startup, so this roughly doubles the job's wall clock.
- Note the known limitation while you are there: the synthetic builders emit
  the thin (compressed) KV encoding only, so the fallback cannot serve a
  wide/OCI client whose rewrite fails. Filing that separately is fine; the
  forced run will simply not cover sqlplus.

Key files: `.github/workflows/integration.yml`, `Makefile`
(`test-e2e-oracle`), `internal/proxy/oracle/synthetic_auth_integration_test.go`,
`docs/oracle.md` ("Exercising the synthetic AUTH fallback").
