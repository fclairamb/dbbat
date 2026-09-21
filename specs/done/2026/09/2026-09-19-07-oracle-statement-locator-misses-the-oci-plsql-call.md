---
model: opus
effort: medium
---

# The exact statement locator cannot certify an OCI PL/SQL call carrying a bind

## Goal

Locate the statement in the one frame shape the corpus now knows it cannot:
sqlplus executing an anonymous PL/SQL block with a bind variable, stapled behind
a close-cursors piggyback in the wide/OCI encoding. Today that session runs
**untagged** — `DBB_QUERY_TAGGING_ORACLE=user` silently does nothing for it —
because `ttc_statement_rewrite.go` refuses any frame it cannot relocate to the
byte.

## Why

Found on 2026-09-19 while recording REF-cursor fixtures for
`2026-09-19-05-oracle-learn-ref-cursor-ids-from-out-binds.md`. Driving

```sql
VARIABLE rc REFCURSOR
BEGIN dbbat_cap_refcur(:rc); END;
/
```

through sqlplus produces a frame the locator does not answer for, while every
other frame in that same session — and every `11 69`-stapled execute in
`testdata/sqlplus_cursor_reexec.pcapng` — locates cleanly. Two of them in one
short session, so it is a shape rather than a one-off.

The refusal is the design working as intended (a statement tagged on some
executions and not others would get two SQL_IDs, which is the whole reason the
decision is per session and per frame), and nothing is mis-tagged. What is lost
is attribution: a thick-client session doing PL/SQL with binds — which is most
of what a DBA does from sqlplus — is invisible to `V$SQL` as a dbbat user.

The header looks like the shape the rewriter already knows:

```
11 69 0c 01 0d  fe ff ff ff ff ff ff ff   close-cursors, wide
01 00 00 00 02 00 00 00
03 5e 0d 01 0e  29 04 04 00  00 00 00 00  fe ff ff ff ff ff ff ff …
```

— the same `0x01`/`seq+1` pad and `fe ff …` sentinel `decodeCloseCursors`
documents — so the divergence is further in, most likely in how the statement's
length and CLR framing are expressed for this op. It has **not** been diagnosed;
that is step 1.

Because of it, the recording is deliberately **not** in `testdata/*.pcapng`:
`TestSurveyStatementRewriteCorpus` holds that corpus to 100% located, and
lowering a standing invariant to accommodate a newly recorded shape would hide
exactly the regression it exists to catch. The bytes live in
`internal/proxy/oracle/testdata/oci_refcursor_bind_output.hex` only as *server*
responses; the client frames are regenerated with
`go test -tags capture -run TestCapture_SQLPlusRefCursor ./internal/proxy/oracle/`,
which writes a full recording to a temp directory.

## Implementation

1. **Diagnose before changing anything.** Record the session, dump the refused
   `03 5e` frame, and find where the statement's length field, width, encoding
   and CLR framing differ from the `11 69`-stapled executes that already locate
   (`sqlplus_cursor_reexec.pcapng`, client frames 9/12/14/16). The bind is the
   obvious suspect; whether it is the bind *count* field or the statement length
   ahead of it is the thing to establish.
2. Extend `locateStatement` only once the layout re-encodes to the client's own
   bytes — the identity + round-trip proof the survey already applies is the
   acceptance test, not an extra.
3. Then add the recording to `testdata/` as an ordinary corpus fixture, which is
   what makes the survey's 100% mean more than it does today rather than less.

## Files

- `internal/proxy/oracle/ttc_statement_rewrite.go` — the locator
- `internal/proxy/oracle/ttc_statement_rewrite_survey_test.go` — the 100% floor
- `internal/proxy/oracle/capture_refcursor_test.go` — how to record the session
- `docs/oracle.md` — the Oracle tagging section
