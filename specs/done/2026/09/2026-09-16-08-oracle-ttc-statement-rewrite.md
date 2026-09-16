---
model: opus
effort: high
---

# The Oracle proxy cannot rewrite a statement, so the per-user tag has nowhere to go

## Goal

Give the Oracle proxy one safe way to forward a statement whose text differs
from the client's, and use it to prepend the per-user tag
(`shared.NewUserQueryTagger`) behind a new `DBB_QUERY_TAGGING_ORACLE=user`.

## Why

Spec `2026-09-16-07` measured the cost of the tag on a real 23ai instance and
closed with a number: a tag constant per dbbat **user** costs one shared-pool
cursor and ~48KB per identity, one hard parse apiece, and then plateaus — 600
further executions added zero loads and zero bytes. The same traffic under a
per-connection tag cost 200 cursors and 9.6MB, with no ceiling. So the coarse
tag is affordable and the fine one is not, which was the open question.

It was not implemented, because the injection point the spec assumed does not
exist. PostgreSQL, MySQL and MongoDB re-encode every message on the way
upstream, so tagging is one function (`upstreamText`,
`internal/proxy/mysql/intercept.go`). Oracle **decodes statements only to gate
and record them** and relays the client's own TNS packets byte for byte —
`reassembly.go` states it as an invariant: *"the reassembled buffer is for
reading only, and dbbat never synthesizes wire bytes toward the upstream."*

The tag is therefore the *occasion* for this work, not its substance. The
substance is a TTC statement writer, which the package does not have and which
anything else that ever needs to alter a statement will also need.

## Implementation

The injection point itself is not in doubt: `clientToUpstream`
(`internal/proxy/oracle/session.go`), between the `blocked` check and the
`for _, frag := range msg.packets` write loop. Every control, the `queries` row
and any approval hold have run on the client's text by then, and nothing has
left. What has to exist before that line can do anything is the writer.

Surface, all of it already decoded somewhere in the package (which is the one
piece of luck — the readings exist, only the inverse is missing):

- **Three statement-carrying ops, three SQL-length fields.** The v315+
  piggyback exec `03 5e` and the JDBC `11 69` declare it as a TTC compressed
  int, whose *encoded width* changes with the value — growing the statement can
  widen the field and shift every byte behind it. `OALL8` uses `decodeVarLen`
  (1 byte / `0xFE`+2BE / `0xFF`+4BE) and puts a 2-byte bind count and the bind
  values immediately **after** the text.
- **A second encoding for OCI clients** (sqlplus, SQL*Developer, Instant
  Client): `sqlLen * 3` as a little-endian ub4 behind the `fe x8` pointer
  sentinel, the x3 being the client's widest-charset buffer convention. OCI
  sometimes counts a trailing NUL in the value and sometimes does not
  (`locateExecSQLText` measured both).
- **CLR framing.** go-ora repeats the length as a raw byte just before the
  text. A statement crossing 252 bytes changes *format*, to the `0xFE`-chunked
  long form — and a ~40-byte tag is exactly what pushes a statement over. The
  chunk encoders exist (`encodeBigChunkCLRSplit`, `ttcClrVariant`,
  `clr_bigchunks.go`), and `s.clientBigClrChunks` says which variant the
  session negotiated.
- **TNS framing.** A rewritten packet must drop `pkt.Raw` (`writeTNSPacket`
  prefers it), and `encodeTNSPacket` writes only the legacy 2-byte length
  header — it cannot produce a v315+ data packet (4-byte length at `[0:4]`,
  2-byte field zeroed), which is what these sessions use. A writer for that
  form is a prerequisite.
- **Re-fragmentation.** A message already at the negotiated SDU cannot absorb
  40 more bytes in place. Rewriting after `collectStatementMessage` is what
  keeps the *shortfall* accounting (`execFragmentShortfall`,
  `oall8FragmentShortfall`) reading the client's declared length and therefore
  unaffected — but the outgoing packets still have to be re-cut.

**The hard constraint, and the thing to solve first.** `locateExecSQLText`
*searches* for a text run of the declared length: it accepts `sqlLen` or
`sqlLen-1`, tolerates a one-byte shift past a printable CLR prefix, and
validates by "looks like text and opens with a SQL verb". That is right for a
gate, which fails open to a scan when it is unsure. It is not a basis for
rewriting a length prefix — the documented failure mode of getting a TTC length
wrong, already met on the AUTH leg, is `ORA-03146 invalid buffer length for TTC
field`: a dead session, in exchange for a comment. So the first deliverable is a
**locator that returns exact offsets or refuses** — the offset of the length
field, its encoded width, the offset of the CLR prefix, the offset and extent of
the text — distinct from today's best-effort `decodeExecStatementText`, and
usable only when it is certain.

Then:

- Rewrite only when the exact locator answers, on the ops and encodings it
  covers. **Every other frame forwards untagged**, as today.
- **But not silently, and not per-frame.** Partial coverage is its own bug: a
  statement tagged on some executions and not others gets *both* a tagged and an
  untagged SQL_ID, doubling the cursor count the measurement was about. So the
  decision must be per **session**, taken once — if this session's client shape
  cannot be rewritten with certainty, the session runs untagged start to finish
  and logs it once, rather than tagging the frames that happen to parse.
- `shared.NewUserQueryTagger(version.Version, user.Username, grant.DefinitionSlug())`
  already exists and its bytes are pinned by tests: `/*dbbat='…',user='…',
  grant='…'*/ `, no `conn=`, ASCII only (dbbat does not know the session
  charset, so byte length must equal character length).
- Config: `DBB_QUERY_TAGGING_ORACLE`, values `off` (default) and `user`,
  **separate** from `DBB_QUERY_TAGGING` — the trade-off is Oracle-specific and
  an operator who turned tagging on for PostgreSQL did not consent to it.
  Deliberately not added by spec 07, so that no setting exists that parses and
  changes nothing.
- Cursor re-execution needs no work and should be checked rather than assumed:
  the re-exec frames carry a cursor id and no SQL, so the upstream cursor keeps
  whatever text the parse installed. Confirm that the tagged parse and the
  untagged `trackedCursor.sql` staying out of step breaks nothing in the
  tracker.

**Testing is the bulk of it.** The invariant the other three protocols assert —
the `queries` row, the audit chain and the `.pcapng` capture all hold the
*client's* text, only the wire carries the tag — plus, uniquely here, a
wire-level assertion that the rewritten frame is what Oracle actually accepts.
`make test-e2e-oracle` drives go-ora, JDBC thin, python-oracledb, sqlcl and
sqlplus (OCI) against a real 23ai container; the tag has to be green on every
one of them, and `V$SQL` should be queried in-suite to confirm the tag arrived
and that the cursor count is k rather than one per session. `testdata/` already
holds recordings of each client shape, so the exact locator can be unit-tested
against every frame in the corpus before any of it goes on a wire
(`sql_extraction_survey_test.go` is the model: it computes a per-shape verdict
across the whole corpus).

## Implementation Plan

Ordered so that nothing is written to a wire before the reading of that wire is
proven against the recorded corpus.

1. **Exact locator, read-only.** `ttc_rewrite_locate.go`: a `stmtLocation`
   naming the op offset, the SQL-length field's offset/width/encoding
   (compressed int, OCI wide `ub4 = sqlLen*3`, `decodeVarLen` for OALL8), the
   CLR blob's offset/extent/variant, the text's offset and extent, and whether
   the client counts a trailing NUL. Certainty comes from a **round-trip
   identity check**, not from a search heuristic: the locator re-encodes the
   length field and the CLR blob from the values it read and refuses unless the
   re-encoding reproduces the client's own bytes exactly. Anything ambiguous
   (two candidate CLR blobs, a chunk convention that does not re-encode,
   a header walk that does not land) refuses.
2. **Corpus verdict test** mirroring `sql_extraction_survey_test.go`: a
   per-recording, per-shape verdict over every `testdata/` frame, run before any
   rewrite code exists. A shape the locator cannot certify is reported as
   refused, never as a best-effort answer.
3. **Length-field writers.** `ttc_rewrite.go`: compressed int (whose encoded
   width changes with the value, so the splice must shift the tail), OCI wide
   `ub4 = len*3` little-endian (fixed width), OALL8 `decodeVarLen` (1 byte /
   `0xFE`+2BE / `0xFF`+4BE, width changes). Each writer is unit-tested for both
   "the tag fits the current width" and "the tag widens the field".
4. **CLR rewrite across the chunk boundary.** Short form under the limit, the
   `0xFE`-chunked long form at or past it, in the variant the session
   negotiated (`s.clientBigClrChunks`). The short→long crossing a ~40-byte tag
   provokes is an explicit, tested case.
5. **v315+ TNS packet writer.** `encodeTNSPacket` emits only the legacy 2-byte
   header; add the 4-byte-length form and make a rewritten packet carry no
   stale `Raw` (which `writeTNSPacket` prefers).
6. **Re-fragmentation.** The rewrite happens after `collectStatementMessage`,
   so `execFragmentShortfall` / `oall8FragmentShortfall` keep reading the
   client's own declared length untouched; only the outgoing packets are re-cut
   to the negotiated SDU.
7. **Per-session tag/no-tag decision.** One session-level state, decided once on
   the first statement-carrying frame and logged once. A session whose shape the
   locator cannot certify runs untagged start to finish.
8. **`clientToUpstream` injection**, between the `blocked` check and the
   `for _, frag := range msg.packets` write loop.
9. **Cursor re-execution**: a test proving a re-exec frame (cursor id, no SQL)
   is untouched and that the tracker does not care that `trackedCursor.sql`
   holds the client's text while the upstream cursor holds the tagged one.
10. **`DBB_QUERY_TAGGING_ORACLE`** (`off` default, `user`), separate from
    `DBB_QUERY_TAGGING`, same koanf pattern, invalid value = startup failure.
11. **Docs**: rewrite docs/oracle.md's "measured, affordable, and still not
    wired" section, plus website configuration reference.
12. **Integration pass**: `make test-e2e-oracle` across go-ora, JDBC thin,
    python-oracledb, sqlcl and sqlplus, with `V$SQL` queried in-suite.
