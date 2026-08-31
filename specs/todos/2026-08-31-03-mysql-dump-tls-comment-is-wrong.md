# MySQL: prove the capture tap records plaintext under TLS

## Goal

Add the test that pins what `docs/dump-format.md` ("Captures are plaintext,
above TLS") and `startDumpIfConfigured` in `internal/proxy/mysql/session.go` now
both assert: a capture taken on a TLS-upgraded MySQL session holds plaintext
MySQL packets, not TLS records.

## Why

Found while auditing every proxy's dump coverage for
`2026-08-31-02-oracle-dump-synthesized-refusal-frames.md`. The comment on
`startDumpIfConfigured` used to claim the opposite — that the tap "will see TLS
records (encrypted application data) … the dump captures timing and packet
boundaries even when payload is opaque" — which steered an operator away from
taking a capture on exactly the sessions they most want one for.

The comment and the doc were corrected in that spec, from reading go-mysql's
`server/handshake_resp.go`: the library upgrades with `c.Conn.Conn = tlsConn`
and dbbat wraps *that* field, so the tap sits above TLS. But the claim now rests
on one careful read of a dependency's internals, which a go-mysql upgrade could
invalidate silently — a wrong comment is what started this, and nothing would
catch it coming back. The same ordering argument is made for PostgreSQL,
MongoDB and SQL Server and is equally untested.

## Implementation

- Unit test in `internal/proxy/mysql`: install the tap over a conn that stands
  in for the `*tls.Conn` (a `net.Conn` fake that records what it is handed is
  enough — the point is the wrap *order*, not real crypto) and assert the
  recorded bytes are the plaintext handed to `Write`.
- Stronger, if cheap: the `//go:build integration` MySQL suite already drives a
  TLS client. Assert there that the resulting `.pcapng` parses as MySQL packets
  rather than as TLS records — that version would survive a go-mysql upgrade
  changing where the `*tls.Conn` is installed.
- Do the same for PostgreSQL (`negotiateSSL`, which runs before
  `attachDumpTaps`); MongoDB and SQL Server need no test, their recording points
  are at the message layer and never touch the socket.
