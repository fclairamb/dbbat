# Oracle: synthesized refusal frames are absent from the session dump

## Goal

A `.pcapng` session capture must contain every frame the client actually
received — including the OER error frames dbbat synthesizes itself
(`writeTTCError`, `internal/proxy/oracle/intercept.go:1166`), not only the
bytes relayed from the upstream.

## Why

Found while diagnosing the 2026-08-31 fragmented-MERGE incident
(`specs/todos/2026-08-31-01-oracle-ttc-fragment-reassembly.md`). The capture
of connection `01a0585d-3486-724d-b769-1002c52ddcf2` shows the client's
blocked exec (packets #3/#4) and the client's logoff (#5), but **no
server→client frame after #2** — yet the client demonstrably received the
`ORA-01031` refusal (it surfaced in python-oracledb). The dump is written
explicitly from the relay loops (`s.dump.WritePacket(...)` in
`clientToUpstream` / the upstream leg), and `writeTTCError` writes straight to
`s.clientConn`, bypassing the dump.

A capture is forensic evidence; a refusal is precisely the event an
investigator replays a capture to see. Today the dump of a refused statement
shows the statement and then silence, which reads as a dropped connection
rather than an enforced control. Other synthesized writes on the client leg
(auth failure frames, held-refusal answers, mid-stream limit OERs) should be
checked for the same gap.

## Implementation

- Inventory every direct `s.clientConn.Write` in the Oracle session
  (`writeTTCError`, `sendAuthFailed`, the held-refusal/mid-stream paths) and
  route each through the dump as a `DirServerToClient` packet when `s.dump`
  is set — either by calling `s.dump.WritePacket` next to the write or by
  wrapping the client conn's write side with the tap (`internal/dump/tapconn.go`)
  so nothing can bypass it again.
- Prefer the tap-wrapping approach if it does not double-record the relay
  loop's explicit writes; otherwise drop the explicit calls in favor of the
  tap so there is exactly one recording point.
- Check the other four proxies for the same pattern (synthesized error frames
  vs dump coverage) and align.
- Test: replay a refusal through a session with dumping enabled and assert the
  capture contains the OER frame (`dump_replay_test.go` has the harness
  shape).
