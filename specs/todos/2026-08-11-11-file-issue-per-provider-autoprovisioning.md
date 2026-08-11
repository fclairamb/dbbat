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
