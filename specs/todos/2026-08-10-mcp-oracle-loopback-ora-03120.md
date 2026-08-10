# MCP over the Oracle proxy fails upstream auth with `ORA-03120`

**No GitHub issue filed yet — one should be.**

## Goal

Make `TestIntegration_MCPExecutesThroughTheProxy`
(`internal/proxy/oracle/integration_test.go`) pass — i.e. make an MCP agent's
statement actually reach Oracle through dbbat's own listener, the way the
feature claims it does.

## Why

The test landed with `6f9de33 feat(mcp): execute Oracle statements over the
loopback proxy` and has never been green. Against
`gvenzl/oracle-free:23-slim` on 2026-08-10 it fails at the first `Execute`:

```
upstream auth failed: upstream auth failed: upstream AUTH Phase 1 rejected:
ORA-03120 ORA-03120: two-task conversion routine: integer overflow
```

`ORA-03120` is the failure mode `docs/oracle.md` ("Authentication path")
attributes to a **TTC compile-time capability mismatch**: the upstream parses a
message at a capability level it did not negotiate. Every other client family
(python-oracledb thin, JDBC/SQLcl, sqlplus/OCI, go-ora as a *human's* client)
authenticates through the proxy against 23ai, so the loopback path is doing
something the human path does not.

It had a second, unrelated bug in front of it, now fixed: the fixture created
the dbbat user as `SYSTEM`, while the proxy lowercases the wire username before
looking it up (`session.authenticateClient`), so client auth failed with
`user not found: SYSTEM` before the upstream leg was ever reached. That is why
the ORA-03120 was only visible from 2026-08-10 on.

This is the sole remaining failure in `make test-e2e-oracle` on
`ORACLE_TEST_IMAGE=gvenzl/oracle-free:23-slim` (all eight other integration
tests pass).

## Implementation

Ruled out already: disabling go-ora's 23ai fast login on the MCP connect string
(`FAST LOGIN=FALSE` in `internal/mcp/exec_oracle.go`) changes nothing — the
error is identical with it on or off.

Suggested next steps:

1. Capture both sides. `DBB_DUMP_DIR` gives a pcapng of the loopback session;
   compare its Set Protocol / Set Data Types exchange against a working go-ora
   human session (`docs/oracle.md` "Testing" lists the existing fixtures under
   `internal/proxy/oracle/testdata/`). The caps bytes at the AUTH boundary are
   what to diff.
2. Suspect the *pre-auth relay* rather than the client. The relay forwards the
   client's own Connect descriptor byte for byte and keeps the relay-phase
   upstream socket open through the AUTH boundary specifically to keep caps
   aligned (`docs/oracle.md` §"Authentication path", point 1). A loopback client
   connecting to `127.0.0.1` with an EZ-Connect descriptor whose SERVICE_NAME is
   the *dbbat entry name* is the one shape not covered by the recorded fixtures.
3. Check `session.upstreamCustomHash` and the `caps[4]&0x20` strip on this path:
   if the strip happens but the recorded value is wrong, the outgoing AUTH uses
   the wrong derivation and 23ai rejects Phase 1.

Key files: `internal/mcp/exec_oracle.go`, `internal/proxy/oracle/phase1_forward.go`,
`internal/proxy/oracle/ttc_auth.go`, `internal/proxy/oracle/session.go`,
`docs/oracle.md` ("Authentication path", "Pre-auth relay (Oracle 23ai)").

Reproduce with:

```bash
ORACLE_TEST_IMAGE=gvenzl/oracle-free:23-slim \
  go test -tags integration -v -timeout 20m -count=1 \
  -run TestIntegration_MCPExecutesThroughTheProxy ./internal/proxy/oracle/
```
