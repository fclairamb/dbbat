# Budget the statement-timeout integration test's durations against their own exchange

## Goal

Replace the remaining absolute wall-clock duration threshold in the
statement-timeout integration suite with a per-statement budget measured around
that statement's own exchange, the way `internal/proxy/postgresql/session_bookkeeping_test.go`
now does.

## Why

`TestIntegration_StatementTimeout_DataGripSequencesKeepTheSessionAlive`
(`internal/proxy/postgresql/statement_timeout_integration_test.go:408`) asserts
`*q.DurationMs < 1000` for the paged grid and the empty statement, to catch a
duration shifted onto a later statement's completion.

That is the same check that failed CI on PR #405: the 501-row grid's own relay
measured 549ms against a 500ms budget with nothing shifted, and widening the
margin had already been tried once (4x → 10x) without removing the flake — a
fixed budget competes with whatever the runner charges the exchange. The fix
landed for the loopback tests only; this integration test is the last absolute
threshold of that shape left in the repo (`grep` for `DurationMs` compared
against a literal), and it runs in the separate integration workflow where a
loaded runner is at least as likely.

It has not failed yet, which is exactly when it is cheap to fix.

## Implementation

- Bracket the client's own call — the statement it sends through the proxy to
  the real PostgreSQL, from send to the completion the test observes — and
  assert the logged duration fits inside that span plus a small slack, instead
  of `float64(1000)`.
- Keep the rule the slack must respect: far below the pause separating this
  statement from the next one (the loopback harness uses 50ms and a 10ms
  slack), because that pause is the smallest amount a shifted pop can add.
- Check `internal/proxy/oracle`'s two duration assertions
  (`oer_no_end_of_call_test.go:185`, `standalone_status_oer_replay_test.go:242`)
  while there: they compare against an idle/think time rather than a fixed
  constant, so they are probably already self-calibrating — confirm rather than
  change.
- Verify with `make test-integration-postgresql`.

Key files: `internal/proxy/postgresql/statement_timeout_integration_test.go`,
`internal/proxy/postgresql/session_bookkeeping_test.go` (the pattern to copy,
`driveStatement`'s returned span and `ownDurationSlackMs`).
