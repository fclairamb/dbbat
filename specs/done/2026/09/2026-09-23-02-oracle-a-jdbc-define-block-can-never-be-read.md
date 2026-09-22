---
model: opus
effort: low
---

# A JDBC define block can never be read, whatever it asks for

## Goal

Decide whether `defineTypeAgrees` should accept a client that *substitutes* a
scalar type rather than echoing the describe's — and if it should, measure the
substitution off recordings rather than off a rule.

## Why

Found on 2026-09-23 while implementing
`2026-09-22-11-oracle-jdbc-thin-has-no-lob-recording.md`.

`execDefineLOBShape` requires every define entry to declare the type the
describe gave, the one exception being a LOB re-declared as a LONG. The comment
on `defineTypeAgrees` states the reason: "a client echoes the describe back for
every column it is not changing, so anything other than the same code is a walk
that landed on the wrong bytes".

JDBC thin disproves the premise. Its define block for `goOraLOBQuery`
re-declares the seven ordinary **CHAR** columns (96) as **VARCHAR2** (1) — a
column it is not changing, spelled with a different code. `readDefineEntry`
walks its entries perfectly (measured on `testdata/jdbc_thin_lob.pcapng`: the
19-byte CHAR entry and the 23-byte CLOB entry both read field for field), so the
only thing refusing the frame is the type rule. Result:
`execDefineLOBShape` returns nothing on **every** JDBC thin session, whatever
that session asked for.

Today that costs nothing, and that is why this is a todo rather than a fix: what
JDBC asks for is locators, which is exactly what an unlearned session reads. But
it means a JDBC deployment that ever *does* ask for inlining — a driver version
that substitutes LONG, an application that defines its own columns — would be
silently refused, which is the failure mode the whole define reading exists to
end.

## Implementation

1. Decide the rule. The tight version is a **measured** substitution table
   alongside the LOB→LONG one: CHAR (96) → VARCHAR2 (1) is the only one any
   recording shows, and it is one-way (a VARCHAR2 column re-declared as CHAR is
   not something any client was seen doing). Anything broader re-opens the
   near-miss the exact-match rule closes.
2. If it is accepted, `internal/proxy/oracle/ttc_define.go`'s `defineTypeAgrees`
   gains that pair, and `TestDefineBlockNeedsEveryColumnToAgreeWithTheDescribe`
   gains both directions of it.
3. The corpus census in `TestDefineBlockIsNotFoundInFramesThatAreNotOne` then has
   to grow `jdbc_thin_lob.pcapng: 1`, and `TestJDBCThinDefineBlockIsNotReadAndDoesNotNeedToBe`
   in `lob_dialects_test.go` inverts into "reads it, and learns the locator" —
   which is the assertion that says the relaxation did not change any row.
4. Re-run the whole-corpus sweep. The rule is only worth relaxing if exactly the
   three define-carrying recordings still answer and nothing else starts to.

## Files

- `internal/proxy/oracle/ttc_define.go`
- `internal/proxy/oracle/ttc_define_test.go`, `lob_dialects_test.go`
- `docs/oracle.md` ("A thin client says which LOB reading it wants")
