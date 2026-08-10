# MCP over the Oracle proxy fails upstream auth with `ORA-03120`

**RESOLVED 2026-08-10** — root-caused and fixed; kept here only until the
coordinator archives it. See "Resolution" below.

**No GitHub issue filed yet — one should be.**

## Goal

Make `TestIntegration_MCPExecutesThroughTheProxy`
(`internal/proxy/oracle/integration_test.go`) pass — i.e. make an MCP agent's
statement actually reach Oracle through dbbat's own listener, the way the
feature claims it does.

## Why

The test landed with `6f9de33 feat(mcp): execute Oracle statements over the
loopback proxy` and had never been green. Against `gvenzl/oracle-free:23-slim`
it failed at the first `Execute`:

```
upstream auth failed: upstream auth failed: upstream AUTH Phase 1 rejected:
ORA-03120 ORA-03120: two-task conversion routine: integer overflow
```

It had a second, unrelated bug in front of it, fixed first: the fixture created
the dbbat user as `SYSTEM`, while the proxy lowercases the wire username before
looking it up (`session.authenticateClient`), so client auth failed with
`user not found: SYSTEM` before the upstream leg was ever reached.

## Resolution

**Not a capability mismatch.** The pre-auth relay was byte-for-byte correct: a
recording TCP relay in front of the same container captured a working direct
`go-ora` v3 login, and its Set Protocol / Set Data Types / data-negotiation
exchange (161→127, 28→261, 2766→2788 bytes) is identical to what the relay
forwarded. The upstream socket state at the AUTH boundary was right.

The bug was in `findUserIDLenPos` (`internal/proxy/oracle/phase1_forward.go`).
A thin client writes `[01 01 <numPairs> 01 01]` immediately before the login
username in AUTH Phase 1, and go-ora's `numPairs` is 5. The thin branch scanned
*backward* from the username for the first byte equal to the old username
length — so a 5-character login name matched that pair count, which sits nearer
the username than the real `user_id_len`, and the splice bumped the number of KV
pairs while leaving the length stale. Diffing the outgoing packet against the
working capture showed it directly:

```
working (user "system"):  03 76 01 00 01 01 06 ... 01 01 05 01 01 06 "system"
dbbat   (user "agent"):   03 76 01 00 01 01 05 ... 01 01 06 01 01 06 "SYSTEM"
                                          ^^ stale        ^^ wrongly bumped
```

The wide (OCI) branch of the same function already documents this exact trap and
anchors instead of scanning, citing the 5-char `admin` case; only the thin
branch still did the condemned scan. Fixed by stepping over the
`[01 01 <numPairs> 01 01]` block before scanning, with two regression tests
(`TestRewriteAuthPhase1Username_PairCountCollision` and
`..._ObservedGoOraCapture`, the latter over the real captured go-ora v3 bytes).

**Scope beyond MCP:** this broke *any* go-ora / python-oracledb thin login whose
dbbat username is exactly 5 characters — including `admin`, dbbat's own default
user. The MCP test only surfaced it because its fixture user is `agent`.

### Ruled out along the way (do not re-litigate)

- **go-ora's 23ai "fast login"** — not in play at all. `FAST LOGIN=FALSE` on the
  MCP connect string produced a byte-identical client Phase 1 (verified from the
  proxy's own hex logging, not from the error text). The reason is upstream of
  the option: `stripAcceptModernAuthFlags` already clears `FAST_AUTH` and
  `HAS_END_OF_RESPONSE` from the Accept forwarded to the client, so the client
  never negotiates it.
- **The Phase 1 rewrite being skipped entirely** — forcing the synthetic
  `buildClientAuthPhase1` fallback failed identically, which is what proved the
  problem was a field value rather than the rewrite mechanism. (That fallback is
  separately wrong; filed as
  `2026-08-10-oracle-synthetic-phase1-preamble-drift.md`.)
- The `customHash` strip and `session.upstreamCustomHash` were correct
  (`custom_hash=true verifier_type=18453` in the logs, and Phase 2 succeeded
  once Phase 1 did).
