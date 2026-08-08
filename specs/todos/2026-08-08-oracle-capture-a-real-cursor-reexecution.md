# Oracle: capture a real cursor re-execution into `testdata`

**No GitHub issue filed yet — one should be.**

## Goal

Record a real Oracle client re-executing an already-parsed cursor — a SQL-less
`OALL8`, or an `OFETCH` starting a fresh query — into
`internal/proxy/oracle/testdata`, and replay it against the cursor re-execution
gate so the enforcement is proven on a frame Oracle actually sends.

## Why

`specs/todos/2026-08-08-oracle-cursor-reexec-skips-the-gate.md` shipped the
gate: a re-execution is now re-validated against the SQL the cursor holds, and
an untracked cursor fails closed under a restrictive grant. That fix was
deliberately implemented and tested with **hand-built TTC frames** — the spec's
resolved decision was "implement now, do not block on a capture".

What remains unproven is the premise. Nobody has observed JDBC thin, `go-ora`,
`python-oracledb`, OCI/sqlplus, SQLcl or DBeaver actually re-executing by cursor
id without resending the SQL; the shape is inferred from what `decodeOALL8`
accepts. So today:

- If real clients do this often, the gate is load-bearing and its exact frame
  layout should be pinned by a recording, not by a builder that encodes our own
  assumptions about the layout.
- If real clients essentially never do it, the fix is cheap hardening and
  `docs/approvals.md` should say so with evidence rather than "unmeasured".

Either answer is worth having. The synthesized fixture cannot distinguish them,
and a builder that quietly diverges from the real wire format would let the
enforcement tests keep passing while the production path stops matching.

## Implementation

- Drive each client through the proxy against the harness in `docs/oracle.md`
  (`make test-e2e-oracle`, the local loop, and the 23ai container), with
  `DBB_DUMP_DIR` set, running a workload that should provoke reuse: a prepared
  statement executed in a loop, a batched DML, a re-`execute()` on the same
  cursor object, JDBC statement caching left on.
- Find whether any capture contains an `OALL8` whose SQL length decodes to zero,
  or an `OFETCH` arriving with no query in flight. `./dbbat dump anonymise`
  first, then keep it under `internal/proxy/oracle/testdata` following the
  dump-per-use-case convention already used by `dump_replay_test.go`.
- Add a replay test next to `internal/proxy/oracle/cursor_reexec_gate_test.go`
  asserting the recorded frame decodes as `*OALL8NoSQLError` with the right
  cursor id, and that the gate refuses it under `read_only`. Keep the hand-built
  fixture too — it covers the shape independently of one client's quirks.
- Record which clients produced it (and which did not) in `docs/oracle.md`, and
  replace the "unmeasured" paragraph in `docs/approvals.md` with what was
  actually observed.
- If no client can be made to produce the shape at all, say **that** in
  `docs/approvals.md` — "not observed from any tested client, kept as
  defence in depth" is a much better sentence than "unmeasured".

Key files: `internal/proxy/oracle/ttc_decode.go` (`decodeOALL8`,
`OALL8NoSQLError`), `internal/proxy/oracle/intercept.go`
(`handleCursorReexec`, `regateCursor`, `handleOFETCH`),
`internal/proxy/oracle/dump_replay_test.go`, `docs/oracle.md`,
`docs/approvals.md`.
