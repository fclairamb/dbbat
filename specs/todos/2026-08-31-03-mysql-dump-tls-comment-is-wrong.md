# MySQL: the capture tap records plaintext, not the TLS records its comment claims

## Goal

Correct `startDumpIfConfigured` in `internal/proxy/mysql/session.go` — its
comment tells operators that a capture of a TLS-upgraded MySQL session is
unreadable, which is the opposite of what the code does — and pin the real
behaviour with a test.

## Why

Found while auditing every proxy's dump coverage for
`2026-08-31-02-oracle-dump-synthesized-refusal-frames.md`. The comment says:

> For TLS-upgraded connections the library has already replaced `c.Conn.Conn`
> with a `*tls.Conn` before this point; the tap will see TLS records (encrypted
> application data), which is fine — the dump captures timing and packet
> boundaries even when payload is opaque.

go-mysql upgrades with `c.Conn.Conn = tlsConn`
(`server/handshake_resp.go:105`, v1.16.0), and dbbat then wraps *that* field:
`s.serverConn.Conn.Conn = dump.NewTapConn(s.serverConn.Conn.Conn, …)`. The tap
therefore sits **above** TLS — `packet.Conn` writes plaintext MySQL packets into
it and it forwards them to the `*tls.Conn` to be encrypted; reads come back
already decrypted. Captures of TLS sessions are fully legible, and have been all
along.

The comment matters because it is the only place the behaviour is written down,
and it steers an operator away from taking a capture on an encrypted session —
exactly the session where they most want one. `docs/dump-format.md` says nothing
either way, so there is no second source to correct it.

## Implementation

- Rewrite the comment in `internal/proxy/mysql/session.go`
  (`startDumpIfConfigured`) to say the tap sits above the TLS layer and records
  plaintext MySQL packets on both legs, and why (go-mysql swaps the `net.Conn`
  underneath `packet.Conn`, so the wrap order puts us on the plaintext side).
- Add the same statement to `docs/dump-format.md` under "What a capture
  contains" — the per-protocol table there is where a reader will look.
- Test: a unit test in `internal/proxy/mysql` that installs the tap over a
  `tls.Conn`-shaped fake and asserts the recorded bytes are the plaintext handed
  to `Write`, not ciphertext. If a fake is awkward, the existing
  `//go:build integration` MySQL suite already runs a TLS client — assert there
  that the capture parses as MySQL packets.
- While in there, check the same question for the PostgreSQL and MongoDB taps
  (both also wrap the client conn after a possible TLS upgrade) and state the
  answer in the same place.
