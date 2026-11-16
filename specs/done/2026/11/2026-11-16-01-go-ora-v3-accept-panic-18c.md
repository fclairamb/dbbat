---
model: opus
effort: medium
pr: batch
automerge: false
---

# go-ora v3 panics on Oracle 18c XE's 41-byte Accept packet

GitHub Issue: https://github.com/fclairamb/dbbat/issues/500

## Source

Issue #500: the nightly `Integration tests` workflow has failed every run for weeks. The
`oracle (18c XE (pinned))` leg dies on its very first test,
`TestIntegration_AsyncRefusalAgainstOCIAndPythonThin`, with a panic that takes the whole
package down (run 43797954046, and the same panic on the runs of 2026-10-03, 10-19 and 10-30):

```
panic: runtime error: index out of range [3] with length 0
encoding/binary.bigEndian.Uint32(...)
github.com/sijms/go-ora/v3/network.newAcceptPacketFromData({0xc0012bc000, 0x29, 0x40}, …)
    go-ora/v3@v3.0.1/network/accept_packet.go:61
…
oracle.startOracleThroughProxyWith  cursor_learning_integration_test.go:228  (client.PingContext)
```

The report calls it a regression; it is one in go-ora, not in dbbat. The test is only the first
to run: every other 18c test would panic the same way.

## Problem

- `go-ora/v3@v3.0.1/network/accept_packet.go:61` reads
  `NegotiatedOptions2: binary.BigEndian.Uint32(packetData[41:])` unconditionally, after a
  `len(packetData) < 32` guard only. Oracle 18c XE answers with a 41-byte (`0x29`) Accept, so
  the read is past the end. go-ora v2.9.0 (`network/accept_packet.go`) has no such read: the
  field arrived with v3, which dbbat adopted in `7458000` (2026-07-23).
- dbbat forwards the upstream's Accept to the client as is
  (`internal/proxy/oracle/relay_preauth.go:75-102`); `stripAcceptModernAuthFlags` already
  returns early on a packet shorter than `acceptFlagsOffset+4` (`relay_preauth.go:455`), so dbbat
  itself copes with the short Accept. The crash is in the go-ora *client*.
- The same go-ora code runs in production, not only in tests: the connectivity check's Oracle
  probe (`internal/proxy/conncheck/probes.go:158` → `upstream.ConnectOracle`,
  `internal/proxy/upstream/oracle.go:56`) and the MCP executor (`internal/mcp/exec_oracle.go`).
  Against an 18c upstream either one panics; whether that kills the process depends on who
  recovers (`internal/mcp/server.go:358` recovers, `internal/api/servers_connectivity.go` needs
  checking: if the probe runs on a goroutine of its own, nothing recovers it).

Not reproduced locally (the XE image is amd64-only, and this Mac is arm64); the diagnosis rests
on the CI stack trace and the go-ora source.

## Why a spec

- It needs a decision outside dbbat's own code (patch, fork or pin a dependency).
- Correctness of a production path (the connectivity check, the MCP executor) against a supported
  server version, with a possible process crash.

## Proposal

1. Confirm the Accept layout on 18c: capture one (an amd64 runner or the nightly with a debug
   dump of `acceptPkt.Raw` in `relay_preauth.go`) and keep it as `testdata/accept_18c.hex`.
2. Fix go-ora: send the upstream a PR guarding the `[41:45]` read on `len(packetData) >= 45`
   (and the `[45:61]` UUID read, already guarded on `length`). Until it is released, carry the
   patch with a `replace github.com/sijms/go-ora/v3 => github.com/fclairamb/go-ora/v3 <tag>`
   (see open question).
3. Defence in depth in dbbat, whatever go-ora does: recover a panic inside
   `upstream.ConnectOracle` and `internal/mcp/exec_oracle.go`'s connect, turning it into an error
   ("the Oracle driver could not parse the server's Accept"), so a driver bug cannot take the
   process down. Unit test: a fake TNS listener answering a 41-byte Accept; `ConnectOracle`
   returns an error, no panic.
4. Re-run the 18c leg (`gh workflow run "Integration tests"`) and fix whatever it then shows:
   until this lands, it has never got past its first test, so further 18c failures may surface.

## Open questions

- **Fork with `replace`, or wait for upstream?** Recommended: fork with `replace` now and drop it
  once go-ora releases the fix. The nightly stays red until then otherwise, and the production
  probe keeps panicking. Trade-off: a fork to track, and Renovate stops proposing go-ora
  updates while the `replace` is there.

## Closing the issue

This spec covers part of #500 (the 18c leg). Issue #500 is already closed by the fix commits of
2026-11-16 and this spec. The implementing PR body carries `Refs #500`.

## Resolved open questions

- Fork go-ora with a `replace` now and drop it once go-ora releases the fix. (autopilot: recommended answer)
