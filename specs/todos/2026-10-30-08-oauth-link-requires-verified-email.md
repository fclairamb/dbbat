---
model: opus
effort: medium
---

# OAuth/OIDC: an existing account is linked by email even when the email is not verified

## Problem

Found by the security scan of 2026-10-30 (finding M8).

Severity: Medium (High on affected deployments). Triggered by anyone who can obtain an identity from a configured provider whose email claim they control, when local usernames are email addresses.

`findOrCreateOAuthUser` resolves an unlinked identity by looking up a local user whose **username** equals the identity's email, then links the identity to that account and signs the caller in as it, with all its roles, admin included (`internal/api/oauth.go:485-498`). No confirmation from the account owner is asked.

The generic OIDC provider rejects an email only when the issuer explicitly says `email_verified: false`. An absent claim is trusted (`internal/auth/oidc/provider.go:310-313`). Some issuers omit `email_verified`, and on some multi-tenant issuers the email claim is user-editable and unverified. There, an attacker sets their email to a victim's address and takes over the victim's dbbat account on first login. `DBB_OIDC_EMAIL_DOMAINS` does not help: the attacker-chosen address is in the allowed domain. A linked Slack identity also decides who may click Approve/Deny (`internal/api/slack_interactions.go`), so a hijacked link carries approver rights too.

## Proposal

1. Require a positively verified email for the email-match link (step 2): only link when the provider asserts the address is verified. Add an `EmailVerified bool` to `auth.OAuthUser`, set it from `email_verified == true` in the OIDC provider and from the equivalent signal (or `false`) in the Slack provider. Without it, skip step 2 and fall through to auto-create or refusal.
2. Never auto-link onto an account holding the `admin` role through step 2. The admin links the identity from a logged-in session instead. (Recommended; see Open questions.)
3. Audit every email-match link (`user.identity_linked`, with provider and email) so a takeover leaves a trace.

## Tests

- `internal/api/oauth_test.go`: an OIDC user with no `email_verified` claim and an email equal to an existing username is **not** linked (refused or auto-created as a new user, per config). This fails before the fix.
- `email_verified: true` still links (positive control). An existing identity link still signs in whatever the claim says.
- With the admin rule: a verified email matching an admin username is not auto-linked.

## Open questions

- Refuse email-match linking onto admin accounts entirely? Recommended: yes. An admin can link their identity explicitly from a session.
