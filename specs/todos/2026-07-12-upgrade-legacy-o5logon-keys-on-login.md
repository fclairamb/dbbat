# Upgrade legacy per-key-salt O5LOGON verifiers on successful login

## Goal
Migrate API keys created before the per-user-salt scheme (`OracleAPIKeyData.user_salt` absent/false) to user-salt verifiers without forcing key rotation.

## Why
Users whose keys all predate the per-user-salt scheme keep the old single-key Oracle limitation until they create a new key. On a successful non-empty-`AUTH_PASSWORD` login the proxy briefly holds the plaintext API key (the decrypted `AUTH_PASSWORD`), which is exactly what is needed to re-derive the verifiers from the user's shared salts.

## Implementation
- In `internal/proxy/oracle/session.go` `resolveAPIKeyFromPhase2`, after `VerifyAPIKey` succeeds for a legacy key (`OracleData().UserSalt == false`), call a new store method (e.g. `Store.UpgradeAPIKeyO5LogonVerifiers(ctx, keyID, plainKey, encryptionKey)`) that runs `EnsureUserOracleSalts` + `computeO5LogonVerifierWithSalts` and persists the updated `protocol_data` — asynchronously, best-effort (like `IncrementAPIKeyUsage`).
- Add a store test: legacy key logs in → key becomes `user_salt: true` with salts equal to the user's.
- No GitHub issue filed yet — file one when picking this up.
