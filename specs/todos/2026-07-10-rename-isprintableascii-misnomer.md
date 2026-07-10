# Audit and rename the misleading `isPrintableASCII` helper in the Oracle proxy

## Goal
Rename `isPrintableASCII` (in `internal/proxy/oracle/ttc_auth.go`) to something
truthful like `isIdentifierRun`, and audit every call site for cases where the
caller actually meant "printable ASCII" (including `.`, `-`, `@`, spaces, etc.)
rather than "Oracle identifier bytes only".

## Why
`isPrintableASCII` does **not** test for printable ASCII — it only accepts the
Oracle identifier set (`A-Z a-z 0-9 _ $ #`). This footgun directly caused the
`ORA-03146` sqlplus-through-proxy failure fixed in
[#235](https://github.com/fclairamb/dbbat/pull/235): the wide-encoding username
locator used it as a "is this a username byte" check, so any login name
containing a `.` (e.g. `florent.clairambault`) was silently truncated at the dot.

That fix introduced a correctly-scoped `isPrintableASCIIRun` (true 0x20–0x7E
check) for the username span, but left the misnamed helper in place with its
other callers untouched. Those callers may have the same latent bug for
non-identifier characters.

## Implementation
- Grep `isPrintableASCII` across `internal/proxy/oracle/`:
  `rewriteAuthPhase1Username` (`detectUsernameEncoding`), `readUsernameAtOffset`,
  and any others.
- For each, decide whether the intent is "identifier bytes only" (keep, but
  rename to `isIdentifierRun` for honesty) or "any printable byte" (switch to
  `isPrintableASCIIRun` / consolidate the two helpers).
- Pay special attention to `detectUsernameEncoding` and the fixed-offset
  fallback `rewriteAuthPhase1Username`: if the anchored locator ever fails and
  it falls back, a dotted username would break there too via the same misnomer.
- Add a table-driven test asserting each helper's acceptance set (dot, dash, at,
  space) so the naming and behavior can't drift again.

No originating GitHub issue yet — file one if this is picked up.
