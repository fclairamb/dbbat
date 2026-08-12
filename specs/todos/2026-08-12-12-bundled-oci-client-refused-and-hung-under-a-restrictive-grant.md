# The DB-bundled OCI client is refused on its first call under a restrictive grant, and then hangs

**No GitHub issue filed yet — one should be.**

## Goal

Make a `read_only` (or any restrictive) grant usable from the Oracle-bundled
sqlplus: its first statement must execute, and a refusal must reach it as an ORA
error rather than hanging the session.

## Why

Measured, not suspected. Wiring the OCI client into CI
(`specs/done/.../2026-08-12-05-oci-client-coverage-runs-nowhere-in-ci.md`) put
a *second* OCI flavor in front of the proxy for the first time: the client
bundled in `gvenzl/oracle-free:23-slim` (23.26), alongside the Instant Client
23.3 the fixtures were captured from. It authenticates fine — the login test
passes on it — but under a restrictive grant it dies immediately:

```
level=INFO  msg="Oracle session established, entering proxy mode"
level=DEBUG msg="TTC message" func=OFETCH
level=WARN  msg="refused a re-execution of an untracked cursor under a restrictive grant" cursor_id=27396
```

Three things are wrong there, in order of how much they should worry us:

1. **The first client message in proxy mode decodes as `OFETCH`**, on a session
   that has not executed anything. A fresh session has no open cursor to fetch
   from.
2. **`cursor_id=27396`** is not a plausible cursor id — the ids this suite
   learns from the same upstream are single digits (`cursor_id=3`,
   `cursor_id=6`). It reads like a misaligned parse of the client's call, which
   would point at the wide/OCI framing rather than at the gate.
3. **The refusal then hangs the client.** sqlplus never comes back and never
   prints a line — not even the output of the statement it was refused on. The
   whole point of the refusal path is that the client gets ORA-01031 and keeps
   going; the refusal test (`TestIntegration_BlockedStatementRefusesSQLPlus`)
   exists because a frame the client cannot consume parks it forever.

The blast radius is real: **any** dbbat user connecting with an Oracle-bundled
OCI client under a `read_only` / `block_ddl` / `block_copy` grant would be
refused on their first statement and then hang. Restrictive grants are the
normal case for a proxy that exists to constrain access.

The Instant Client 23.3 on PATH passes the same test against the same upstream
image, so this is a property of that client flavor (or of how dbbat frames for
it), not of the gate in general.

## Repro

```bash
# fails: hangs, then context deadline exceeded with no output
ORACLE_TEST_REQUIRE_OCI_CLIENT=1 ORACLE_TEST_OCI_CLIENT=container \
  go test -tags integration -timeout 40m -count=1 -v \
  -run TestIntegration_BlockedStatementRefusesSQLPlus ./internal/proxy/oracle/

# passes on a host with an Instant Client on PATH
ORACLE_TEST_OCI_CLIENT=path ... same -run
```

`TestIntegration_BlockedStatementRefusesSQLPlus` currently **skips** on the
container flavor, naming this file
(`knownBadBundledRefusal` in `internal/proxy/oracle/oci_client_integration_test.go`).
Deleting that skip is how the fix is verified — it is the last step of this
task, not an optional one.

## Implementation

- Start at the decode, not the gate: dump the first client packet in proxy mode
  for both flavors and diff them (`DBB_DUMP_DIR`, or the `ORACLE_TEST_LOG`-style
  tee used to capture the trace above). If the 23.26 client's first call is
  genuinely an `OFETCH`, item 1 is a protocol fact dbbat has to handle; if the
  packet is really an exec and dbbat reads `OFETCH` out of it, the bug is in the
  wide framing walk and item 2 is the same bug's second symptom.
- Suspect the interaction flagged in
  `specs/todos/2026-08-12-08-oracle-wide-leg-bigclrchunks-unverified.md`: the
  wide/OCI leg ignores `UseBigClrChunks`, which the upstream advertises on
  every 23ai session. Two clients that differ in how much they lean on chunked
  CLR would then diverge exactly here. Check that first — it is cheap and the
  two specs may be one bug.
- Whatever the cause of the misread, the hang is a separate defect and deserves
  its own fix: a refusal written for a call the client is not parked on must
  still end *some* call, or the gate must decline to refuse a message it could
  not confidently decode. Failing open on an undecodable message is the safer
  default for a call dbbat cannot even name.
- Add the bundled client's first-call bytes to `testdata/` while the repro is
  in hand, so the unit half of this can be pinned without a container.

Key files: `internal/proxy/oracle/session.go` (`nextOERFrame`,
`observeClientCallNumber`), `internal/proxy/oracle/intercept.go` (the untracked
re-execution gate), `internal/proxy/oracle/cursor_reexec_gate_test.go`,
`internal/proxy/oracle/oci_client_integration_test.go`.
