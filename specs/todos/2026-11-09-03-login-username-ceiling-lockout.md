---
model: opus
effort: medium
pr: batch
automerge: false
---

# Login: the per-username failure ceiling is an account-lockout lever

## Problem

Found by the security scan of 2026-11-09 (finding M3). Severity: Medium.
Triggerable by an unauthenticated remote attacker with a handful of source
addresses (roughly 8+, since one address is held to ~12 recorded failures/hour
by the per-source backoff).

The login backoff shipped in `23516c0` added a per-username ceiling:
`authUsernameCeiling = 100` failures against one username within one hour,
summed over **every** source IP (`internal/api/middleware.go:270-276`). The
comment claims it is "a backstop against distributed guessing and not a lockout
lever." It is a lockout lever:

- `checkRateLimit` checks the ceiling **before any credential verification**
  (`internal/api/middleware.go:338-361`) and returns `blocked` for *every*
  source once `count >= 100`, including the legitimate user's correct-password
  request.
- A successful login never lowers the ceiling: `resetFailures`
  (`middleware.go:418-436`) clears only the per-`(username, ip)` record, not the
  `ceilings[username]` entry.
- The throttle gates `POST /auth/login`, `PUT /auth/password`, and both
  username-keyed password-change branches (`internal/api/auth.go:61, 213, 298,
  314`).

So an attacker who drives 100 failed attempts for `admin` (a predictable target)
across enough addresses within an hour blocks all password logins for `admin`
until `windowStart + 1h`, renewable indefinitely. Existing sessions and API keys
survive, and SSO/OIDC login is unaffected — but password login for the targeted
account is denied. (The tracker is per-process, so multi-replica deployments
enforce it per replica, which weakens both the attack and the backstop.)

## Proposal

1. Make a correct password pass regardless of the ceiling: verify credentials
   even when `count >= authUsernameCeiling`, and only apply the throttle to the
   response when the attempt **fails**. Concretely, split "should I slow this
   response down" from "should I refuse to even check the password": the ceiling
   should delay/annotate a *failed* attempt, not reject a request before the
   password is checked. The per-source backoff (`authBackoffDelays`) already
   does the slowing; the ceiling's job is to stop the *failure feedback* from
   being useful at scale, not to deny the real user.
   - Option A (recommended): keep the ceiling as a signal that raises the
     per-failure delay / triggers an audit event, but let the handler proceed to
     `VerifyPassword`; on success, `resetFailures` also clears the ceiling.
   - Option B: on success at or above the ceiling, bypass the block and clear
     the ceiling; only truly block when the credential is wrong.
2. Clear `ceilings[username]` in `resetFailures` (or on any successful login),
   so one correct login ends the lockout.
3. Keep the audit event the ceiling already emits so distributed guessing is
   still visible.

Pick Option A or B in the implementation; both restore "the real user can always
log in with the right password" while keeping the distributed-guessing backstop.

## Tests

- `internal/api/middleware_test.go` (or `ratelimit_test.go`): with the ceiling
  reached for a username, a request carrying the **correct** password succeeds
  (fails before the fix — currently blocked with 429/`retryAfter`).
- A request carrying a **wrong** password while the ceiling is reached is still
  throttled/blocked (positive control: the distributed-guessing backstop still
  works).
- After one successful login, the ceiling is cleared: a subsequent wrong
  password is throttled only by the per-source backoff, not the username
  ceiling.

## Open questions

- Should a correct password past the ceiling be allowed silently, or allowed
  **and** audited as "login succeeded during an active guessing burst"?
  Recommended: allow and audit (keeps the signal without locking the user out).
