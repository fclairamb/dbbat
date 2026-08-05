---
model: opus
effort: high
---

# Approvals WebSocket topic leaks other grants' held SQL to any approver

## Problem

Reported by a security audit (cross-scope information disclosure /
authorization bypass on `GET /api/v1/stream`). Verified in code — the
combination is:

1. `approvals/pending` is a single global topic: `publishPending`
   (`internal/proxy/shared/approval.go:411`) publishes every held statement's
   full payload — `sql_text`, `database_name`, `username`, `user_uid`,
   `connection_uid` — to `events.TopicApprovalsPending`.
2. Topic authorization is coarse. `mayReadTopic`
   (`internal/api/stream.go:386-389`) admits admins **or anyone who is an
   approver on at least one live grant** (`isApproverSomewhere`,
   `internal/api/stream.go:421`). The per-send re-check (`stream.go:124-135`)
   re-asks the same topic-level question, so it never narrows per event.
3. The REST equivalent is stricter: `GET /api/v1/queries/pending` filters each
   row through `mayViewQuery` (`internal/api/approvals.go:341`), which for a
   non-admin/non-viewer requires `mayApproveQuery` — i.e. membership in *that
   grant's* approver groups.

Consequence: a user who is an approver on a single low-sensitivity grant can
subscribe to `approvals/pending` and receive the SQL text (which routinely
carries PII), target database, and requesting user of **every** held query on
the instance — including grants they have no relationship with. The stream is
exactly the "wider read path than the REST API" that the comment at
`stream.go:124-126` promises it must never become.

Existing coverage does not catch this: `TestHoldPublishesOnBothTopics`
(`internal/proxy/shared/approval_test.go:473`) asserts publication on both
topics but there is no test that an approver of grant A never *receives* grant
B's events.

## Proposal

Make the stream's effective visibility identical to `mayViewQuery`:

- Extend the subscriber authorization hook so the global approvals topic is
  filtered **per event**, not just per topic. The natural seam is the filter
  passed to `s.broker.Subscribe` (`stream.go:133-137`): for
  `TopicApprovalsPending` events, when the user is neither admin nor viewer,
  resolve the event's `query_uid` to its query/grant and apply the same
  grant-approver-group check as `mayApproveQuery`. This needs the filter to see
  the event payload (or at least topic + query/connection UID), so the
  `events.Subscriber` filter signature likely grows from `func(topic) bool` to
  something event-aware.
- Keep the memoization story: cache the per-(user, grant or connection)
  decision with the same short TTL as `topicAuthCache` so a busy instance does
  not pay a store round-trip per event. Note the cache key must include the
  event's grant/connection, not just the topic.
- Decide viewers deliberately: `mayViewQuery` lets viewers see every pending
  query, while `mayReadTopic` currently does not admit viewers to
  `approvals/pending` at all. Aligning both directions on the REST rule
  (viewers may subscribe, ordinary connectors get per-grant filtering) removes
  the asymmetry in one move.
- Also filter `announceResolved` events (`approval.go:434`) with the same rule
  — resolution events carry `connection_uid` and approver identity.
- Cross-replica path: `StartEventListener` (`stream.go:442`) republishes into
  the local broker, so a per-event filter at the subscriber seam covers
  replica-forwarded events too — verify with a test rather than assuming.
- Add the missing negative test: two users, two grants, user A approver only on
  grant A; hold a query on grant B; assert A's socket receives nothing (and an
  admin's socket does). Plus a unit test on the new per-event authorization.

Out of scope, noted by the same audit as an open policy question (not a
confirmed defect): whether the `viewer` role is *intended* to read all
queries/rows/audit via REST (`internal/api/server.go` routes). The code
comments (`approvals.go:336-340`) say yes, deliberately. If that policy ever
changes, this filter inherits the change automatically by staying aligned with
`mayViewQuery`.

No GitHub issue filed yet — this should get one, but as a security-sensitive
disclosure consider a private channel (GitHub security advisory) rather than a
public issue with the reproduction recipe.
