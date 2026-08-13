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

Key files: `internal/proxy/oracle/ttc_decode.go` (`decodeExecSQL`,
`findSQLInPayload`, `extractSQLAtOffset`), `internal/proxy/oracle/intercept.go`
(`handleJDBCExec`), `internal/proxy/oracle/session.go` (`stapledStatement`,
`statementOpOffsets`), `internal/proxy/shared/validation.go`.
