# Upgrade legacy per-key-salt O5LOGON verifiers on successful login

## Goal
Migrate API keys created before the per-user-salt scheme (`OracleAPIKeyData.user_salt` absent/false) to user-salt verifiers without forcing key rotation.

## Why
Users whose keys all predate the per-user-salt scheme keep the old single-key Oracle limitation until they create a new key. On a successful non-empty-`AUTH_PASSWORD` login the proxy briefly holds the plaintext API key (the decrypted `AUTH_PASSWORD`), which is exactly what is needed to re-derive the verifiers from the user's shared salts.

## Implementation
- In `internal/proxy/oracle/session.go` `resolveAPIKeyFromPhase2`, after `VerifyAPIKey` succeeds for a legacy key (`OracleData().UserSalt == false`), call a new store method (e.g. `Store.UpgradeAPIKeyO5LogonVerifiers(ctx, keyID, plainKey, encryptionKey)`) that runs `EnsureUserOracleSalts` + `computeO5LogonVerifierWithSalts` and persists the updated `protocol_data` — asynchronously, best-effort (like `IncrementAPIKeyUsage`).
- Add a store test: legacy key logs in → key becomes `user_salt: true` with salts equal to the user's.
- No GitHub issue filed yet — file one when picking this up.

## Implementation Plan

1. **Store method `UpgradeAPIKeyO5LogonVerifiers(ctx, keyID, plainKey, encryptionKey)`**
   (`internal/store/api_keys.go`): load the key by ID; no-op if the encryption
   key is empty or the key already uses user salts (`OracleData().UserSalt`);
   otherwise `EnsureUserOracleSalts` for the owner, re-derive the 6949 + 18453
   verifiers from the plaintext + the user's shared salts via
   `computeO5LogonVerifierWithSalts` (which also flips `UserSalt = true`), and
   persist the refreshed `protocol_data` jsonb with an UPDATE on `id`. Best-effort
   / idempotent: a second login on an already-upgraded key is a cheap no-op.
2. **Proxy hook** (`internal/proxy/oracle/session.go`, `resolveAPIKeyFromPhase2`,
   non-empty-`AUTH_PASSWORD` branch): right after `VerifyAPIKey` succeeds, if the
   winning key is legacy (`OracleData().UserSalt == false`), fire the upgrade in a
   background goroutine (like `IncrementAPIKeyUsage`) using the plaintext
   (`plainPassword`) the proxy still holds and `s.encryptionKey`. Failures are
   logged, never surfaced to the client. The empty-password path can't upgrade
   (no plaintext) and is left untouched.
3. **Store test** (`internal/store/api_keys_test.go`): create a key, downgrade it
   to legacy per-key salts to simulate a pre-user-salt key, call
   `UpgradeAPIKeyO5LogonVerifiers`, and assert the reloaded key is `user_salt:
   true` with salts equal to the user's shared salts and verifiers that re-derive
   from the plaintext + user salts. Also assert idempotency (second call is a
   no-op and leaves the key unchanged).
