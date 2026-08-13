# The detached store-write goroutines still take the process down on a panic

## Goal

Finish what
`specs/.../2026-08-13-08-oracle-relay-goroutines-have-no-panic-recovery.md`
started: the relays and the limit watchdogs are guarded on all five protocols
now, but every proxy also fires *detached* goroutines to write to the store, and
those are still bare. A panic in one of them ends the process — every live
session, of every user, on every database — exactly as a relay panic used to.

## Why

The recover added on each proxy's `handleConnection` does not reach them: they
outlive the call that spawned them by design (a query record must not block the
wire), so they run on goroutines of their own with no recover above them.

They are less exposed than a relay — they handle values dbbat itself built,
not bytes a client sent — but not unexposed: what they carry are decoded
statements, captured result rows and parameter blobs lifted off the wire, so a
malformed frame can still reach them by way of a decoder that did not panic
until the encode.

The sites, as of the 08 fix:

- `internal/proxy/oracle/intercept.go:940`, `:1042`;
  `internal/proxy/oracle/session.go:864`, `:1017`
- `internal/proxy/postgresql/session.go:758`;
  `internal/proxy/postgresql/intercept.go:520`; `internal/proxy/postgresql/auth.go:157`
- `internal/proxy/mysql/intercept.go:250`, `:344`; `internal/proxy/mysql/auth.go:140`
- `internal/proxy/mssql/result.go:731`; `internal/proxy/mssql/auth.go:130`
- `internal/proxy/mongodb/result.go:287`; `internal/proxy/mongodb/intercept.go:245`;
  `internal/proxy/mongodb/auth.go:238`

Worth checking the same way while in there: the `RowWriter` drain goroutine
(`internal/proxy/shared/rowwriter.go`), each proxy's `runDumpCleanup`, and the
bare goroutines outside `internal/proxy` entirely (`internal/api`,
`internal/store`, `internal/mcp`) — the blast radius argument does not stop at
the proxy package.

No GitHub issue yet — one should be filed.

## Implementation

- `shared.RunGuarded` already exists (`internal/proxy/shared/panics.go`) and is
  exactly the right shape for a goroutine with nothing to report to: wrap each
  site as `go shared.RunGuarded(ctx, logger, name, func() { … })`.
- Prefer naming each site after what it writes (`"oracle query record"`,
  `"mysql held query completion"`) rather than a generic label — the point of the
  log line is that it says which write died.
- The `IncrementAPIKeyUsage` one-liners are the cheapest of the lot and are
  identical in all five proxies; consider a single `shared` helper for them
  instead of five wrapped closures.
- A test per proxy is overkill. `shared.RunGuarded` is already pinned
  (`internal/proxy/shared/panics_test.go`); one table-driven test that a
  store-write goroutine's panic does not escape would be enough, if a seam
  exists to inject the failure without a real store.
