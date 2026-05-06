# PostgreSQL proxy hangs on libpq GSSEncRequest

## Goal

Make the dbbat PostgreSQL proxy reply to a **GSSEncRequest** startup packet so that modern libpq clients (psql 17 from Homebrew, most distro builds of psql 12+, anything else linked against a GSS-enabled libpq) can connect without setting `PGGSSENCMODE=disable`.

Today, running

```
psql -h <dbbat-host> -p 5432 -U <user> -d <db>
```

with a stock Homebrew psql 17 fails with

```
psql: error: connection to server at "...", port 5432 failed: timeout expired
```

even though TCP connectivity is fine, the user / database / grant exist, and the proxy is otherwise healthy.

## Why this matters

- **Default psql 17 builds are broken against dbbat.** Homebrew, most Linux distros, and AWS RDS official client packages ship libpq with GSS support compiled in, which means `gssencmode=prefer` is the default.
- The error message is **`timeout expired`** with no hint that GSS is the culprit. Users have no path to a workaround without reading the source.
- The workaround (`PGGSSENCMODE=disable`) is non-discoverable and has to be set on every client machine. Operators rolling out dbbat to a team will hit this silently.
- pgx-based and pure-JDBC clients aren't affected (GSS is opt-in there), so the bug only shows up against the most common interactive client. That's worse, not better — it's the demo path.

## Symptom on the wire

Verified 2026-05-06 against `k8s-tooling-dbbatpg-…elb.eu-west-3.amazonaws.com:5432` from a Homebrew psql 17.5 client. Three independent probes:

```
Client → Server  (8 bytes)  00 00 00 08 04 d2 16 30          GSSEncRequest (code 80877104)
Server → Client                                              <silence — connection hangs>
```

For comparison, the rest of the protocol works correctly when the GSS step is skipped:

```
SSLRequest    (8 bytes, code 80877103)  →  N                      (TLS refused, expected)
StartupMessage v3                       →  R 00 00 00 08 00 00 00 03   (AuthenticationCleartextPassword)
PasswordMessage "wrongpw"               →  E … FATAL 28000 authentication failed
```

So the proxy is healthy after auth — it's specifically the GSS startup negotiation that deadlocks.

`PGGSSENCMODE=disable psql …` succeeds end-to-end against the same instance, confirming the diagnosis.

## Root cause

`internal/proxy/postgresql/session.go:497-543` `receiveStartupMessage` hand-rolls the startup parser and only recognises one of the two length-8 magic packets:

```go
const sslRequestCode = 80877103
if version == sslRequestCode {
    s.clientConn.Write([]byte{'N'})  // refuse, then…
    return s.receiveStartupMessage() // …read the next message
}
// fall through
msgBuf := make([]byte, length)         // length == 8
copy(msgBuf, lengthBuf)
io.ReadFull(s.clientConn, msgBuf[4:])  // ← BLOCKS: tries to read 4 more bytes
```

When the client sends a **GSSEncRequest** (length=8, code `80877104`):

1. The function reads 4 length bytes (`length=8`).
2. Enters the `if length == 8` branch and reads 4 more bytes (the version code).
3. Compares against `sslRequestCode` — mismatch — falls through.
4. Drops to `io.ReadFull(s.clientConn, msgBuf[4:])`, which expects 4 more bytes from the wire.
5. The client is *not* sending more bytes; it's waiting for the server's GSS reply (`G` accept / `N` reject / `E` error).
6. Both sides block until libpq's `PGCONNECT_TIMEOUT` fires.

`pgproto3` (already a dep, used elsewhere in the same Session) defines `GSSEncRequest` and `gssEncReqNumber = 80877104`, and `pgproto3.Backend.ReceiveStartupMessage` correctly returns one of `StartupMessage`, `SSLRequest`, `GSSEncRequest`, `CancelRequest`. The dbbat proxy just doesn't use it for the startup phase — it parses the header by hand and only knows the SSL code.

Same bug shape exists for `CancelRequest` (length=16, code `80877102`): the current parser will read all 16 bytes but then try to decode them as a `pgproto3.StartupMessage` and fail. Less severe (errors instead of hanging), but still wrong.

## Fix approach

### Recommended: replace hand-rolled parser with `pgproto3.Backend.ReceiveStartupMessage`

`s.clientBackend` is already constructed in `Run()` before `authenticate()` runs (`session.go:127`), so `receiveStartupMessage` can use it instead of poking `s.clientConn` directly. `pgproto3.Backend.ReceiveStartupMessage()` already handles all four startup-frame variants correctly.

```go
// receiveStartupMessage receives the startup message from the client,
// transparently denying any SSL/GSS negotiation requests beforehand.
func (s *Session) receiveStartupMessage() (*pgproto3.StartupMessage, error) {
    // Bound the negotiation loop: a real client does at most two rounds
    // (GSSEncRequest then SSLRequest) before sending the actual StartupMessage.
    for i := 0; i < 3; i++ {
        msg, err := s.clientBackend.ReceiveStartupMessage()
        if err != nil {
            return nil, fmt.Errorf("read startup: %w", err)
        }
        switch m := msg.(type) {
        case *pgproto3.StartupMessage:
            return m, nil
        case *pgproto3.SSLRequest, *pgproto3.GSSEncRequest:
            // Refuse encryption; client falls back to the next mode.
            if _, err := s.clientConn.Write([]byte{'N'}); err != nil {
                return nil, fmt.Errorf("deny encryption negotiation: %w", err)
            }
        case *pgproto3.CancelRequest:
            // dbbat doesn't track session keys yet, so we can't honour
            // the cancel. Close cleanly instead of dangling.
            return nil, ErrCancelRequestUnsupported
        default:
            return nil, fmt.Errorf("unexpected startup frame %T", msg)
        }
    }
    return nil, fmt.Errorf("client sent too many SSL/GSS negotiation rounds")
}
```

Caller change at `auth.go:17` is a one-liner: `startupMsg.(*pgproto3.StartupMessage)` cast goes away because the function now returns the concrete type. Drop the `ErrExpectedStartupMessage` branch from `authenticate()` — the new function only ever returns a `*StartupMessage` or an error.

Add `ErrCancelRequestUnsupported` to `internal/proxy/postgresql/errors.go`.

### Minimal alternative: extend the hand-rolled parser

If the refactor above is judged too invasive, the smallest possible patch is to add a second magic-number case alongside the SSL one and bound the recursion:

```go
const (
    sslRequestCode    = 80877103
    gssEncRequestCode = 80877104
)

if length == 8 {
    // …read version…
    if version == sslRequestCode || version == gssEncRequestCode {
        if _, err := s.clientConn.Write([]byte{'N'}); err != nil {
            return nil, fmt.Errorf("deny encryption negotiation: %w", err)
        }
        if depth >= 2 { return nil, fmt.Errorf("too many negotiation rounds") }
        return s.receiveStartupMessageDepth(depth + 1)
    }
    // Anything else with length==8 is malformed — error out instead of blocking.
    return nil, fmt.Errorf("unknown length-8 startup magic 0x%08x", version)
}
```

This keeps the existing structure and is the lower-risk change for a point release. The `pgproto3.Backend` route is preferred for the long term because it removes 40-odd lines of brittle protocol parsing; pick whichever the reviewer prefers.

## Acceptance criteria

- [ ] Homebrew **psql 17** with default settings (`gssencmode=prefer`) connects through dbbat and runs `SELECT 1` end-to-end. No `PGGSSENCMODE=disable` needed.
- [ ] **psql 16 and earlier** (typical no-GSS or GSS-prefer build) still works — both with and without an SSL-prefer fallback.
- [ ] **pgx-based clients** (the dbbat upstream connection itself, plus any Go consumer using jackc/pgx) still work — no regression on the existing happy path.
- [ ] **JDBC PG driver** (DBeaver, IntelliJ data tools) still works.
- [ ] An explicit `gssencmode=require` connection from psql is rejected cleanly (the proxy doesn't speak GSS, so the client should error fast — not hang).
- [ ] A malicious or buggy client that loops sending `SSLRequest` forever is bounded; the proxy returns an error after at most 2-3 negotiation rounds rather than running unbounded recursion.
- [ ] A `CancelRequest` (different connection, length=16, code `80877102`) doesn't hang the proxy; it's either ignored or errored — but never deadlocks.
- [ ] Unit tests in `internal/proxy/postgresql/session_test.go` (create the file) cover at minimum:
  - plain StartupMessage → parsed
  - SSLRequest → `N` written, then plain StartupMessage parsed
  - GSSEncRequest → `N` written, then plain StartupMessage parsed
  - GSSEncRequest → `N`, then SSLRequest → `N`, then StartupMessage parsed (libpq 17 default flow)
  - Repeated SSL/GSS requests bounded (returns error after the cap)
  - CancelRequest handled cleanly (no block)
- [ ] `make test` and `make lint` pass.

## Implementation notes

- `s.clientBackend` is created at `session.go:127` *before* `authenticate()` runs, so it's safe to use inside `receiveStartupMessage`. The dump-tap re-creation at `session.go:172` happens after auth, so the backend identity used during startup is the right one (raw `clientConn`, not the tap).
- Keep `s.clientConn.Write([]byte{'N'})` for the refusal byte — that's a single byte that doesn't need to go through the backend's framing.
- For testing, expose a variant that takes an `io.ReadWriter` (or wire it through `net.Pipe`) so tests don't need a real TCP socket. The simplest pattern: extract the loop into a free function `negotiateStartup(backend *pgproto3.Backend, denyWriter io.Writer) (*pgproto3.StartupMessage, error)` and have the method call it.
- libpq 17 sends GSS first, then SSL, then plain (`gssencmode=prefer` + `sslmode=prefer` defaults). Both denials happen on the **same TCP connection**, with no reconnect between them. The fix must not close or reset the socket between rounds.
- The denial byte for both SSL and GSS is the same: ASCII `'N'` (0x4E). For GSS, `'G'` would mean "I'll speak GSS encryption now"; we don't, so `'N'` is correct.
- Spell the constants out (`sslRequestCode = 80877103`, `gssEncRequestCode = 80877104`) and match the receive logic to the values from `pgproto3` so a future libpq version that adds, say, a third pre-startup probe is easier to extend.

## Out of scope

- **Implementing actual GSSAPI/Kerberos encryption.** dbbat doesn't terminate Kerberos and there's no operator demand for it — refusing the upgrade and falling back is the right answer.
- **Implementing query cancellation** (the `CancelRequest` path). Tracked separately if/when needed; for this fix, returning a clean error is sufficient.
- **Replacing `receivePasswordMessage` with `clientBackend.SetAuthType` + `Receive`.** Same hand-rolled-parser smell, but it's not currently broken — it can ride along in a follow-up cleanup.
- **TLS termination on the PG listener.** Out of scope here; the proxy refuses TLS on purpose today (the wire trace above shows the `N` reply to SSLRequest). If TLS support is added later, the GSS denial logic still applies.

## Verification

1. **Unit tests** — `make test` covering the negotiation matrix above.
2. **Local manual test** — run `make dev`, then from the host:
   ```
   PGPASSWORD=admintest psql -h 127.0.0.1 -p 5434 -U admin -d <some-db> -c "SELECT 1"
   ```
   on a Homebrew psql 17 client. Expect a row, not a timeout.
3. **Production / ELB test** — once a `test-` image is rolled per `feedback_intermediate_releases.md`, retry the failing command from this session against `k8s-tooling-dbbatpg-…elb.eu-west-3.amazonaws.com:5432`. Expect prompt → password → result.
4. **Regression check** — run the existing `internal/proxy/postgresql/intercept_test.go` suite; nothing in the post-auth path should change.
5. **JDBC sanity** — connect from DBeaver (or any JDBC client) to confirm the non-GSS flow still works.

## Related

- `internal/proxy/postgresql/session.go:497-543` — the buggy hand-rolled parser.
- `internal/proxy/postgresql/auth.go:17` — only caller of `receiveStartupMessage`.
- `pgproto3.Backend.ReceiveStartupMessage` (`github.com/jackc/pgx/v5/pgproto3/backend.go`) — the library function that already does this correctly.
- PG protocol docs: <https://www.postgresql.org/docs/current/protocol-flow.html#PROTOCOL-FLOW-SSL> (SSL/GSS negotiation paragraph; both denials use the same single-byte response semantics).

## Implementation Plan

Note: the PostgreSQL TLS termination spec landed first and already restructured
`receiveStartupMessage` so SSL detection happens in a separate `negotiateSSL`
step that uses a `bufio.Reader` peek-and-discard. The GSS fix slots into that
same step rather than the hand-rolled parser the spec quotes.

1. **Extend `negotiateSSL` to a multi-round encryption negotiation**. Rename it to `negotiateStartupEncryption` (or similar). On each round, peek the first 8 bytes; recognize `pgSSLRequestCode` (TLS upgrade or `'N'` deny) and `pgGSSEncRequestCode` (`'N'` deny only — dbbat doesn't speak Kerberos). After 2-3 rounds, error out so a misbehaving client can't loop forever.
2. **Add `pgGSSEncRequestCode` constant and `ErrTooManyNegotiationRounds` sentinel** in the postgresql package.
3. **Tests** — extend `negotiate_ssl_test.go` (or add a new test file) covering: GSS-then-StartupMessage (no TLS), GSS-then-SSL-then-StartupMessage (libpq17 default), unbounded GSS loop bounded by the round cap, length-8-with-unknown-magic returns a clear error.
4. **CancelRequest** — out of scope per spec; still want a bounded behavior. The peek-based negotiation only consumes 8 bytes when it recognizes SSL/GSS; for CancelRequest (length=16) the peek will see `length=16`, fall through to "not SSL/GSS", and `receiveStartupMessage` will read all 16 bytes and try to decode as StartupMessage — which fails with a parse error rather than hanging. Acceptable.
5. **QA**: `make test`, `make lint`, `make build-binary`. Manual smoke check with a libpq17 client if convenient.
