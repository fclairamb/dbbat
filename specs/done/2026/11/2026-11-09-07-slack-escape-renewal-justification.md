---
model: opus
effort: low
pr: batch
automerge: false
---

# Slack: renewal justification is posted as unescaped mrkdwn

## Problem

Found by the security scan of 2026-11-09 (finding L3). Severity: Low.
Triggerable by any dbbat user who can file a grant **renewal request** against an
auto-approve definition (where a justification is required).

`renewalText` embeds the requester-controlled justification with `%q`
(`internal/notify/slack.go:423-433`), which escapes only `"` and `\`, and the
message is posted with `slack.MsgOptionText(..., false)` — escaping disabled
(`slack.go:408`). So a justification containing Slack mrkdwn metacharacters
(`<!channel>`, `<!here>`, `<@U…>`, or a masked link `<https://evilco.example|
click here>`) renders as a live mention or link in the ops channel, posted by the
official dbbat bot. This is phishing / nuisance inside a trusted channel; it
grants no dbbat privilege.

This is the same unescaped-mrkdwn class flagged (but never specced) as L4 in the
2026-11-02 scan; the renewal justification is a concrete new vector because that
field is user-supplied and required.

## Proposal

1. Escape Slack mrkdwn metacharacters (`&`, `<`, `>` → `&amp;`, `&lt;`, `&gt;`)
   in every user-supplied string interpolated into a mrkdwn message posted with
   escaping disabled. Add one helper (e.g. `escapeSlackText`) in
   `internal/notify/slack.go` and apply it to the justification in
   `renewalText`.
2. Audit the other user-supplied strings posted to Slack for the same pattern
   and route them through the helper: the held/terminated SQL text, the grant
   request justification on the initial post, and any username rendered into
   mrkdwn. (The 2026-11-02 L4 covered these generally; fix them together.)
3. Alternatively, post these fields in a `section` block whose text type is
   plain — decide per call site; escaping is the smaller change.

## Tests

- `internal/notify/` (the slack message-building test, e.g.
  `channel_routing_test.go` or a new `slack_escape_test.go`): `renewalText` with
  a justification of `<!channel> <https://evilco.example|x>` produces output with
  the metacharacters escaped (fails before the fix — the raw `<`/`>` survive).
- Positive control: an ordinary justification (`"need prod access for incident
  1234"`) is unchanged apart from the escaping of any `&`, and renders as plain
  text.

## To verify

- The full list of call sites that pass user-supplied text to
  `MsgOptionText(..., false)` (grep `MsgOptionText` in `internal/notify`), so the
  helper is applied consistently and the fix closes 2026-11-02 L4 too.
