# Dev-server proxy: make the WebSocket comment true, or drop it

## Goal

In `internal/api/server.go`, `proxyToDevServer` installs a `ModifyResponse`
hook that does nothing, under the comment `// Handle WebSocket upgrades`:

```go
// Handle WebSocket upgrades
proxy.ModifyResponse = func(_ *http.Response) error {
    return nil
}
```

Either implement the thing the comment claims, or delete both.

## Why

The comment asserts a behaviour the code does not have, which is worse than no
comment: someone debugging why Vite's HMR websocket drops through the dev proxy
will read it and conclude the upgrade path is already handled.

Whether it needs implementing at all is the open question. `httputil.ReverseProxy`
has forwarded `Connection: Upgrade` / `Upgrade: websocket` natively since Go
1.12, so the 101 handshake most likely already works and the hook is simply
dead code from an earlier attempt. That should be *verified* against a real
Vite dev server (HMR over `/@vite/client`) rather than assumed — and if it does
work, the hook and its comment both go.

Dev-mode only, so no production impact either way; it is a correctness-of-the-
record cleanup.

## Implementation

- `internal/api/server.go`, `proxyToDevServer` (~line 868).
- Reproduce first: `make dev` and confirm whether HMR reconnects through the
  proxy prefix, watching for a 101 in the dev server's log.
- If it works: delete the hook and the comment.
- If it does not: find out what ReverseProxy is dropping (most likely the
  hop-by-hop header handling or a missing `Connection` passthrough) and fix it
  there — `ModifyResponse` returning nil is not where an upgrade gets handled
  in any case.
- The proxy now has coverage in `internal/api/dev_proxy_test.go`
  (path rewrite, Host passthrough, X-Forwarded-For); a websocket case would
  slot in beside it using `httptest` + `golang.org/x/net/websocket` or a raw
  `Upgrade` handshake.

## Context

Noticed while migrating this function off the deprecated
`ReverseProxy.Director` (staticcheck SA1019, surfaced by the Go 1.26 bump that
came with the k8s 0.36 update). The migration deliberately left the hook alone
to keep that change reviewable.
