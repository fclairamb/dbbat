# The "Oracle proxy has been tested with" table contradicts the rest of docs/oracle.md

**No GitHub issue filed yet — one should be.**

## Goal

Reconcile the client table under `## Testing` in `docs/oracle.md` (around line
706) with the authoritative one under `### Client compatibility on Oracle 23ai`
(around line 930), or delete it.

## Why

The first table is a historical snapshot from before the 23ai auth work landed
and every interesting row in it is now wrong:

| Row | Says | Reality (measured, later in the same file) |
|---|---|---|
| `sqlplus / OCI` | "Fails at AUTH vs Oracle 23ai" | works — auth + query over the wide TTC encoding, and now proved end to end in CI by `TestIntegration_SqlplusLoginThroughSyntheticAuth` |
| `python-oracledb thin` | "fails at AUTH vs Oracle 23ai" | works — FAST_AUTH de-pipelined, verifier 18453 |
| `SQLcl (ojdbc)` | "fails at AUTH vs Oracle 23ai (ORA-03113)" | works on 26.1.2 |

A reader hitting the first table stops there; it is the first thing under
`## Testing`. Two tables in one document disagreeing about whether a client can
authenticate is worse than one table, and the second one carries the
measurements and the test names.

Noticed while wiring the OCI client into CI
(`specs/todos/2026-08-12-05-oci-client-coverage-runs-nowhere-in-ci.md`); left
alone there because the staleness is uniform and predates that change, so a
one-row fix would have made the table look current while the rest of it stayed
wrong.

## Implementation

- Prefer deleting the `## Testing` table outright and pointing at
  `### Client compatibility on Oracle 23ai`, which is the one that is kept up to
  date and names the tests behind each verdict.
- If it is kept, it needs the DBeaver and "row capture partial" rows carried
  over — those are the only claims it makes that the newer table does not.
- Check `website/docs/` for a copy of the same table before editing.

Key files: `docs/oracle.md`.
