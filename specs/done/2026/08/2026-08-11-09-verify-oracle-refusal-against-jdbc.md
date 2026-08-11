# Verify the Oracle refusal frame against a live JDBC thin client

## Goal

Run a refusal (`read_only`, `block_ddl`, a quota) through the proxy against a
real Oracle JDBC thin driver and confirm it comes back as an ORA error and
leaves the connection usable — the same assertion
`TestIntegration_BlockedStatementsAreLogged` and
`TestIntegration_BlockedStatementRefusesPythonThin` already make for go-ora v3
and python-oracledb thin.

## Why

`2026-08-10-17-oracle-refusal-frame-hangs-the-client` fixed a frame that hung
every Oracle client, and the fix is verified end to end against Oracle 23ai Free
with **three** live clients: go-ora v3, python-oracledb thin and sqlplus (OCI).
JDBC thin is not among them — no `ojdbc*.jar` was reachable on the machine the
work was done on.

That gap matters more than it looks. The three clients verified turned out to
need **three different encodings** of the same TTC summary object (compressed
with no tail fields, compressed with two, and fixed-width little-endian with an
end-of-response marker), which is exactly why `oerShape` learns its tail from
the upstream's own OERs instead of deriving it. JDBC could plausibly be a fourth
shape — and the docstring that claimed JDBC parsed the *old* frame was simply
untrue, which is the precedent for not assuming.

`internal/proxy/oracle/testdata/jdbc_thin_cursor_reexec.pcapng` shows JDBC thin
does negotiate a 23ai server the same way the other thin clients do, so the
compressed encoding is the likely answer — but "likely" is what this repo has
been burned by here.

No GitHub issue yet — file one when picking this up.

## Implementation

- The learning path is client-agnostic, so the first thing to check is simply
  whether it converges: run a JDBC session through the proxy with
  `DBB_LOG_LEVEL=debug` and look for `learned OER tail shape from upstream`
  (`session.learnOERTail`), then confirm `extra_tail_fields` matches what the
  server sends JDBC.
- Cheapest harness, and the one used for the other three clients: a tap that
  sits between the client and a real Oracle, learns the shape from the server's
  OERs, and answers one statement with `encodeOER` instead of forwarding it.
  That needs no dbbat instance and no grant, and it isolates the frame from
  everything else. See the commit that added `ttc_oer_encode.go` for the shape
  of it.
- If JDBC needs a fourth variation, add it to `oerShape` and learn it the same
  way; do **not** key it off a capability byte without measuring, and record
  the measurement in `docs/oracle.md` under "Ending a call: the OER encoder".
- Then add a JDBC case to `blocked_integration_test.go`, skipped when no
  `ojdbc` jar is on the classpath, mirroring how the python case skips when
  python-oracledb is not installed.
- While a JDBC client is set up, also confirm the refusal leaves the connection
  usable (`mustStaySurvivable`'s assertion), which is the half only observable
  once the call ends.
