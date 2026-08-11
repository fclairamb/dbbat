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
