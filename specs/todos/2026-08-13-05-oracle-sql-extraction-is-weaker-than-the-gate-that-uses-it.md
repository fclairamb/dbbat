# The Oracle SQL extractor is weaker than the gate built on it

**No GitHub issue filed yet — one should be.** (Automation must not run
`gh issue create`; see `specs/todos/2026-08-11-06-*.md`.)

## Goal

Make `decodeExecSQL` / `findSQLInPayload` extract *the statement a frame will
actually run*, so the statement gate cannot be walked past by where the text
sits or by which verb it starts with.

## Why

Cross-cutting, and that is the point: this is **not** a property of one path.
The same extractor is what `handleJDBCExec` gates on (`intercept.go`, the
func-0x11 execute every JDBC/DBeaver session uses) and what
`gateUnnameableFrame` scans with (`session.go`, the frame dbbat cannot walk —
see `docs/oracle.md`, "Two OCI encodings, not one"). Anything it misses is
missed by both. Three holes, found during the completeness audit of
`specs/done/.../2026-08-12-12-bundled-oci-client-refused-and-hung-under-a-restrictive-grant.md`
and left unpatched there on purpose, because each deserves a measurement rather
than a guess:

1. **It returns the first hit, not the executable one.** `decodeExecSQL` scans
   offsets 50–75 and then falls back to the earliest keyword match anywhere in
   the payload. A decoy — `SELECT 1` placed ahead of the real `INSERT` or
   `DROP` — is what the gate sees, and the frame is forwarded **whole**, decoy
   and statement together. The gate then reports, records and enforces against
   a statement that is not the one the upstream runs, which is worse than a
   miss: the audit trail disagrees with what happened.

2. **The keyword list is narrower than the controls it feeds.**
   `findSQLInPayload` (`ttc_decode.go`) knows SELECT/INSERT/UPDATE/DELETE/
   CREATE/DROP/ALTER/BEGIN/DECLARE/WITH/MERGE/CALL — but **not** `TRUNCATE`,
   `GRANT` or `REVOKE`, all three of which `writeKeywords` and `ddlKeywords`
   refuse (`internal/proxy/shared/validation.go`). So a `TRUNCATE TABLE payroll`
   that lands outside the 50–75 window extracts as `""`, and both paths fail
   open on an empty extraction (`intercept.go`: "Don't block on decode failure
   — let it pass through, as OALL8 does"). A `read_only` grant does not stop it.

3. **A re-execution carries no text at all.** An unnameable frame that
   re-executes a tracked cursor holding a `DELETE` has no SQL in it, so
   `stapledStatement` returns `""` and the frame is forwarded. Cursor
   re-execution is ungated on that path. This one is pre-existing and unchanged
   — the nameable paths route re-executions through `regateCursor`, which
   resolves the cursor's statement — but the unnameable path has no cursor id it
   can trust (that is the whole reason it exists), so closing it means something
   other than resolving an id.

Two more, found in the *anchoring* the unnameable path added on top of the
extractor (`statementOpOffsets`, `session.go`). Both are open questions rather
than known holes, and both want a measurement:

4. **The anchor's op list does not match dbbat's own doctrine.** It carries
   `03 5e`, `11 69` and `11 98`, while `docs/oracle.md` ("SQL Extraction") names
   the three statement-carrying ops as **OALL8**, the v315+ piggyback exec and
   the JDBC exec — so the legacy `0e` OALL8 is absent. The exclusion is
   defended in the code on false-positive grounds (`0x0e` is one common byte;
   matching it would hand back the false positives anchoring removed), which is
   a *different axis* from the executability argument the doc leans on ("bytes
   the upstream will not execute either"). Nobody has measured whether Oracle
   executes a `0e` OALL8 stapled behind a `0x11` piggyback; until someone does,
   the two justifications are not the same justification. Mitigating, and worth
   recording next to it: this path is reachable only from the `0x11` branch, and
   `docs/oracle.md` ("Cursor re-execution") records that **no tested client
   sends OALL8 at all** — it is the pre-v315 framing, kept as defence in depth.

5. **The anchor degenerates at offset 0.** `statementOpOffsets` counts offset 0,
   so for a frame whose own header is a statement op (`11 69`, `11 98`) the scan
   covers the whole payload — the unanchored behaviour, on exactly the recorded
   shape `11 69 <close list> 11 87 <set-end-to-end-attrs>`. It is deliberate and
   documented (a frame that *is* an exec has to be read whole, or a live
   statement is forwarded ungated), and it is pinned by
   `TestUnnameableExecFrameIsGatedOnItsOwnPayload` — but it means an application
   whose module or client-identifier string reads as a refused statement loses
   its session, on any client whose close list stops walking. The measurement
   that would settle it is the same one item 1 needs: where the SQL sits
   relative to the op header in real frames, which decides whether a
   length-prefixed decode can replace the keyword scan and make the carve-out
   unnecessary.

## Implementation

**Measure before widening.** The extractor's looseness is load-bearing in both
directions: `findSQLInPayload` is a keyword scan over raw bytes, so every
keyword added is a new way for a binary frame to be read as a statement. On the
unnameable path that costs a *session* (the refusal there is a teardown, not an
ORA error), which is why that path already anchors the scan to a
statement-carrying op header (`statementOpOffsets`). Adding `TRUNCATE`/`GRANT`/
`REVOKE` blindly to the shared scan would raise the false-positive rate on the
JDBC path too.

So, in order:

1. Measure which shapes real clients actually produce. Every recording in
   `internal/proxy/oracle/testdata/*.pcapng` carries executes from go-ora,
   python-oracledb thin, JDBC thin, DBeaver and both sqlplus flavors: for each
   func-0x11 and piggyback exec, record where the SQL sits relative to the op
   header and whether the 50–75 window or the keyword fallback found it. That
   says whether a length-prefixed decode anchored on the op header can replace
   the keyword scan outright, which would fix (1) and (2) together and remove
   the false-positive surface instead of enlarging it.
2. If a proper decode is reachable, take the **last** statement-carrying op in a
   frame rather than the first, or gate every one of them — a frame with two
   execs must be enforced against both.
3. Only if a keyword scan has to survive, add the three missing verbs *and*
   pin the false-positive behaviour on the binary fixtures already in
   `testdata/` (`oci_bundled_*.hex`).
4. For (3): decide what an unnameable re-execution should mean. Fail-closed
   (tear the session down when a restrictive grant is in force and the frame is
   unnameable) is defensible but breaks any client whose framing dbbat misreads;
   measuring whether such a frame occurs at all is the first step.

## Resolved open questions

> For (3): decide what an unnameable re-execution should mean. Fail-closed (tear
> the session down when a restrictive grant is in force and the frame is
> unnameable) is defensible but breaks any client whose framing dbbat misreads;
> measuring whether such a frame occurs at all is the first step.

**Decision (2026-08-13): measure first, and fail *open* unless the measurement
shows such frames actually occur.** Do **not** implement fail-closed teardown
for an unnameable re-execution on the strength of reasoning alone.

- Step 1 is the count: across every `internal/proxy/oracle/testdata/*.pcapng`,
  how many frames are both unnameable *and* a cursor re-execution? Report the
  number even if it is zero — a measured zero is the deliverable.
- **If the count is zero**, document it as unreachable-in-practice in
  `docs/oracle.md` next to the anchoring discussion, leave the frame forwarded,
  and close this item. Do not add a teardown path for a shape no client produces.
- **If the count is non-zero**, do not decide unilaterally: report the shapes you
  found and stop, leaving the item open for the owner. A non-zero count changes
  the trade-off and is worth a human decision.

The reasoning behind the choice, which also governs items 4 and 5 below: on the
unnameable path a refusal is a **session teardown**, not an ORA error, so a
false positive costs a live session for what is a dbbat parsing bug rather than a
user policy violation. `2026-08-12-12` already had that exact risk land on the
DB-bundled OCI client. Do not enlarge the teardown surface without evidence.

> Two more, found in the *anchoring* the unnameable path added on top of the
> extractor (`statementOpOffsets`, `session.go`). Both are open questions rather
> than known holes, and both want a measurement.

**Decision (2026-08-13): these two are yours to settle by measurement, under the
same do-not-enlarge-the-teardown-surface rule above.** They are not blocking.

- **Item 4 (the `0e` OALL8 absent from the anchor's op list):** measure whether
  Oracle executes a `0e` OALL8 stapled behind a `0x11` piggyback. If you can
  establish it, add the op to the anchor list *only if* the false-positive cost
  on the binary fixtures in `testdata/` (`oci_bundled_*.hex`) is nil. If you
  cannot establish it either way, say so plainly in the doc and leave the list
  as it is — an unmeasured claim replaced by an honest "not measured" is a
  legitimate outcome here, and is what `2026-08-12-08` did in the same
  situation.
- **Item 5 (the anchor degenerating at offset 0):** perform the measurement that
  item 1 of the Implementation already asks for — where the SQL sits relative to
  the op header in real frames — and let it decide whether a length-prefixed
  decode can replace the keyword scan and remove the carve-out. If it cannot,
  keep the carve-out (it is the fail-closed direction and is already pinned by
  `TestUnnameableExecFrameIsGatedOnItsOwnPayload`) and document why.

> If a proper decode is reachable, take the **last** statement-carrying op in a
> frame rather than the first, or gate every one of them.

**Both branches are sanctioned** — gating every statement-carrying op is the
stronger of the two and is preferred where it is reachable, since "a frame with
two execs must be enforced against both" is the property that matters.

Key files: `internal/proxy/oracle/ttc_decode.go` (`decodeExecSQL`,
`findSQLInPayload`, `extractSQLAtOffset`), `internal/proxy/oracle/intercept.go`
(`handleJDBCExec`), `internal/proxy/oracle/session.go` (`stapledStatement`,
`statementOpOffsets`), `internal/proxy/shared/validation.go`.

## Implementation Plan

Written after step 1. The measurement is in
`internal/proxy/oracle/sql_extraction_survey_test.go` (`go test
./internal/proxy/oracle/ -run TestSurvey -v`); the numbers below are what it
reported against the 22 recordings in `testdata/`.

### What the measurement found

1. **A header-anchored length-prefixed decode is reachable, and it replaces the
   keyword scan.** The piggyback exec's header carries the SQL length as a
   compressed int at a walkable offset:
   `[03][5e][seq][(v315 pad)][options][cursorID][flag][sqlLen]…`. Verified
   byte-for-byte: go-ora's `UPDATE dbbat_dml_test SET name = 'updated' WHERE
   id <= 3` is 56 bytes and its header carries `01 38`; DBeaver's `ALTER SESSION
   SET CURRENT_SCHEMA=TESTADM` is 40 and carries `01 28`. sqlplus/OCI writes the
   same field as a little-endian `ub4` after the wide header's pointer sentinel.
2. **There is only one statement-carrying op shape on the wire.** `11/98` (the
   "python oracledb exec" in dbbat's anchor list) appears in **zero** frames,
   and every `11/69` that carries SQL is a *close-cursors* piggyback with a
   `03 5e` exec stapled behind it — which `closeCursorsEnd` already locates. So
   the "JDBC exec" was never a distinct layout; `decodeExecSQL`'s 50–75 window
   was scanning *past a close list* into the stapled op's SQL.
3. **The defect is far broader than the spec claimed.** It is not a planted
   decoy: of the **137** execute ops in the corpus the old scan agreed with the
   statement on 89 and returned a *mid-statement fragment* on **48**, produced
   by ordinary clients with no adversary at all. (Counted per production decode
   path rather than per op — the `11 69` and `03 5e` views of a stapled execute
   are decoded separately — it is 130 of 207.) `extractSQLAtOffset` accepts any byte inside the SQL text as a length
   prefix (an ASCII space is 32, `T` is 84), `looksLikeSQL` matches a keyword
   with no word boundary, and nothing requires the declared run to be printable.
   The measured consequences:
   - 18 frames of `ALTER SESSION SET …` extracted as `SET …` — `ALTER` is in
     both `writeKeywords` and `ddlKeywords`, `SET` is in neither, so
     `read_only`/`block_ddl` **did not fire on a statement they are written to
     refuse**;
   - go-ora's `UPDATE … SET …` extracted as `SET name = 'updated' WHERE id <=`,
     same bypass;
   - `SELECT 'YES' FROM USER_ROLE_PRIVS WHERE GRANTED_ROLE='DBA'` extracted as
     `GRANTED_ROLE='DBA'…` — `GRANT` matching `GRANTED_ROLE`;
   - 76 frames of a DBeaver `SELECT` recorded in `/queries` as
     `COMMENTS FROM ALL_TAB_COMMENTS …`.
4. **Unnameable ∧ cursor re-execution = 0** across every recording. Item 3 of
   the Why section is unreachable in practice; per the resolved open question it
   is closed with the frame left forwarded.
5. **A decodable legacy `0e` OALL8 stapled behind a `0x11` piggyback: 0.**
   Item 4 is closed by leaving the anchor list as it is.

### What gets built

1. `execSQLLength` — walk the exec header (thin and OCI-wide) to the declared
   SQL length. `locateExecSQLText` — find the printable run of *exactly* that
   length whose start is not inside a longer text run. Together
   `decodeExecStatement`.
2. `decodePiggybackExecSQL` and `decodeExecSQL` try the precise decode first
   (`decodeExecSQL` walking the close list to the stapled op), and keep the old
   window+keyword scan only as a fallback for a shape no recording produces.
3. `looksLikeSQL` gains a word-boundary requirement and `extractSQLAtOffset` a
   printability requirement — both *shrink* what the loose scan accepts.
   `findSQLInPayload` gains `TRUNCATE`/`GRANT`/`REVOKE` (item 2) under the same
   word-boundary rule, which removes more false positives than the three verbs
   add.
4. `stapledStatement` becomes `stapledStatements` and `gateUnnameableFrame`
   gates **every** statement a frame carries, not the first (the preferred
   branch of the resolved question).
5. Item 5's carve-out (the anchor counting offset 0) stays, and the doc records
   why: the precise decode makes it cost less, but a `11 69` frame really is an
   exec and declining to look would forward a live statement ungated.

### Known consequence, flagged for the owner

Correct extraction means `ALTER SESSION SET …` — which DBeaver and SQL
Developer send during connection setup — is now seen by the gate as the `ALTER`
it is. Under a `read_only` or `block_ddl` grant those sessions will start being
refused where the fragment used to slip through. That is the gate working, not a
regression, but it is a live behaviour change on the most common GUI client and
is filed as its own todo.
