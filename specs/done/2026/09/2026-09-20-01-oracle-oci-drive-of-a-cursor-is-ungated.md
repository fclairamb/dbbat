---
model: opus
effort: medium
---

# An OCI client re-executing a cursor is forwarded ungated: the wide exec header declares no statement and nothing reads it

## Goal

Read the cursor id out of an OCI/wide `03 5e` execute op that declares **no
statement**, so a re-execution driven from sqlplus, Instant Client or
SQL*Developer-over-OCI is gated against the SQL that cursor was parsed with —
the way every thin client's re-execution already is.

## Why

Found on 2026-09-20 while implementing
`2026-09-19-06-oracle-refcursor-id-in-the-oci-encoding.md`. That spec's premise
was that a REF cursor driven from sqlplus is `ORA-01031`'d under a `read_only`
grant. Measured live, it is not — and the reason is worse than a refusal:

`execNoStatementCursorAt` (`internal/proxy/oracle/ttc_exec_statement.go`) bails
out on the wide header outright, with the comment "no recording carries a
SQL-less one, and its cursor id does not sit where the thin walk below looks".
One does now: `testdata/oci_refcursor_drives.hex` is two of them. So the frame
decodes as "could not find SQL text", a decode failure is forwarded ungated, and
from the second execution of any cursor on an OCI session — REF cursor or
ordinary prepared statement — `read_only`, `block_ddl`, the approval patterns,
the `queries` row and the quota all apply to the parse alone. That is exactly
the defect `execNoStatementCursor` was written for on ojdbc6, on a different
client.

The id's position **is** known now, and it was measured against an independent
witness rather than guessed: in the wide header the eight bytes
`execSQLLengthWideField` calls "options" are two four-byte fields, and the second
is the cursor id. It is zero on every recorded frame that carries a statement (a
parse allocates its cursor) and non-zero on exactly the frames that carry none —
2 and 5 in the drives fixture, which are the two ids
`refCursorIDsInBindOutput` reads out of the calls' bind output in the same
session (`TestDumpReplay_OCIRefCursorIDsMatchTheCursorsTheClientDrives`).

**This has to come after that spec, not before.** Gating these frames while a
REF cursor's id was unlearnable on OCI would have turned every sqlplus
`VARIABLE rc REFCURSOR` / `PRINT rc` into the `ORA-01031` the REF-cursor work
exists to prevent. With the ids now learned, they resolve.

## Implementation

1. Teach `execNoStatementCursorAt` the wide header instead of refusing it: when
   `execSQLLengthWideField` does **not** fit (a SQL-less frame has zeros where
   the statement's pointer sentinel goes) but the header prefix does —
   `03 5e`, `[seq]`, `[0x01][seq+1]` — read the cursor id at body[9:13] LE and
   apply the same bounds the thin path does (`> 0`, `<= cursorReexecMaxID`).
   Keep it as narrow as the thin reading: a false positive here refuses a
   re-execution on a client that was working.
2. The frame arrives as a close-cursors piggyback with the exec stapled behind
   it, so the existing `closeCursorsEnd` fallback in `execNoStatementCursor`
   already covers reaching it — check that path rather than adding another.
3. Cross-check it the way the REF-cursor walk is cross-checked, and with the
   fixtures that already exist: the id read out of
   `testdata/oci_refcursor_drives.hex` must be the id
   `refCursorIDsInBindOutput` read out of
   `testdata/oci_refcursor_bind_output.hex`, in order. Those two files come from
   one session and were selected by position, so the agreement is real evidence.
   Add the negative: on `testdata/sqlplus_cursor_reexec.pcapng` and every other
   OCI recording, a frame that **does** carry a statement must still yield no
   re-execution (its field is zero, and zero must stay "a parse", exactly as
   `decodeCursorReexec` refuses it).
4. Then assert it end to end: extend
   `TestIntegration_RefCursorFromSQLPlusUnderReadOnly` (or add a sibling) to
   require the drives to be *gated* — `logMsgReexecGated` twice, still zero
   `logMsgUntrackedCursorRefused`, rows still returned. Today only the first
   half of that is true. A second live case is worth having: an ordinary
   prepared statement re-executed from sqlplus under `read_only`, which must be
   refused when it is a write and allowed when it is a read.
5. Update `docs/oracle.md` ("Learning a REF cursor's id" carries the caveat
   paragraph naming this spec, and the cursor-re-execution section describes the
   three re-execution shapes) and the `execNoStatementCursorAt` comment that
   says the wide header is not read here.

## Files

- `internal/proxy/oracle/ttc_exec_statement.go` — `execNoStatementCursorAt`,
  `execSQLLengthWideField`
- `internal/proxy/oracle/ttc_decode.go` — `decodePiggybackExecSQL`,
  `closeCursorsEnd`
- `internal/proxy/oracle/testdata/oci_refcursor_drives.hex`,
  `oci_refcursor_bind_output.hex` — the evidence, already recorded
- `internal/proxy/oracle/refcursor_oci_integration_test.go` — the live half
- `docs/oracle.md`
