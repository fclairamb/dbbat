---
model: opus
effort: medium
---

# The 64-bit OCI dialect learns no REF cursor id, and its drives are ungated

## Goal

Make the REF-cursor id reading and the SQL-less exec gating work on the
**wide64** OCI dialect — the one `isCloseCursorsWide8Header` knows — so an
sqlplus that writes 8-byte integers gets the same treatment as one that writes
4-byte integers.

## Why

Measured 2026-09-20 while implementing
`2026-09-20-01-oracle-oci-drive-of-a-cursor-is-ungated.md`, by running
`TestIntegration_RefCursorFromSQLPlusUnderReadOnly` against **both** OCI clients
(`ORACLE_TEST_OCI_CLIENT=path` and `=container`) rather than whichever one the
machine had:

| client | dialect | REF cursor ids learned | drives gated |
|---|---|---|---|
| Instant Client on PATH (23.x, homebrew) | 4-byte | 2 | 2 |
| sqlplus bundled in `gvenzl/oracle-free:23-slim` (23.26) | **64-bit** | **0** | **0** |

Two consequences, and the second is the one that matters:

1. The live test was **failing** on the 64-bit dialect — on the id-learning
   assertion that predates the gating work, not only on the new one. CI runs
   exactly that client (`.github/workflows/integration.yml` reaches sqlplus back
   out of the Oracle container over `host.docker.internal`), so it was red there
   and had not been noticed because the branch is unpushed. It is now
   dialect-aware: the claims on the 4-byte dialect, the measured zeros pinned on
   the 64-bit one, so closing this fails the test rather than passing unnoticed.
2. A drive on the 64-bit dialect is still **forwarded ungated**, which is the
   same live control bypass `2026-09-20-01` closed for the 4-byte one: from the
   second execution of a cursor, `read_only`, `block_ddl`, the approval
   patterns, the `queries` row and the quota apply to the parse alone. It is a
   narrower blast radius (a REF cursor is what has no text to resend, and an
   ordinary repeated statement on OCI does resend its text — the repeated-
   statement live test passes on both dialects), but it is the same class.

Both halves come from the same root: `refCursorIDsInBindOutput`'s shape has a
`fixedWidth64` axis, but nothing was ever recorded in that dialect to fit it,
and `execSQLLengthWideField` / `execWideNoStatementCursor` key on
`body[3] == 0x01`, which the 17-byte 64-bit op header does not carry.

## Implementation

1. **Record it first.** The 4-byte work is only trustworthy because it was
   pinned against recorded frames and cross-checked in order; do the same here
   rather than extrapolating offsets. `capture_refcursor_test.go` already writes
   the `oci_refcursor_bind_output.hex` / `oci_refcursor_drives.hex` pair —
   re-run it with `ORACLE_TEST_OCI_CLIENT=container` and keep the result as its
   own fixture pair, so the two dialects are two sets of evidence, never one
   walk asked to fit both.
2. **The bind-output walk.** Check what `ociOERShape()` / `fixedWidth64`
   actually produce against the new fixture before changing anything: the axis
   may be right and only the caller wrong. Whatever the answer, the same
   false-positive guard applies — the scalar out-bind fixture must yield no id
   in this dialect either.
3. **The drive.** Locate the cursor id in the 64-bit exec header the way the
   4-byte one was located: it must be zero on every recorded frame carrying a
   statement and non-zero on exactly the SQL-less ones, and the ids it yields
   must be the ids the bind-output walk read, in order. Then route it through
   `execNoStatementCursor` → `PiggybackExecNoSQLError` → `handleCursorReexec`,
   the same gate, with no parallel path.
4. **Order matters, again.** Gate the drive only once the id is learnable, or
   every `PRINT rc` on this dialect becomes the `ORA-01031` the REF-cursor work
   exists to prevent — the same trap `2026-09-20-01` had to be sequenced around.
5. **Flip the pin.** `TestIntegration_RefCursorFromSQLPlusUnderReadOnly`'s
   64-bit branch asserts the zeros *as the measured gap*; replace it with the
   same claims the 4-byte branch makes, and keep running the test both ways.

## Files

- `internal/proxy/oracle/refcursor_bind.go` — the bind-output walk and its shape
- `internal/proxy/oracle/ttc_exec_statement.go` — `execWideNoStatementCursor`,
  `execSQLLengthWideField`
- `internal/proxy/oracle/ttc_decode.go` — `isCloseCursorsWide8Header`,
  `usesWide64OpHeader`
- `internal/proxy/oracle/capture_refcursor_test.go` — the fixture recorder
- `internal/proxy/oracle/refcursor_oci_integration_test.go` — the live half
- `docs/oracle.md` — "Learning a REF cursor's id", "Cursor re-execution"
