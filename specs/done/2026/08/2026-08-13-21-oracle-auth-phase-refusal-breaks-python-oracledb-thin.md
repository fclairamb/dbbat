# Oracle: the AUTH-phase refusal frame is not an OER error → python-oracledb thin dies with DPY-5002

## Goal

A client refused at AUTH Phase 2 — no active grant (ORA-01045), ambiguous
service name (ORA-01045), bad key (ORA-01017) — must receive an error frame
that python-oracledb **thin** renders as the intended `ORA-xxxxx` message, not
`DPY-5002: internal error: read integer of length N when expecting integer of
no more than length 4`.

## Why

Reported by Nicolas Heinrich (Slack #C0B2P27DLQ5, p1786649003353289), then
reproduced against production (`dbbat.tools.stonal.io`, image 0.23.2) on
2026-08-13 with python-oracledb 3.4.2 **and** 2.5.1, both thin.

**The evidence is decisive: `N` is exactly the byte length of dbbat's own error
message.**

| Refusal | Message | `len()` | DPY-5002 reports |
|---|---|---|---|
| bad/absent O5LOGON verifier | `invalid username/password; logon denied` | 39 | `length 39` |
| ambiguous service name | `service name matches multiple dbbat databases; connect using the dbbat database name instead` | 92 | `length 92` (Nicolas's MUTU01 case) |
| no active grant | `no active grant for this database; request access via dbbat` | 59 | — |

`buildAuthFailed` (`internal/proxy/oracle/ttc_auth.go:483`) writes only three
things — `0x00 0x00 TTCFuncResponse`, the compressed ORA code, then
`ttcClr(message)`. A real server sends a full OER error structure first, so the
driver's `_process_return_parameters` → `read_str_with_length` → `read_ub4`
lands on the CLR's **length byte** and tries to read it as a ub4 length
indicator. Hence "read integer of length 39": 39 *is* the message length.

Every refusal therefore looks to the user like a protocol crash rather than an
actionable "you have no grant" / "pick a database". Nicolas concluded the thin
driver was at fault and started porting to thick mode, which would not have
helped.

**This is still the code at HEAD.** The large refusal-frame rework in the
2026-08-12 batch (`ttc_oer_encode.go`, `fix(oracle): end the client's call on a
refusal with a real OER frame`) rebuilt the **mid-session** refusal path
(`encodeOER`, used from `intercept.go`), and left the AUTH-phase path on the
old minimal frame. The two paths need the same treatment.

## Implementation

- Rebuild `buildAuthFailed` on the same OER encoder the mid-session refusal now
  uses (`encodeOER` / `oerSummary` in `internal/proxy/oracle/ttc_oer_encode.go`),
  rather than hand-rolling a second, weaker frame. Watch the width question the
  fixed-width work already settled: the AUTH frame must match the caps the
  client negotiated (`s.clientWideEncoding`, `bigChunks`), which at AUTH time is
  known from `observeClientAuthEncoding`.
- Note the upstreams differ: production Abyla is **Oracle 19c SE2**, while the
  integration fixture is 23ai Free. The 19c/6949 legacy summary is already a
  distinct branch a few lines above `buildAuthFailed` — cover both.
- Test per client, which nothing does today for the auth phase. Extend
  `async_refusal_clients_integration_test.go` (mid-session refusals) with an
  **auth-phase** case for python-oracledb thin, go-ora and sqlplus: assert an
  `oracledb.DatabaseError` carrying `ORA-01045`, never `InternalError`.
- Cheap local repro: any user with a valid key but no grant on the target.
