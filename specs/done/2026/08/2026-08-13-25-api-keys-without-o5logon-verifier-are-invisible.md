# An API key that cannot be used for Oracle looks exactly like a wrong password

## Goal

A user must be able to tell which of their API keys work for Oracle login, and
a key that cannot must say so rather than returning "invalid username/password".

## Why

Oracle login uses O5LOGON, so dbbat needs a verifier derived from the key.
API keys are Argon2id-hashed, so the verifier cannot be recovered after the
fact: it is stored (encrypted) at mint time. **Keys minted before that feature
shipped therefore authenticate fine against the REST API and can never be used
for Oracle** — and nothing anywhere says so.

Hit on 2026-08-13 while testing production. The key in `~/.zshenv`
(`dbb_3js0…`, created 2026-07-12) authenticates against the API — `GET
/api/v1/auth/me` returns the user, roles and all — but every Oracle connect
with it fails. The proxy log is unambiguous:

```
O5LOGON verifiers loaded — any of these API keys works for Oracle login
  candidates=3 primary_key_prefix=dbb_8kre has_18453=true
AUTH Phase 2: candidate did not decrypt AUTH_PASSWORD  key_prefix=dbb_8kre
AUTH Phase 2: candidate did not decrypt AUTH_PASSWORD  key_prefix=dbb_sjzu
AUTH Phase 2: candidate did not decrypt AUTH_PASSWORD  key_prefix=dbb_2417
WARN client authentication failed  error="API key verification failed: no candidate key decrypted AUTH_PASSWORD (3 tried)"
```

The three candidates are the user's *other*, newer keys; the one actually being
presented is not in the list at all, because it has no verifier. The client sees
`ORA-01017 invalid username/password` — indistinguishable from a typo, a
revoked key or a wrong username. `GET /api/v1/keys` returns
`key_prefix`/`name`/`created_at` and gives no hint either. The natural
conclusion is "my key is wrong", and the natural fix — mint a new key — happens
to work, which hides the cause and teaches nothing.

This wastes real time (it did here) and it will keep doing so: every key minted
before the verifier feature is a live trap, and `stn dbbat key` hands one out
without comment.

## Implementation

- **Surface it.** Add a boolean to the key model returned by
  `GET /api/v1/keys` — `oracle_capable` (or `protocols: ["api","oracle"]`) —
  computed from whether verifier data decrypts, the same predicate
  `decryptVerifierData` already applies in
  `internal/proxy/oracle/session.go:1140`. Show it in the UI key list and in
  `stn dbbat status`.
- **Say it at the point of failure.** When the presented key resolves to a
  known, non-revoked key that simply has no verifier, that is not "wrong
  password" — it is actionable, like `ErrNoActiveGrant` already is in
  `authRejectFor`. Give it its own message ("this API key predates Oracle
  support; mint a new one"). Note the proxy must be able to tell *which* key
  was presented to say this — today it only knows that no candidate decrypted.
  If that identification is not available at AUTH Phase 2, the listing half
  above is still worth shipping on its own.
- **Consider backfill-on-use**: a key presented over REST could have its
  verifier written then, since the plaintext is in hand at that moment. Weigh
  against the deliberate design that a leaked store yields no usable key —
  verifier data is encrypted, not hashed, so this widens what a store leak
  gives up. Probably not worth it; the listing + message is the honest fix.
- Depends on nothing; independent of
  `2026-08-13-21-oracle-auth-phase-refusal-breaks-python-oracledb-thin.md`,
  though that bug is what made this one hard to see (the ORA-01017 never even
  rendered — it arrived as DPY-5002).
