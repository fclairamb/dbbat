# Oracle: reassemble SDU-fragmented statement-carrying TTC messages before gating

## Goal

A statement whose TTC execute message spans multiple TNS Data packets (anything
past ~8.1 KB of SQL under the default SDU of 8192) must be gated, recorded and
— when refused — answered exactly like a single-packet one:

1. the gate and the audit trail see the **whole** statement, never a fragment;
2. a refusal swallows **every** fragment of the message and keeps the session
   alive (OER answered, next call proceeds);
3. when dbbat genuinely cannot read a statement to its end, the error says so
   — it must not claim "dynamic SQL that is itself built from dynamic SQL"
   for a statement that carries no dynamic SQL at all.

## Why

Live incident, 2026-08-31 (Slack thread
`stonaltech.slack.com/archives/C0B2P27DLQ5/p1788189168619969`): generated
`MERGE ... USING (SELECT ... FROM dual UNION ALL ...)` batches with literal
values — zero dynamic SQL — against `abyla_abymutualise02_admin` via
python-oracledb thin. Statements past ~8 KB were refused erratically with
`ORA-01031: dbbat: dbbat cannot check dynamic SQL that is itself built from
dynamic SQL: run the inner statement directly`, deterministically per query
(8/8 on the same text), and after each refusal the connection died (the caller
saw `DPY-4011` at the next `rollback()`, hiding the real message).

### Root cause

The data-phase relay reads **one TNS packet at a time** and gates each Data
packet independently (`clientToUpstream` → `interceptClientMessage`,
`internal/proxy/oracle/session.go:1931`). TTC has no message-length field, and
unlike the AUTH phase (which reassembles, `readUpstreamAuthMessages`), the data
phase never reassembles. A statement-carrying message larger than the
negotiated SDU therefore reaches the decoder as its first fragment only.

Wire proof, from the session capture of the refused query
`01a0585d-3497-78c4-a9cb-0e4168833364` (connection
`01a0585d-3486-724d-b769-1002c52ddcf2`, file `E1_attribut_propriete.sql`,
9 178 chars = 9 214 UTF-8 bytes):

- packet #3, client→dbbat, **exactly 8 192 bytes** (the negotiated SDU): an
  `11 69` close-cursors piggyback with the stapled `03 5e` exec; the header
  declares `sqlLen = 0x23FE = 9 214`, but only ~8 110 bytes of SQL are in the
  fragment;
- packet #4, client→dbbat, 1 123 bytes: the raw continuation
  (`...ION ALL\n  SELECT 673 AS num_attr, ...`) with no TTC header of its own.

The decode then degrades in three steps (`decodeExecSQL` /
`decodePiggybackExecSQL`, `internal/proxy/oracle/ttc_decode.go`):

1. the header-anchored decode reads `sqlLen = 9 214`, but
   `locateExecSQLText` requires a contiguous run of exactly that length —
   `sqlLen > len(body)` — and fails
   (`internal/proxy/oracle/ttc_exec_statement.go:269`);
2. the offset-window scan fails for the same reason;
3. the last-resort keyword scan `findSQLInPayload`
   (`internal/proxy/oracle/ttc_decode.go:1096`) finds `MERGE` and extracts
   until the first byte outside `0x0A..0x7E` — i.e. **until the first accented
   character**, here the `è` of `'Surface Pièce'` at offset 879 — or until the
   fragment ends, whichever comes first.

The 879-char prefix ends inside a string literal (`... 'Surface Pi`). The
dynamic-SQL scan runs over it, `skipLiteral` hits end-of-input inside an open
quote, fails closed (`internal/proxy/shared/dynamicsql.go:733`), and
`ValidateDynamicSQL` returns `ErrDynamicSQLNotCheckable`
(`internal/proxy/shared/validation.go:35`) — the misleading "nested dynamic
SQL" message for a statement that has none.

This explains every measurement in the thread:

- **deterministic per query**: the truncation point is a function of the text
  (first non-ASCII byte, else the fragment boundary), so the same statement is
  always cut at the same place;
- **nothing under ~7 KB refused, 7 915 passes**: ≤ ~8 100 bytes of SQL fits
  the single 8 192-byte packet (SDU − TNS header − data flags − exec header),
  the header-anchored decode reads the full text, everything works;
- **erratic beyond**: refusal ⇔ the extracted prefix has an unterminated
  quote. A file whose first accent falls inside a literal early (they all do —
  the accents live in the French labels) is always refused; an ASCII-only
  prefix is cut at the fragment boundary instead and quote parity there is
  luck (`8 601 passe · 8 670 refusé · 9 425 passe`). Shifting the statement
  by 1–8 spaces moves neither cut across a quote, matching the measurement;
- **connection death after refusal**: `gateStatement` blocks packet #3 and
  answers the OER, but packet #4 — the orphan continuation — is then read,
  fails `parseTTCFunctionCode` shape checks or parses as an unknown function
  code, and is **forwarded upstream alone**. The upstream receives
  mid-statement bytes as a fresh TTC message, desyncs, and the session dies.
  Hence `DPY-4011` on the next call.

### Severity — this is not just a false-refusal bug

The stored evidence (queries API, 2026-08-31 14:43–15:08 UTC) shows refused
statements recorded as 74/86/216/222/240/409/879-char prefixes of 8–15 KB
files, and — worse — **statements that passed were validated and recorded
against the same truncated prefixes**:

1. **Enforcement bypass.** Everything the gate matches beyond the leading verb
   is evadable by padding a statement past the SDU: `oracleBlockedPatterns`
   (`UTL_HTTP`, `DBMS_SCHEDULER`, `ALTER SESSION ... CONTAINER=`, ...),
   approval-hold patterns, and the dynamic-SQL scan. Concretely, under a
   `read_only` grant: `BEGIN NULL; /* ~8 KB of padding */ EXECUTE IMMEDIATE
   'DROP ...'; END;` — prefix verb `BEGIN` is neither write nor DDL, the
   `EXECUTE IMMEDIATE` sits past the fragment boundary, and if quote parity at
   the cut is balanced the statement is forwarded whole to the upstream. The
   controls only ever saw the padding.
2. **Audit hole.** `/queries` records the truncated prefix as if it were the
   statement; the query-chain MACs then seal truncated text. PCI-style
   evidence for anything past ~8 KB is silently partial.
3. **False refusals** with a misleading error (the incident itself).
4. **Session teardown on refusal** via upstream desync, turning one refused
   statement into a broken batch and an uninformative client error.

## Implementation

### 1. Reassemble statement-carrying messages (the core fix)

In `clientToUpstream` (`internal/proxy/oracle/session.go`), when
`interceptClientMessage` recognizes a statement-carrying op (`03 5e`
piggyback exec, OALL8, `11 69` JDBC exec) whose header-declared `sqlLen`
extends past the packet's payload, do not gate the fragment: buffer the
packet, keep reading TNS Data packets from the client, and append each
continuation payload (strip the 8-byte TNS header and 2-byte data flags of
each packet — the capture shows continuations carry both) until the
reassembled TTC body covers `sqlLen` plus the decoder's needs, then run the
existing decode + gate over the reassembled body.

- **Forward path**: on allow, forward the buffered packets in order,
  unchanged — the reassembled buffer is for reading only, the wire bytes are
  the client's own, exactly like today's one-packet path.
- **Refusal path**: on refuse, drop **all** buffered fragments, answer the OER
  once. Nothing reaches the upstream, nothing desyncs, the session lives.
  This alone fixes the `DPY-4011` half of the incident.
- **Bounds, fail-closed**: cap the reassembly at `execMaxSQLLen` (1 MB,
  `ttc_exec_statement.go`) plus slack, and bound it by a read deadline. A
  client that declares a length it never sends is torn down like
  `gateUnnameableFrame` tears down (the session is the blast radius, not the
  gate). The declared length is attacker-controlled input: never allocate it
  eagerly, grow as packets actually arrive.
- **Detection**: the trigger is precise — `execSQLLength`/`execSQLLengthWide`
  already return the declared length; reassembly starts iff
  `sqlLen > remaining bytes of this fragment`. Same for the OALL8 path
  (`decodeOALL8`'s `ErrSQLLengthInvalid` branch is exactly this situation).
- Bind values may also spill into continuation packets; capture what is
  present, as today — binds are best-effort, the statement text is not.

### 2. Never validate a fragment as if it were the statement

Belt to the braces above, for frames reassembly cannot fix: the fallback
extractors (`findSQLInPayload`, the offset windows) must report when their run
was cut by the fragment boundary or a non-ASCII byte rather than by the TTC
framing, and a statement known to be truncated must not flow into
`ValidateOracleQuery` as if complete. Under a grant with statement-shaped
controls or with blocked/approval patterns in play, refuse it honestly
("dbbat could not read this statement to its end"); a permissive grant may
forward it, but the `queries` row must say the text is partial rather than
pass it off as the statement. (Note `findSQLInPayload` truncating at the first
byte `> 0x7E` also mutilates any non-ASCII statement it touches — the
header-anchored path deliberately accepts non-ASCII (`sanitizeSQLRun`), the
fallback contradicts it.)

### 3. Honest error for the unreadable case

`ErrDynamicSQLNotCheckable` currently covers two very different findings: a
real nested dynamic-SQL form, and "the scan fell off the end of the text"
(unterminated quote). Split them (`internal/proxy/shared/validation.go`,
`internal/proxy/shared/dynamicsql.go`): the unterminated/unreadable case gets
its own error naming the actual problem. All three protocols share the
scanner; keep the split shared.

### 4. Tests

- Unit: fragment a recorded `11 69` + `03 5e` exec at 8 192 bytes (the capture
  from this incident is the fixture shape); assert full-text decode, gating
  over the whole statement, refusal consuming both fragments, and the session
  answering the *next* call normally after a refusal.
- Unit: blocked pattern (`UTL_HTTP`) placed beyond the first fragment must be
  refused post-fix — this is the regression test for the bypass.
- Unit: non-ASCII (accented) literals beyond 8 KB survive intact into
  `/queries`.
- Integration (`//go:build integration`): python-oracledb-shaped >8 KB MERGE
  with accented literals through a live proxy; assert executed, fully
  recorded, and a read-only-grant refusal of the same statement leaves the
  connection usable.

### Key files

- `internal/proxy/oracle/session.go` — `clientToUpstream`,
  `interceptClientMessage`, `gateStatement`
- `internal/proxy/oracle/ttc_exec_statement.go` — `execSQLLength`,
  `locateExecSQLText`
- `internal/proxy/oracle/ttc_decode.go` — `decodeOALL8`,
  `decodePiggybackExecSQL`, `decodeExecSQL`, `findSQLInPayload`
- `internal/proxy/shared/dynamicsql.go`, `internal/proxy/shared/validation.go`
  — error split
- `docs/oracle.md` — document data-phase reassembly next to the existing AUTH
  OK reassembly section

### Non-goals / notes

- The other four protocols are unaffected: their wire formats carry explicit
  message lengths and are already reassembled before gating; TTC alone has no
  message-length field.
- A per-grant switch to disable the dynamic-SQL/unreadable refusal (asked in
  the thread) is **not** the fix and stays out: it would institutionalize the
  bypass in §2. The refusal disappears for legitimate statements once dbbat
  reads them whole.
- Operator workaround until shipped: keep each statement under ~8 000 **bytes**
  (not characters — accents count double in UTF-8); that guarantees the
  single-packet path. Raising the negotiated SDU (client `sdu=65535` and a
  matching listener/server SDU) also moves the threshold, but the server side
  defaults to 8192, so verify the accept packet before relying on it.
