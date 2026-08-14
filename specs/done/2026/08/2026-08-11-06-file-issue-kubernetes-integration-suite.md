---
model: sonnet
effort: low
---

# File the GitHub issues the Kubernetes tunnel work left implicit

## Goal

Two pieces of Kubernetes-tunnel work were specced and (for the first) shipped
with no GitHub issue behind them. File them, then link the issue from the spec
files the way `specs/README.md` asks.

## Why

`specs/todos/2026-08-10-16-kubernetes-tunnel-integration-suite.md` says outright
"No GitHub issue filed yet — one should be", and
`specs/todos/2026-08-11-05-conncheck-portforward-rbac-both-verbs.md` (filed off
the back of it) says the same. The issue is what makes the change discoverable
from outside the repo, and the release notes reference issue numbers.

## Implementation

- `gh issue create` for the real-cluster integration suite, describing what it
  covers that the in-process fake cannot (the websocket transport, RBAC as the
  API server evaluates it, a pod restart, a real database session). Link the
  commits that landed it and the archived spec.
- `gh issue create` for the `pods/portforward` RBAC verb question, summarising
  todo `2026-08-11-05`.
- Add the `[#N](https://github.com/fclairamb/dbbat/issues/N)` links to both spec
  files (the first one now lives under `specs/done/2026/08/`).

## Resolved open questions

> May an implementing agent run `gh issue create` on the owner's behalf?

**Decision (2026-08-11): no.** The owner was asked directly during an
`/implement-todos` batch and answered "proceed but do not create issues".
Creating a public issue is an outward-facing publish action and stays a human
action. **This spec is therefore not implementable by automation** — it stays in
`specs/todos/` until the owner files the issues themselves, at which point only
the `[#N](…)` link edits remain. Do not implement it, and do not archive it.

## Closed — not implemented (2026-08-14)

The owner dropped the GitHub-issue requirement outright: *"You never need to
create github issues."* `specs/README.md` no longer asks for one and the root
`CLAUDE.md` now forbids filing them.

This spec's only content was "open these issues and link them back", so there is
nothing left in it to do. It supersedes the `## Resolved open questions` decision
above, which parked the spec in `specs/todos/` pending the owner filing the
issues by hand — that is no longer wanted either.

Archived as closed rather than deleted, so the reasoning stays greppable. **It
was never implemented**; no issue was created, and none should be.
