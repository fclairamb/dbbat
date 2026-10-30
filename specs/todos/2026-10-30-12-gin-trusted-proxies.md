---
model: opus
effort: low
---

# API: Gin trusts every proxy, so the client IP behind rate limits can be spoofed

## Problem

Found by the security scan of 2026-10-30 (finding L1).

Severity: Low. Triggered by an unauthenticated network client.

The router is `gin.New()` (`internal/api/server.go:242`) and nothing calls `SetTrustedProxies`. Gin then trusts `X-Forwarded-For` / `X-Real-IP` from any peer, so `c.ClientIP()` is whatever the client writes in those headers. That value is the key of the anonymous rate limits (`internal/api/ratelimit.go:200` and `ratelimit.go:292`, in front of `POST /auth/device` and `POST /auth/oauth/exchange`) and the `client_ip` of the access log (`server.go:659`).

A client that sends a different `X-Forwarded-For` on each request gets a fresh rate-limit bucket every time, and can write any address into the access log. Finding M9 (`2026-10-30-09`) is what that buys: unbounded growth of `oauth_states` and `audit_log`.

## Proposal

1. Call `router.SetTrustedProxies(...)` from a new setting (for example `DBB_TRUSTED_PROXIES`, CIDR list). Default to none, so `ClientIP()` is the socket peer unless the operator declares the reverse proxy in front of dbbat.
2. Document it in CLAUDE.md's environment table and `website/docs/configuration`, with the Helm chart and ingress case: a deployment behind an ingress sets the ingress CIDR.

## Tests

- `internal/api/ratelimit_test.go`: with no trusted proxies, two requests with different `X-Forwarded-For` from the same peer share one anonymous bucket. This fails before the fix.
- Positive control: with the peer's CIDR trusted, `X-Forwarded-For` is honored.
- Config test: a malformed CIDR is a startup error.

## Open questions

- Default when unset: trust nobody (secure, but behind an ingress every client then shares the ingress's bucket until configured), or keep today's behavior with a startup WARN? Recommended: trust nobody, and say it in the release notes.
