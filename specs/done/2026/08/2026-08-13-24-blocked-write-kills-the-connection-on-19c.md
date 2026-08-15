# Oracle 19c: a blocked write kills the client connection instead of refusing it

## Goal

A statement refused by access control (`read_only`, `block_ddl`, `block_copy`)
must reach the client as an ORA error on a **still-usable** connection, against
production Oracle 19c as well as against the 23ai test fixture.

## Why

Measured 2026-08-13 against production (`dbbat.tools.stonal.io`, image
**0.23.2**), upstream `Oracle Database 19c Standard Edition 2`, through
`abyla_abymutualise02_ro` with a `read_only` grant, python-oracledb 3.4.2 thin:

- The proxy blocked correctly — `WARN query blocked by access control … write
  operations not permitted with read-only access`.
- The client never received it. A first run **hung past 2 minutes** with no
  response. With `conn.call_timeout = 15000` set, the call returned after 30s
  as `DPY-4011: the database or network closed the connection`, the connection
  was dead for every subsequent statement (`DPY-1001`), and the interpreter
  exited on a segfault (139) during driver teardown.

`TestIntegration_BlockedStatementRefusesPythonThin`
(`internal/proxy/oracle/blocked_integration_test.go:248`) covers exactly this
shape and passes — against **Oracle 23ai Free**. The suite has no 19c fixture,
which is the gap: the refusal frame is shaped from what the session negotiated,
and 19c negotiates differently (there is already a distinct "legacy 19c/6949
Summary" branch in `ttc_auth.go`).

**Re-test before doing any work here.** The mid-session refusal path was
substantially rebuilt after 0.23.2 — `ttc_oer_encode.go` (new, ~930 lines),
`fix(oracle): end the client's call on a refusal with a real OER frame`,
`fix(oracle): refuse sqlplus/OCI in the encoding and framing it waits for` —
all sitting unreleased in the 2026-08-12 batch
([#313](https://github.com/fclairamb/dbbat/pull/313)). The measurement above is
against code that predates every one of those commits, so this may already be
fixed. What is *not* fixed at HEAD is the AUTH-phase twin
(`2026-08-13-21-oracle-auth-phase-refusal-breaks-python-oracledb-thin.md`).

## Implementation

- Deploy the batch, then re-run the measurement against a 19c upstream before
  writing any code. If it passes, close this and keep the regression test below.
- Either way, the suite needs a **19c** upstream, not only 23ai Free. Oracle
  19c has no Free image; options are a licensed image behind an opt-in build
  tag, or replaying a recorded 19c refusal through the decoder the way
  `testdata/*.pcapng` fixtures already do for JDBC/OCI mid-fetch. The replay
  route fits the existing pattern and needs no license.
- Assert all three properties, not just the error text: an ORA error arrives,
  it arrives promptly, and the connection survives it.

## Resolved open questions

> Deploy the batch, then re-run the measurement against a 19c upstream before
> writing any code.

**Decision: do not wait on that measurement — it needs a prod deploy and a 19c
instance, neither of which is reachable from the implementation environment.**
The live re-measurement stays with the owner. Implement the license-free half
now:

> Options are a licensed image behind an opt-in build tag, or replaying a
> recorded 19c refusal through the decoder.

**Decision: the pcapng-replay route only.** It fits the existing
`testdata/*.pcapng` pattern, needs no Oracle licence, and runs in CI. Do **not**
add a licensed-image build tag — a suite leg that can never run in CI is not
coverage.

- Add the 19c refusal replay fixture and assert all three properties above (an
  ORA error arrives, promptly, and the connection survives), so the regression
  is pinned whatever the live re-measurement concludes.
- Do not close this spec on the assumption that the 2026-08-12 refusal rework
  already fixed the live 19c behaviour; the replay coverage is what this spec
  delivers, and the owner's measurement is what confirms the fix.
