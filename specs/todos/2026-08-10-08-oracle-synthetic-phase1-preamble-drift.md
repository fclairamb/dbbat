---
model: opus
effort: high
---

# The synthetic Oracle AUTH Phase 1 fallback builds a preamble no server accepts

**No GitHub issue filed yet — one should be.**

## Goal

Make `buildClientAuthPhase1` produce a packet Oracle actually accepts, so the
fallback it exists to provide is real rather than notional.

## Why

`session.sendUpstreamAuthPhase1` (`internal/proxy/oracle/upstream_auth_client.go`)
normally forwards the *client's own* AUTH Phase 1 with the username swapped. It
falls back to the synthetic `buildClientAuthPhase1` when the client packet is
missing or the rewrite fails — and that fallback is dead on arrival.

Measured on 2026-08-10 against `gvenzl/oracle-free:23-slim`: forcing the
synthetic path (by short-circuiting the rewrite branch) made the upstream answer
two break markers plus `ORA-03120: two-task conversion routine: integer
overflow` — the same symptom as the pair-count bug fixed in
`2026-08-10-mcp-oracle-loopback-ora-03120.md`, but from a different cause. The
preamble does not match what a real client sends:

```
go-ora v3 on the wire:  03 76 01 00 01 | 01 <userLen> | 01 <mode> | 01 01 05 01 01 | <clr> user
buildClientAuthPhase1:  03 76 00 01    | 01 <userLen> | 01 <mode> | 01 01 05 01 01 | <clr> user
                              ^^^^^ one byte short
```

(The go-ora bytes are from a recording TCP relay in front of a live 23ai
container, driving `upstream.ConnectOracle` — a login that succeeds.)

Nobody has noticed because the rewrite path always wins in practice, so this is
latent rather than user-visible. It is still worth fixing: the fallback exists
precisely for the case where the client's packet is unusable, which is the worst
moment to discover it produces garbage.

## Implementation

- Correct the preamble in `buildClientAuthPhase1`
  (`internal/proxy/oracle/upstream_auth_client.go`, ~line 796) to the observed
  `03 76 01 00 01` shape, and check the same drift in `buildClientAuthPhase2`
  (`03 73 ...`) against a captured Phase 2.
- `buildPhase1Body` in `internal/proxy/oracle/phase1_forward_test.go` mirrors the
  current (wrong) `[03 76 00 01]` layout, and several rewrite tests are written
  against it. Those tests are still valid as *round-trip* assertions — the
  rewriter must not care about the preamble width — but the fixture should be
  updated to the real shape so it stops documenting a layout no client sends.
  Check `oci_instantclient_test.go` and `sqlcl_regression_test.go` for fixtures
  built on the same assumption.
- Verify the way this was diagnosed: temporarily force the synthetic branch in
  `sendUpstreamAuthPhase1` and run

  ```bash
  ORACLE_TEST_IMAGE=gvenzl/oracle-free:23-slim go test -tags integration -v \
    -timeout 20m -count=1 -run TestIntegration_MCPExecutesThroughTheProxy \
    ./internal/proxy/oracle/
  ```

  It must pass with the rewrite disabled. Until it does, the fallback is not
  fixed — a green suite with the rewrite enabled proves nothing about it.

Key files: `internal/proxy/oracle/upstream_auth_client.go`,
`internal/proxy/oracle/phase1_forward_test.go`, `docs/oracle.md`
("Authentication path").
