# An Oracle refusal never ends the client's call — the statement hangs forever

## Goal

Make `session.writeTTCError` emit a frame a real Oracle client accepts as the
end of the call, so a statement dbbat refuses comes back to the client as an
ORA error instead of hanging until the client's own timeout (or forever, if it
has none).

## Why

This is a live correctness bug on the shipped Oracle path, not a test problem.
Every refusal — `read_only`, `block_ddl`, `block_copy`, a quota, an expired or
revoked grant — funnels through `gateStatement` →
`session.sendOracleError` → `session.writeTTCError`
(`internal/proxy/oracle/intercept.go:858`), and that function synthesises a
frame that no client parses:

```go
buf = append(buf, 0x00, 0x00)                 // data flags
buf = append(buf, byte(TTCFuncResponse))      // 0x08  ← wrong message type
buf = append(buf, 0x01)                       // "sequence number"
… 4-byte error code, 2-byte cursor id, 4-byte row count, 2-byte flag,
  2-byte message length, message text — none of it TTC-encoded
```

An Oracle server ends a call with message type **`0x04` (OER)**, whose body is
the summary object: TTC *compressed* integers, not fixed-width big-endian ones.
`0x08` is a different message entirely, and its fields are read as
`size := int(2); size × int(4)` — so a client hands our bytes to the wrong
parser, consumes them as a length and a list, and then blocks reading the rest
of a message that will never arrive.

Confirmed end to end against Oracle 23ai Free through the real proxy: an
`INSERT` under a `read_only` grant left `go-ora v3`'s `Stmt.ExecContext` parked
in `select` for as long as the test was allowed to run. The goroutine dump is
unambiguous — client in `command.go:1239`, proxy in `upstreamToClient`, nothing
in flight. The refusal *is* correctly enforced (the statement never reaches
upstream) and *is* correctly logged (a `queries` row with the refusal as
`error`); only the client-facing half is broken.

Nothing caught this because the Oracle suite had no client-through-proxy
refusal until now: `blocked_persist_test.go` calls `handleOALL8` directly and
never looks at what goes back on the wire, and the docstring's claim that
sqlplus and JDBC parse this frame has never been exercised against a live
client either — treat both of those as unverified too.

`internal/proxy/oracle/blocked_integration_test.go` currently bounds each
refused statement with a `context.WithTimeout` and says so in a comment; that
bound comes out when this lands, and the test should then assert the client
receives an ORA error.

No GitHub issue yet — file one when picking this up.

## Implementation

- Write the OER encoder as the mirror of the decoder already in
  `internal/proxy/oracle/ttc_oer.go` (`decodeOERFieldsAt`, `readCompressedInt`
  in `ttc_auth.go`). Message type `0x04`, then the summary fields in the order
  `go-ora`'s `network.NewSummary` reads them
  (`network/summary_object.go`): end-of-call status, ECID sequence, current row
  number, **return code**, array-element error fields, cursor id, error
  position, sql type, oer-fatal, flags, … , then the bind-error blocks (all
  zero-length), then the wide `RetCode`/`CurRowNumber` pair, then the error
  text as a CLR chunk. Set the end-of-call bit (`oerEndOfCallBit`) in the
  status word — the decoder in this package already keys completion off it.
- The obstacle, and the reason this is its own task: several of those fields
  are **conditional on what client and server negotiated** —
  `HasEOSCapability`, `HasFSAPCapability`, `TTCVersion`, `UseBigClrChunks`. The
  proxy does not track any of them today. Either learn them by watching the
  OSETPRO/ODTYPES exchange in `phase1_forward.go` / `phase2_forward.go` and
  keep them on the session, or derive the shape from the real OERs the server
  sends (every successful statement carries one, and `findOERInResponse`
  already locates them).
- Verify against more than `go-ora`: the same refusal has to end the call for
  python-oracledb thin (the workload harness in
  `cursor_learning_integration_test.go` already drives it) and, if a JDBC or
  OCI client is reachable, for those too — the current docstring's claim about
  sqlplus and JDBC is exactly the sort of thing that turned out to be untrue
  for `go-ora`.
- Consider what a refusal owes the session afterwards. PostgreSQL keeps the
  connection alive across a refusal and the Oracle path should too, but that is
  only observable once the call actually ends.
