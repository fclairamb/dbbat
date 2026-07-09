# Surface Oracle grant/auth denials as a clean error, not ORA-12566

## Goal
When the Oracle proxy rejects a login at the client-authentication or grant
stage (`ErrNoActiveGrant`, `ErrUserNotFound`, bad password), return a proper
Oracle auth error to the client — ideally `ORA-01017: invalid username/password`
or a dbbat-specific message — instead of tearing the socket down and letting the
client report a generic `ORA-12566: TNS:protocol error` / `ORA-03113`.

## Why
Verified live on 2026-07-10: a user who authenticates successfully but has no
active grant for the target database gets `ORA-12566: TNS:protocol error` at
sqlplus, while the server logs the real reason
(`no active grant for this user/database: user=FLORENT.CLAIRAMBAULT
database=abyla_glh`). The raw protocol error is indistinguishable from an actual
bug and gives the user no idea they simply need a grant. This directly cost
debugging time while diagnosing the dotted-username issue
([#235](https://github.com/fclairamb/dbbat/pull/235)).

## Implementation
- `internal/proxy/oracle/session.go:~544` — where `ErrNoActiveGrant` (and the
  client-auth failures around session.go:228) cause the session to end.
- Before closing, emit a well-formed TTC error/AUTH-reject frame carrying an
  Oracle error number the client will render, e.g. ORA-01017 (invalid
  credentials) or a refusal message the O5LOGON/OCI client surfaces cleanly.
  Check what the existing `buildAuthChallengeEndMarker` / error-frame helpers can
  emit, and what sqlplus + go-ora actually display for each candidate ORA code.
- Prefer a message that distinguishes "no grant" (actionable: request a grant)
  from "bad credentials" without leaking whether the user exists, consistent with
  how the PostgreSQL/MySQL proxies word their denials.
- Add a regression test asserting the reject frame carries the chosen ORA code
  rather than an abrupt EOF.

No originating GitHub issue yet — file one if picked up.
