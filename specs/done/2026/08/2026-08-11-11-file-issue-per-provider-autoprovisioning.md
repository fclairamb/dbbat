# File the GitHub issue for the per-provider auto-provisioning override

## Goal

Open the GitHub issue that
`specs/todos/2026-08-10-18-per-provider-autoprovisioning-override.md` asks for,
and link it back from that spec (and from its parent, the 2026-08-09
provider-agnostic rename spec, whose issue was never filed either).

## Why

Both specs say "no GitHub issue yet — file one when picking this up". The code
landed without one, so the change has no public tracking item: nothing on the
issue tracker explains why `DBB_AUTH_AUTO_CREATE_USERS_<PROVIDER>` exists, and
the release notes will point at nothing.

Filing it was deliberately left undone rather than forgotten: creating a public
issue is a publish action, and the agent implementing the spec had no standing
authorization to make one on the user's behalf.

## Implementation

- `gh issue create` on `fclairamb/dbbat`, describing the two-providers-at-
  different-trust-levels case (a gated Entra tenant that may auto-provision next
  to a Slack workspace full of contractors that may not), and noting that it is
  implemented.
- Add the `[#N](https://github.com/fclairamb/dbbat/issues/N)` link to the spec
  file, wherever it has been archived to under `specs/done/`.
- Do the same for the parent rename spec if its issue is still missing.

## Resolved open questions

> May an implementing agent run `gh issue create` on the owner's behalf?

**Decision (2026-08-11): no.** The owner was asked directly during an
`/implement-todos` batch and answered "proceed but do not create issues".
Creating a public issue is an outward-facing publish action and stays a human
action — which is the same reason the original spec left it undone. **This spec
is therefore not implementable by automation** — it stays in `specs/todos/`
until the owner files the issue themselves, at which point only the `[#N](…)`
link edits remain. Do not implement it, and do not archive it.

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
