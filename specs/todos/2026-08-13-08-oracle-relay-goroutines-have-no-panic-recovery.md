# A panic in an Oracle relay goroutine takes the process down, not the session

## Goal

Make a panic on either Oracle relay goroutine end the *session* it belongs to,
the way a panic anywhere else in a session already does, instead of ending the
whole dbbat process.

## Why

`session.proxyMessages` starts both directions bare:

```go
go func() { errChan <- s.clientToUpstream() }()
go func() { errChan <- s.upstreamToClient() }()
```

The only `recover` above them is on `Server.handleConnection`, which runs on a
**different** goroutine and therefore catches nothing either of these raises. Go
kills the process on an unrecovered panic in any goroutine, so one malformed
session can take every other live session with it — including sessions belonging
to other users and other databases.

The two intercept paths *are* guarded (`interceptClientMessage`,
`interceptUpstreamMessage`, each with its own recover, precisely because a
malformed TTC layout must never break the connection). Everything else on those
goroutines is not: `readTNSPacket`/`writeTNSPacket`, the dump writer, the
mid-stream limit check, `holdIfNeeded`, and — added by
`specs/done/.../2026-08-13-01-mid-reply-refusal-lands-mid-ttc-message.md` —
`heldRefusalBlocks`'s teardown, which is the one that carries its own nested
recover for exactly this reason. That nested recover is a patch on one site; the
goroutines are the hole.

Found while auditing the held mid-reply refusal, which is what makes it
worth writing down: the fix there had to reason about "a panic here ends the
process" and guard one call site, and the next person will have to reason about
it again. This is pre-existing and predates that change — it is not a regression
from it.

Every other protocol proxy should be checked for the same shape; PostgreSQL,
MySQL, MongoDB and MSSQL all run their relays on goroutines too.

No GitHub issue yet — one should be filed.

## Implementation

- Give both goroutines in `session.proxyMessages` (`internal/proxy/oracle/session.go`)
  a `defer func() { if r := recover(); r != nil { … } }()` that logs with the
  stack (as `handleConnection` does, `internal/proxy/oracle/server.go`) and
  pushes an error onto `errChan` so the session tears down normally — cleanup,
  connection record closed, dump flushed — rather than dying mid-flight.
- The error must reach `errChan` on the panic path too, or `proxyMessages`
  blocks forever on a session whose relay is gone. `errChan` is buffered at 2,
  so the send cannot block.
- Prefer one small helper (`s.relay(name string, fn func() error, errChan chan error)`)
  over two copies, and use it for both directions.
- Once it exists, revisit the nested recover in `heldRefusalBlocks`: it stays
  useful (it keeps the *session* alive through a teardown panic rather than
  merely the process), but its comment should stop claiming the goroutine has no
  recover above it.
- Check the other four proxies for the same pattern and fix them the same way,
  or record why they differ.
- A test can pin it: a session whose upstream conn returns a payload that makes
  a decoder panic must end the session and leave the process running — a
  `recover`-less panic in a test goroutine fails the whole test binary, which is
  itself the assertion.
