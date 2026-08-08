---
model: sonnet
effort: medium
---

# `TestCheck_OracleTarget_ThroughTunnel` flakes on EOF instead of the auth rejection

No GitHub issue filed yet — one should be opened when this is picked up.

## Goal

Make `TestCheck_OracleTarget_ThroughTunnel` deterministic, so a dependency-bump
PR is not blocked by a test that has nothing to do with the dependency.

## Why

It blocked [#303](https://github.com/fclairamb/dbbat/pull/303) (a
`go-mssqldb` 1.9.3 → 1.10.0 Renovate bump) **twice in a row**, then passed on a
plain re-run of the same commit. The PR changes only `go.mod`/`go.sum` for
go-mssqldb and the `go` directive — there is no path from that to an Oracle
probe running through an SSH tunnel, so this is the test, not the bump.

The failure, at
[oracle_probe_test.go:216](internal/proxy/conncheck/oracle_probe_test.go:216):

```
--- FAIL: TestCheck_OracleTarget_ThroughTunnel (0.52s)
    oracle_probe_test.go:216: stage/code = target_auth/db_handshake_failed,
      want target_auth/db_auth_failed
      (msg=the target was reachable but the database handshake failed: EOF)
```

The test stands up a fake SSH server, tunnels through it to a
`tnsRefuseListener(t, 1017)`, and asserts the probe classifies the result as
`CodeDBAuthFailed`. The classification depends on go-ora actually *reading* the
ORA-01017 refusal packet. When the listener's write and the tunnel teardown
race, go-ora sees EOF first and the probe degrades to
`CodeDBHandshakeFailed` — the same "reachable but no handshake" bucket a genuine
network fault lands in.

Cost when it fires: the failure is indistinguishable from a real regression, so
it burns a full diagnosis cycle on an unrelated PR. It is also `t.Parallel()`,
so it is likelier to lose the race on a loaded runner — which is exactly when
someone is least able to tell a flake from a break.

## Implementation

In `internal/proxy/conncheck/`:

- **Make the refusal ordered rather than racing.** `tnsRefuseListener` should
  write the ORA-01017 refusal and then wait for the client to have consumed it
  before closing — e.g. a half-close (`CloseWrite`) followed by a read to EOF,
  rather than a bare `Close()` on the accepted conn. The listener already knows
  the exact bytes it intends to deliver; nothing should close until they are
  delivered.
- **Check the same shape in the non-tunnelled sibling.** The direct-connection
  Oracle probe test uses the same listener helper and has the same latent race;
  it is presumably just faster and so has not flaked yet. Fix the helper once
  and both benefit.
- **Consider asserting on the classification input, not only the output.** If
  the test can assert that go-ora saw the ORA-01017 text, a future regression in
  `isDBAuthRejection` is distinguishable from a transport race, instead of both
  surfacing as the same wrong code.
- Do **not** paper over it by accepting `db_handshake_failed` as a pass — that
  is the exact code a real connectivity fault produces, and widening the
  assertion would make the test unable to fail for the reason it exists.

Optional, if cheap: the suite has no retry/flake reporting, so a flake is only
visible as a red PR. If `gotestsum` or `-count` retry reporting is ever added,
this test is a good canary.
