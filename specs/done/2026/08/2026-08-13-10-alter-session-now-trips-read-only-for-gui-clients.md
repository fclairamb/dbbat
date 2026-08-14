# `ALTER SESSION SET …` now trips `read_only` and `block_ddl` for GUI clients

**No GitHub issue filed yet — one should be.** (Automation must not run
`gh issue create`; see `specs/todos/2026-08-11-06-*.md`.)

## Goal

Decide what a connection-setup `ALTER SESSION SET …` should mean to the Oracle
statement gate, now that the gate can actually see it.

## Why

Until the 2026-08-13-05 fix, the Oracle SQL extractor returned a mid-statement
*fragment* for 48 of the 137 execute ops in `internal/proxy/oracle/testdata/`.
Nine of them — five distinct statements, all from DBeaver's connection setup in
`dbeaver.pcapng` and `dbeaver_init.pcapng`, computed and asserted by
`TestSurveyAlterSessionMisreadAsSet` — were this:

```
on the wire:  ALTER SESSION SET CURRENT_SCHEMA=TESTADM
the gate saw: SET CURRENT_SCHEMA=TESTADM
```

**Before grepping the recordings, read this.** `ALTER SESSION` appears in all 22
files, and almost every occurrence is *not* a statement: it is the
`AUTH_ALTER_SESSION` key/value inside the client's phase-2 AUTH message — dbbat
emits the identical shape itself (`upstream_auth_client_wide.go`) and the key is
in the known set (`ttc_auth.go`). AUTH is not a statement-carrying op: it is func
`0x03` with sub-ops `0x76` and `0x73` (`ttc.go`, `PiggybackSubAuth1`), so it
never reaches the statement gate and those occurrences are an authentication
attribute the gate does not and should not see. The nine above are the only ones that were ever
gated. A reviewer grepped the corpus, found the AUTH occurrences and concluded
thin clients were already being refused; they never were.

`ALTER` is in both `writeKeywords` and `ddlKeywords`
(`internal/proxy/shared/validation.go`); `SET` is in neither. So under a
`read_only` or `block_ddl` grant these statements *passed*, and `/queries`
recorded the fragment. That was a bypass, and the extractor now reads the
statement the header declares — which means the same statements are refused.

DBeaver sends several of them (`CURRENT_SCHEMA`, a run of `_optimizer_*` hints,
`OPTIMIZER_FEATURES_ENABLE`) while a connection is being established, before the
user has run anything. SQL Developer over the OCI driver is expected to behave
the same way but is **not** in the corpus, so that is inference rather than
measurement. The practical effect is that a
GUI client may now **fail to connect at all** under a read-only grant, where it
used to connect and then be correctly refused on any real write.

Nobody has decided that this is what should happen — it fell out of fixing the
extractor, and correctness said not to paper over it in the same change.

## Implementation

Three options, in increasing order of nuance. Whichever is chosen, it must be
decided on the *statement*, never by going back to a looser extractor.

1. **Leave it.** `ALTER SESSION` really is a session-altering DDL verb and a
   read-only grant refusing it is defensible. Cost: a support burden on the most
   common client, and the refusal is a session teardown when the frame is
   unnameable (`gateUnnameableFrame`).
2. **Treat `ALTER SESSION SET <session parameter>` as neither a write nor DDL.**
   It changes nothing durable — no data, no schema — and Oracle itself scopes it
   to the session. This is the narrowest carve-out that unblocks GUI clients, and
   it would live in `shared.IsWriteQuery` / `shared.IsDDLQuery` next to the
   existing `IsPasswordChangeQuery` special case. Must stay narrow: `ALTER
   SESSION SET CONTAINER` switches PDB and `ALTER SYSTEM` is already blocked
   outright, so the allowance should be a parameter allowlist rather than a
   prefix match.
3. **Make it a grant control.** An `allow_session_settings` control, off by
   default, so an operator opts in per grant definition.

Measure before choosing: `internal/proxy/oracle/sql_extraction_survey_test.go`
already enumerates every statement in the corpus, so listing the distinct
`ALTER SESSION` parameters real clients set is a few lines on top of it.

## Resolved open questions

> Decide what a connection-setup `ALTER SESSION SET …` should mean to the Oracle
> statement gate, now that the gate can actually see it. [Three options, in
> increasing order of nuance.]

**Decision (2026-08-13): take option 2 — a narrow parameter allowlist.** Treat
`ALTER SESSION SET <allowlisted parameter>` as neither a write nor DDL, in
`shared.IsWriteQuery` / `shared.IsDDLQuery` next to the existing
`IsPasswordChangeQuery` special case. Do **not** take option 1 (leave it) and do
**not** add a grant control (option 3).

Binding constraints on that allowlist:

- **Allowlist by parameter name, never by prefix.** `ALTER SESSION SET …` must
  not be matched as a prefix. Each permitted parameter is enumerated, and
  anything not enumerated keeps today's behaviour (refused under `read_only` /
  `block_ddl`).
- **`CONTAINER` is excluded, explicitly and with a comment saying why.**
  `ALTER SESSION SET CONTAINER` switches PDB, and a grant is scoped to a
  database — allowing it would let a session step outside the database its grant
  covers. This is the reason the allowlist is a list and not a prefix match; say
  so where the list is defined. `ALTER SYSTEM` is already blocked outright and
  stays that way.
- **A multi-parameter statement is allowed only if *every* parameter it sets is
  on the list.** `ALTER SESSION SET CURRENT_SCHEMA=X CONTAINER=Y` must be
  refused, not partially honoured. If the parameters cannot be parsed with
  confidence, refuse — fail closed.
- **Quoted underscore parameters count.** The corpus carries
  `ALTER SESSION SET "_optimizer_squ_bottomup" = FALSE`; the matcher must handle
  the double-quoted form and the spacing around `=`.
- **The statement is still recorded.** This changes classification, not
  visibility: an allowed `ALTER SESSION` must still appear in `/queries` like any
  other statement. It is being reclassified as not-a-write, not hidden.
- **The carve-out applies to both controls**, since it is a classification
  change: `read_only` and `block_ddl` both stop refusing an allowlisted
  `ALTER SESSION`.

**Seed measurement, already done — the corpus was enumerated on 2026-08-13 and
these five are the entire set of `ALTER SESSION` statements that were ever
gated** (the `TIME_ZONE`/`NLS_*` occurrence is the `AUTH_ALTER_SESSION`
attribute described above, not a statement):

```
ALTER SESSION SET CURRENT_SCHEMA=TESTADM
ALTER SESSION SET OPTIMIZER_FEATURES_ENABLE='10.2.0.5'
ALTER SESSION SET "_optimizer_cost_based_transformation" = 'OFF'
ALTER SESSION SET "_optimizer_push_pred_cost_based" = FALSE
ALTER SESSION SET "_optimizer_squ_bottomup" = FALSE
```

Those five are the floor the allowlist must cover. Extending it to the adjacent
session-scoped, non-durable settings real clients also set (`NLS_*`, `TIME_ZONE`)
is sanctioned and expected — but **enumerate each one and justify it in a
comment**; do not reach for a family wildcard beyond the `_optimizer_*` case, and
if you do allow `_optimizer_*` as a family, say why that family is safe when a
wildcard generally is not.

Pin the behaviour on both sides: an allowlisted parameter passes under
`read_only`, and `CONTAINER` plus at least one unenumerated parameter are still
refused. Extending
`internal/proxy/oracle/sql_extraction_survey_test.go` to assert the corpus's
distinct `ALTER SESSION` parameters keeps the floor honest if a recording is
ever added.

Key files: `internal/proxy/shared/validation.go` (`writeKeywords`,
`ddlKeywords`, `IsWriteQuery`, `IsDDLQuery`), `internal/proxy/oracle/intercept.go`
(`handleJDBCExec`, `handlePiggybackExec`), `internal/proxy/oracle/session.go`
(`gateUnnameableFrame`), `docs/oracle.md` ("SQL Extraction").

## Implementation Plan

1. **`internal/proxy/shared/validation.go` — the allowlist and the parser.**
   Add `IsAllowedAlterSession(sql string) bool`, exported so the Oracle corpus
   survey can assert the recorded statements against the very list the gate
   uses. It is consulted by `IsWriteQuery` and `IsDDLQuery` — one carve-out
   serving both controls, next to `IsPasswordChangeQuery`.

   The parser is a hand-rolled scanner, not a regexp, because the decision is
   per *parameter* and a regexp over the whole statement cannot express "every
   parameter must be on the list":
   - require the literal `ALTER SESSION SET` prefix at a word boundary
     (case-insensitive); anything else — `ALTER SESSION ENABLE …`,
     `ALTER SESSION CLOSE DATABASE LINK …`, `ALTER SYSTEM …` — is not this
     statement and keeps today's behaviour;
   - then loop over `name = value` pairs: a name is either a bare identifier or
     a double-quoted one (`"_optimizer_squ_bottomup"`), `=` may be surrounded
     by spaces, and a value is a single-quoted string, a double-quoted
     identifier, or a bare token restricted to a safe charset;
   - **every** parsed name must be on the allowlist, and the scan must consume
     the whole statement. Zero parameters, an unparseable byte, a stray `;`
     stapling a second statement — all return `false`, i.e. today's refusal.
     Fail closed is the default branch of every step.

2. **The list itself, enumerated with a per-entry justification**, and a comment
   at its head saying why it is a list and not a prefix match: `CONTAINER`
   switches PDB and a dbbat grant is scoped to a database, so an
   `ALTER SESSION SET CONTAINER` would step the session outside what its grant
   covers. Floor = the five statements measured in the corpus; plus the
   session-scoped, non-durable `NLS_*` and `TIME_ZONE` settings real clients
   set, each named individually. `_OPTIMIZER_*` is the single family rule, with
   its justification written where it is applied.

3. **Tests, both sides.** `internal/proxy/shared/validation_test.go`: the five
   corpus statements pass under `read_only` *and* `block_ddl`; `CONTAINER`,
   an unenumerated parameter, a multi-parameter statement mixing an allowed one
   with `CONTAINER`, a non-`SET` `ALTER SESSION` and malformed input are all
   still refused; `ALTER TABLE` / `ALTER USER` / `ALTER SYSTEM` and the other
   dialects' statements classify exactly as before.

4. **`internal/proxy/oracle/sql_extraction_survey_test.go`**: assert the exact
   set of distinct `ALTER SESSION` statements the corpus carries and that each
   one is `shared.IsAllowedAlterSession` — so a re-recording that adds a
   parameter fails loudly instead of silently reintroducing the connect-time
   refusal.

5. **`docs/oracle.md`**: rewrite the "A consequence worth knowing about"
   paragraph, which currently ends by pointing at this spec as unresolved.
