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

## Implementation Plan

### Root cause
`session.run()` already calls `s.sendAuthFailed(ORA01017, …)` for every
`authenticateClient` failure (grant, user-not-found, bad password). The frame
never reaches the client cleanly because `sendAuthFailed` writes the packet via
`writeTNSPacket` → `encodeTNSPacket`, which uses the **legacy 2-byte** TNS length
header. After the TNS Accept, v315+ clients read the length as a **4-byte** field
(the 2-byte field must be `0x0000`). A 2-byte-framed reject is therefore misread
as a malformed/oversized packet → the client surfaces `ORA-12566` / `ORA-03113`.
The AUTH challenge itself is correctly written with `encodeV315DataPacket`
(4-byte framing) — `sendAuthFailed` was simply not updated to match.

### Changes
1. `session.go:sendAuthFailed` — frame the `buildAuthFailed` TTC payload with
   `encodeV315DataPacket` (4-byte v315+ header, type `0x06`) and write it
   directly, instead of `writeTNSPacket` (legacy 2-byte). This is the fix that
   makes any reject frame render as a clean ORA error rather than ORA-12566.
2. `errors.go` — add `ORA01045 = 1045` ("user lacks privilege; logon denied"),
   the canonical Oracle "known but not permitted to log on" code, for the
   no-grant path.
3. `session.go:run()` — route the `authenticateClient` error through a new pure
   helper `authRejectFor(err)`:
   - `ErrNoActiveGrant` → `ORA01045`, actionable message
     "no active grant for this database; request access via dbbat".
   - everything else (user-not-found, bad password) → `ORA01017`,
     "invalid username/password; logon denied" — generic, does not reveal
     whether the username exists or the password was wrong.
   This mirrors the PostgreSQL proxy, which distinguishes
   "access denied: no valid grant" from "authentication failed".

### Tests (no Docker)
- `session_test.go` — assert `sendAuthFailed` writes a **v315+** frame: raw
  bytes `[0:2] == 0x0000`, `BE uint32([0:4]) == totalLen`, type byte `0x06`,
  TTC payload `00 00 08` + compressed ORA code + CLR message; decode and assert
  the code (regression against the abrupt-EOF / 2-byte-framing bug).
- `authRejectFor` table test: `ErrNoActiveGrant` → 1045; `ErrUserNotFound`,
  bad-password/other → 1017.
