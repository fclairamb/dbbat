# Remediate Security Review Findings

## Goal

Remove session-token leakage, enforce user-detail authorization, purge captured credentials from version control, and update vulnerable runtime and frontend dependencies.

## Why

The OAuth callback redirects an active `web_` session token in the URL. Request logging records query strings, and the repository currently tracks a HAR containing such a token. Any actor with access to request logs, browser/network history, or the artifact during its validity can replay the session. Separately, any authenticated connector can request another user's detail and group membership through `GET /api/v1/users/:uid`, contrary to the list endpoint's own visibility policy.

`govulncheck` also reports reachable vulnerabilities in the Go 1.26.1 standard library and in `google.golang.org/grpc` and `github.com/quic-go/quic-go`; `bun audit --production` reports vulnerable frontend/build dependencies including `vite` and `seroval`.

An issue should be filed before implementation.

## Implementation

1. Replace the OAuth query-string token handoff with a short-lived, single-use exchange or a Secure, HttpOnly, SameSite cookie, and redact sensitive query parameters in HTTP access logs.
2. Rotate any token that may have been captured, remove `priv/dbbat.tools.stonal.io.har` from Git history/current tree, and add an ignore rule plus secret-scanning CI protection for HAR exports.
3. Make `handleGetUser` allow non-admin/non-viewer callers to retrieve only their own row (prefer a 404 for another UID), and add endpoint-level authorization tests.
4. Upgrade the Go build/runtime image to at least Go 1.26.5, bump `google.golang.org/grpc` to at least 1.82.1 and `github.com/quic-go/quic-go` to at least 0.59.1 through the dependency graph, then run `govulncheck ./...`.
5. Upgrade the affected Bun dependencies/lockfile (including Vite and the TanStack router stack), then rerun `bun audit --production` and the frontend build.

## Resolved open questions

**Item 2 — should `priv/dbbat.tools.stonal.io.har` only be removed from the
working tree, or purged from Git history as well?**

Decision (2026-08-07, repository owner): **purge it from history as well.**
But the purge is sequenced deliberately:

- **In this spec's implementation, do the tree-level work only**: `git rm` the
  HAR, add a `.gitignore` rule covering `*.har`, and add secret-scanning CI
  protection. Commit that normally.
- **Do NOT run `git filter-repo` / BFG as part of implementing this spec.** A
  history rewrite invalidates every commit SHA on the branch, and this working
  tree is shared with a running batch of other specs. The rewrite is performed by
  the coordinator as the **final action of the whole batch**, after every other
  spec has been implemented and archived.
- **Do NOT force-push, ever.** The force-push over the remote is the repository
  owner's to run by hand, after they have inspected the rewritten history.

**Item 2 — token rotation.**

Rotating the captured `web_` session token is a manual operator action against
the live deployment and is not automatable here. Note it in the final report as
an outstanding action for the owner; the tree/history work is what this spec
delivers. Rotation is the real fix — a rotated token in old history is inert.

**Should a GitHub issue be filed for this spec?**

Decision: **no.** Do not run `gh issue create`. The spec file is the record.
