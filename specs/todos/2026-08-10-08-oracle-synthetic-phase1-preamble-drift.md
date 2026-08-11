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

## Implementation Plan

### What the evidence says (established before touching code)

go-ora v3 `basicSession.PutTTCFunc` (module cache,
`network/basic_session.go:487`) writes `[ttcCode][funcCode][ttcIndex]` and
appends **one extra `0x00` when `TTCVersion >= 18`**; `TTCVersion` is
`min(client CompileTimeCaps[7], server CompileTimeCaps[7])`
(`data_type_nego.go:588`). `doAuth` then writes `PutBytes(1)` before the
compressed `user_id_len`. So Phase 1 is

```
03 76 <seq> [00]  01  01 <userLen>  01 <mode>  01 01 05 01 01  <clr> user
```

and Phase 2 (`auth_object.go:332`) is the same header with sub-op `0x73`,
`seq` one higher, and `01 <numPairs> 01 01` in place of the fixed magic.

Both widths are real, confirmed against the repo's own captures
(`internal/proxy/oracle/testdata/*.pcapng`, bytes preceding `AUTH_TERMINAL` /
`AUTH_SESSKEY`):

| capture | Phase 1 header | Phase 2 header |
|---|---|---|
| `go_ora_cursor_reexec` (go-ora v3, 23ai) | `03 76 01 00` | `03 73 02 00` |
| `jdbc_thin_cursor_reexec` | `03 76 01 00` | — |
| `python_thin_cursor_reexec` | `03 76 01` | `03 73 02 00` |
| `python_thin` / `dbeaver` (older) | `03 76 01` | `03 73 02` |
| `go_ora` (go-ora v2 era) | `03 76 00` | `03 73 00` |

So there is no single constant: the trailing `00` follows the negotiated TTC
field version, and `seq` is a per-session TTC function counter (1 for Phase 1,
2 for Phase 2 on every modern capture). dbbat currently hard-codes
`03 76 00 01` / `03 73 00 …` — the go-ora **v2** shape, which is one byte short
of what a v3/23ai session expects. The spec's transcription is confirmed.

### Changes

1. `internal/proxy/oracle/upstream_auth_client.go`
   - Add `syntheticAuthHeader(sub, seq byte) []byte` on `session`: returns
     `[03 <sub> <seq> 00]` by default (modern, what the integration stack
     negotiates) and drops the trailing `00` when the client's own captured
     AUTH body shows the 3-byte framing. Detection helper
     `ttcAuthFuncHeaderExtended(body)`: for a thin (non-wide) AUTH body,
     byte 3 is either the extension `0x00` or the `0x01` "username present"
     marker, so it decides the width unambiguously.
   - `buildClientAuthPhase1` / `buildClientAuthPhase2` take the header as a
     parameter instead of hard-coding it; call sites pass
     `s.syntheticAuthHeader(PiggybackSubAuth1, 1)` and
     `s.syntheticAuthHeader(PiggybackSubAuth2, 2)`.
2. Test seam so the fallback is testable forever instead of once: package var
   `forceSyntheticUpstreamAuth` (always false in production code), consulted by
   `sendUpstreamAuthPhase1` / `sendUpstreamAuthPhase2`, flipped by a `TestMain`
   in a new `//go:build integration` file when
   `DBBAT_ORACLE_FORCE_SYNTHETIC_AUTH=1`.
3. Fixtures: `buildPhase1Body` in `phase1_forward_test.go` moves to the real
   `03 76 01 00 01` shape (the rewrite tests stay valid as round-trips);
   `upstream_auth_client_test.go` asserts the new builder output.
   `oci_instantclient_test.go` bodies (`03 76 02 01 03 …`) already match the
   captured OCI/wide framing and need no change.
4. `docs/oracle.md`: state the header rule and how to exercise the fallback.

### Verification

`make lint`, `make test`, plus the run the spec prescribes — with the rewrite
disabled:

```bash
DBBAT_ORACLE_FORCE_SYNTHETIC_AUTH=1 go test -tags integration -v -timeout 20m \
  -count=1 -run TestIntegration_MCPExecutesThroughTheProxy ./internal/proxy/oracle/
```

### Verification result (2026-08-10, gvenzl/oracle-free:23-slim on arm64)

- Forced-synthetic run with the fix: **PASS** (170s).
- Control, same run with `syntheticAuthHeader` temporarily reverted to the
  pre-fix `03 76 00` / `03 73 00`: **FAIL**, with the upstream answering
  `upstream AUTH Phase 1 rejected: ORA-03120: two-task conversion routine:
  integer overflow` — the exact symptom this spec reported, reproduced and
  then removed. The control also proves the seam really does bypass the
  rewrite, since with the rewrite in play the run cannot fail this way.
- `make lint` clean, `make test` green, `make build-binary` OK.
