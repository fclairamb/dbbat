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

Key files: `internal/proxy/shared/validation.go` (`writeKeywords`,
`ddlKeywords`, `IsWriteQuery`, `IsDDLQuery`), `internal/proxy/oracle/intercept.go`
(`handleJDBCExec`, `handlePiggybackExec`), `internal/proxy/oracle/session.go`
(`gateUnnameableFrame`), `docs/oracle.md` ("SQL Extraction").
