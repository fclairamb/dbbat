# Widen the synthetic Oracle AUTH fallback to cover wide/OCI clients

**No GitHub issue filed yet — one should be.**

## Goal

The synthetic AUTH builders that back the `DBBAT_ORACLE_FORCE_SYNTHETIC_AUTH`
fallback (`internal/proxy/oracle`) only emit the thin (compressed) KV encoding
and a go-ora-shaped KV set. A wide/OCI client (sqlplus, Instant Client)
whose own AUTH packet couldn't be rewritten would still be handed a body its
caps-conditioned upstream cannot parse. The fallback is currently a safety
net for thin clients only, not a general-purpose one.

## Why

`.github/workflows/integration.yml` now runs the Oracle suite a third time
with `DBBAT_ORACLE_FORCE_SYNTHETIC_AUTH=1` (see
`specs/todos/2026-08-11-01-ci-run-oracle-suite-with-the-auth-rewrite-off.md`
and `docs/oracle.md`, "Exercising the synthetic AUTH fallback"), so the
synthetic builders' thin-client behavior can no longer rot unnoticed. But that
forced run only proves the thin path; it says nothing about a wide/OCI client
falling back to the synthetic builders, because the suite has no such client
and the builders don't emit a body one could parse. That gap is real but was
explicitly out of scope for the CI-wiring spec.

## Implementation

- Inventory what a wide/OCI AUTH phase1/phase2 body actually needs (caps,
  session-key material, KV entries) beyond what the thin/go-ora encoding
  carries — compare against a captured sqlplus/Instant Client login.
- Extend the synthetic builders in `internal/proxy/oracle` (near
  `buildClientAuthPhase1` / `buildClientAuthPhase2`) to emit a wide-compatible
  body, or document why a single synthetic shape can't serve both and a
  second builder path is needed instead.
- Add integration coverage exercising a wide/OCI-shaped session through the
  forced-synthetic path, alongside the existing thin-client tests in
  `internal/proxy/oracle/synthetic_auth_integration_test.go`.
- Update `docs/oracle.md` ("Exercising the synthetic AUTH fallback") once the
  limitation is closed — it currently states the fallback does not cover
  wide/OCI clients.

Key files: `internal/proxy/oracle/synthetic_auth_integration_test.go`,
`internal/proxy/oracle` (AUTH phase1/phase2 synthetic builders),
`docs/oracle.md`.
