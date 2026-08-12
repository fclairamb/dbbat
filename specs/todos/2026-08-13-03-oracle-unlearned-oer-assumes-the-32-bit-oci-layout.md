# A refusal written before any upstream OER assumes the 32-bit OCI layout

**No GitHub issue filed yet — one should be.** (Automation must not run
`gh issue create`; see `specs/todos/2026-08-11-06-*.md`.)

## Goal

Make dbbat's *unlearned* refusal frame right for a 64-bit OCI client, so a
session refused on its literal first statement gets an ORA error rather than a
frame it cannot parse.

## Why

`specs/done/.../2026-08-12-12-bundled-oci-client-refused-and-hung-under-a-restrictive-grant.md`
taught `learnOERShape` a second fixed-width layout (`oerFixed64Layout`), because
the DB-bundled OCI client — sqlplus 23.26, inside `gvenzl/oracle-free:23-slim` —
marshals the summary object at 64-bit widths and hangs on the 32-bit one.

Learning happens off the upstream's own OERs, and every OCI session measured
issues statements at login, so the shape is known well before any refusal. But
`nextOERFrame`'s unlearned fallback still seeds only `fixedWidth` (from
`s.clientWideEncoding`) and leaves `fixedWidth64` false:

```go
shape := s.oer.orDefault()
if !shape.tailLearned {
    shape.fixedWidth = s.clientWideEncoding
    shape.endOfResponse = s.clientWideEncoding
}
```

A 64-bit client whose *first* statement is refused — a grant with an approval
pattern matching it, an exhausted quota, a `read_only` grant and a client that
opens with a write — therefore gets the narrow frame and hangs, which is exactly
the symptom that spec spent three fixes eliminating. It is a narrower window
than the one that was closed, not a closed one.

## Implementation

- The client's own AUTH framing already distinguishes the two flavors: the
  bundled client writes 8-byte integers in Phase 1 where the Instant Client
  writes 4-byte ones (see the recorded preambles in `oci_instantclient_test.go`
  and `docs/oracle.md`, "Two OCI encodings, not one"). Derive a
  `clientWide64Encoding` alongside `payloadUsesWideKVEncoding` and seed
  `shape.fixedWidth64` from it in `nextOERFrame`, the same way `fixedWidth` is
  seeded today.
- Pin it from bytes: `testdata/oci_bundled_first_call.hex` and the AUTH
  fixtures are already in the tree, and the recorded 64-bit summaries are in
  `testdata/oci_bundled_oer.hex`.
- The live half needs a session that refuses before the upstream has sent an
  OER. `TestIntegration_BlockedStatementRefusesSQLPlus` cannot show it — sqlplus
  runs its own login SELECTs first — so drive it with a grant whose approval
  pattern (or `max_query_counts` of 0) refuses the session's opening statement.

Key files: `internal/proxy/oracle/session.go` (`nextOERFrame`),
`internal/proxy/oracle/ttc_oer_encode.go` (`oerFixed64Layout`),
`internal/proxy/oracle/phase1_forward.go` (`payloadUsesWideKVEncoding`).
