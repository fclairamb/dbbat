# Access-log query redaction is a denylist, so the next secret parameter leaks

## Goal

Make `redactQuery` in `internal/api/server.go` fail safe: redact every query
parameter except an explicit allowlist of known-harmless ones, instead of
redacting an enumerated list of known-sensitive ones.

## Why

Found by the completeness audit of
[2026-08-07-remediate-security-review-findings.md](../done/2026/08/2026-08-07-remediate-security-review-findings.md),
as a design caveat outside that spec's scope. That spec asked to "redact
sensitive query parameters in HTTP access logs", and the implementation does
exactly that — correctly, at the only site that logs a query string
(`internal/api/server.go:550`; the router is `gin.New()`, so gin's own
path+query logger is never installed and there is no second unredacted site).

The caveat is the direction of the check. `redactQuery`
(`internal/api/server.go:577-592`) matches parameter names against a denylist.
Every parameter the API reads today is covered — the audit cross-checked `code`,
`state`, `user_code`, `redirect`, `limit`, `cursor` and the rest — so there is no
leak right now. But the failure mode is silent and delayed: whoever adds a
`?reset_token=`, `?otp=`, `?invite=` or `?signature=` next gets it written to the
access log verbatim, and nothing in review or CI points at the omission. The
whole reason the originating security finding existed is that a session token
reached a log through a query string.

An allowlist inverts that: a new parameter is redacted by default, and the person
adding it has to consciously declare it safe to log. The cost is that genuinely
useful debug parameters need registering, which is a one-line change made by the
person who already has the context.

## Implementation

Replace the denylist in `redactQuery` with an allowlist of parameter names whose
values are safe to log — the pagination/filtering set (`limit`, `offset`,
`cursor`, `sort`, `order`, `status`, `protocol`, …) is most of it. Everything
else logs as `<redacted>`, with the *key* still logged so a request remains
diagnosable.

Keep the key visible and only mask the value: an access log that hides which
parameters were present is much harder to debug, and the parameter names
themselves are not the secret.

Add a test asserting the default: an unknown parameter name is redacted without
anyone having to add it to a list. That test is the whole point — it is what
stops the next contributor from reintroducing the denylist behaviour.

Sweep for other log sites while in there. The audit found only one today, but
`internal/api/` gains middleware over time, and any new one that formats a raw
URL should route through the same helper.

Files: `internal/api/server.go` (`redactQuery` and its caller),
`internal/api/server_test.go` (`TestRedactQuery`).

No GitHub issue exists yet; per the batch decision of 2026-08-07 none is to be
filed automatically.
