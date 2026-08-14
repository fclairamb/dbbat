# `TestApprovedRequestBindsTheGrantToTheServerGroup` fails in isolation

## Goal

Make `TestApprovedRequestBindsTheGrantToTheServerGroup`
(`internal/store/grants_server_group_test.go`) pass whether it runs alone or as
part of the package, and work out which of the two runs is telling the truth.

No GitHub issue filed yet — one should be.

## Why

The test is order-dependent today:

```
go test ./internal/store/ -run TestApprovedRequestBindsTheGrantToTheServerGroup -count=1
  --- FAIL: grants_server_group_test.go:337: GetActiveGrant(after joining) error = no active grant found
go test ./internal/store/ -count=1
  ok
```

It also failed once inside a full `make test` run, so the whole-package pass is
not reliable either — it is a flake, not a clean "only fails alone".

This is **pre-existing**: it reproduces at `120fa77`, before the Kubernetes CA
TOFU work, in a detached worktree. It was found while running the QA gate for
`specs/todos/2026-08-10-15-kubernetes-ca-tofu-pin.md` and is unrelated to it.

It matters beyond the noise: the assertion is the *live server-group
membership* rule — adding a server to a group must immediately widen every
grant bound to that group, sessions included. If `GetActiveGrant` genuinely
cannot see a freshly joined server under some orderings, that is a real
authorization bug wearing a flaky-test costume.

## Implementation

- Reproduce with `-run` alone, then bisect what the rest of the package
  supplies that makes it pass — most likely a row another test creates, or
  shared clock/lookup state. `GetActiveGrant` is in
  `internal/store/grants.go`; the group-membership join is what to read first.
- Check the obvious candidates before anything clever: the grant's `starts_at`
  window versus the test's wall clock (a grant that starts "now" and a query
  that filters `starts_at <= now()` can race at sub-second resolution), and any
  cache in front of group membership that a solo run leaves cold or warm
  differently.
- Decide which behaviour is correct, fix that side, and keep the test as the
  regression guard — do not paper over it with a retry or a sleep.
- Run it 20×, alone and in-package, before calling it fixed.
