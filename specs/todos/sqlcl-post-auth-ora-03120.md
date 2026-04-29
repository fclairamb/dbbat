# SQLcl post-auth ORA-03120

## Goal

Make the first user query through SQLcl 26.1 reach the upstream Oracle and return rows. Today auth completes (PBKDF2 path), the proxy enters relay mode, and Oracle replies to the very first query with two TNS Marker (interrupt) packets followed by an OER carrying:

> ORA-03120: two-task conversion routine: integer overflow

This is the next blocker after `sqlcl-client-validation.md`'s auth gap, which is now resolved.

## Symptom on the wire

Captured 2026-04-29 via `DBB_DUMP_DIR=/tmp/dbbat-dumps`. First post-AUTH packet, SQLcl → upstream, 469 bytes:

```
0:  00 00 01 d5 06 00 00 00 08 00 03 5e 03 02 80 21 00 01 02 01 83 01 01 0d 00 00 04 ff ff ff ff 01
32: 0a 04 7f ff ff ff 00 00 00 00 00 00 00 00 00 00 01 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00
64: 00 20 73 65 6c 65 63 74 ...   "select parameter,value from nls_session_parameters union all ..."
```

Upstream response sequence:

```
#2  S→C 11 bytes  0000000b 0c200000 010001        ← Marker (interrupt)
#3  S→C 11 bytes  0000000b 0c200000 010002        ← Marker (reset)
#4  C→S 11 bytes                                   ← SQLcl marker reply
#5  S→C 105 bytes containing "ORA-03120: two-task conversion routine: integer overflow"
```

Markers mean Oracle is interrupting *mid-parse*, not refusing on auth state. python-oracledb thin works fine through the same dbbat instance.

## Root cause

TNS framing is identical to python-oracledb. There is **no** extra `0x00` byte before the TTC function code, and the data flag (`0x0800`, `TNS_DATA_FLAGS_END_OF_REQUEST`) is a documented value. The earlier hypothesis about a "JDBC 0x0008 framing extension" was based on a hex-column-boundary misread and turned out to be wrong — it's recorded so we don't go down that path again.

The real divergence is **inside the OALL8 piggyback body**, reflecting different TTC compile-time caps:

| Field                | python-oracledb | SQLcl (JDBC thin 23.x)  |
|----------------------|-----------------|-------------------------|
| Cursor option byte   | `0x61`          | `0x21`                  |
| Options/flags run    | `01 18 01 01`   | `02 01 83 01 01`        |
| SQL length encoding  | 1-byte `0x18`   | 2-byte `00 20`          |

Why upstream chokes: the pre-auth relay forwards Connect / Accept / Set Protocol / Set Data Types directly between SQLcl and the real upstream so SQLcl gets responses tailored to its own caps. Then dbbat **closes** that socket and `upstreamGoOraAuth` opens a fresh go-ora connection, which negotiates `CompileTimeCaps[7]=11` from `data_type_nego.go`. The go-ora-negotiated upstream socket is at a different cap level than the one SQLcl thinks it's talking to, so SQLcl's OALL8 encoding doesn't parse and Oracle returns ORA-03120 on the first query.

## Paths forward

Each is multi-day; pick one.

### A. Carry the relay-phase upstream socket through AUTH (preferred)

Don't close `upstream` in `relayPreAuthNegotiation`. After dbbat completes O5LOGON with the client (using the API key), inject AUTH Phase 1 / 2 toward the *same* upstream socket using stored DB credentials. Caps stay aligned automatically — Oracle parses SQLcl's bytes the way SQLcl encoded them.

Cost: implement an O5LOGON CLIENT in dbbat that can drive an existing TNS connection, including the PBKDF2 (verifier 18453) path and the `customHash` capability bit. `go-ora/v2/auth_object.go` is the canonical reference.

### B. Transcode OALL8 piggyback bodies in `proxyMessages`

Decode SQLcl's OALL8 payload, re-encode in go-ora's lower-cap format on the way to upstream, do the inverse for responses. Most code by far — needs a full OALL8/QueryResult encoder/decoder for both directions, plus `OFETCH`, `OCLOSE`, continuation packets, error responses, etc.

### C. Force go-ora to advertise SQLcl's caps

Would only work if (1) we can observe SQLcl's negotiated caps during the relay and (2) go-ora exposes a way to override its `CompileTimeCaps` before opening. Neither is currently true; option C is on the list for completeness but is the weakest.

## Acceptance criteria

- [ ] `SELECT 1 FROM DUAL` from SQLcl 26.1 through dbbat returns the row.
- [ ] `SELECT * FROM <table>` returns expected data (correctness across types, NULLs).
- [ ] dbbat `/api/v1/queries` records the SQL text (no longer truncated to 32 bytes).
- [ ] The Oracle compatibility table in `docs/oracle.md` updated for SQLcl.
- [ ] python-oracledb thin still works (no regression on the working path).

## Artifacts

- `/tmp/dbbat-dumps/019dd9e0-fce1-78a2-8e25-5e5b204d06a7.dbbat-dump` — SQLcl 26.1 session this turn.
- `/tmp/sqlcl-final-tap.log` — earlier SQLcl tap.
- `/tmp/pyora-tap.log` — python-oracledb working reference for comparison.
- `/tmp/parse_dump.py` — working v2 dump parser (the in-repo `scripts/replay_dump.py` skips the JSON-header-length read and crashes on v2 dumps; that's a separate one-line fix).

## Related

- `specs/todos/sqlcl-client-validation.md` — predecessor blocker (auth itself); resolved 2026-04-29 by the PBKDF2 / customHash work in `43ef6ea` and `aecb032`.
- `~/.claude/projects/-Users-florent-code-fclairamb-dbbat/memory/feedback_oracle_pbkdf2.md` — PBKDF2 reference notes.
- `~/.claude/projects/-Users-florent-code-fclairamb-dbbat/memory/feedback_sqlcl_post_auth.md` — short-form summary of the misread + correct diagnosis.
