# Support TLS 1.3 on the SQL Server proxy's client leg

## Goal

Let the encapsulated TDS handshake negotiate TLS 1.3 instead of being pinned to
TLS 1.2, without risking the class of failure that pinning avoids.

## Why

`internal/proxy/mssql/tls.go` sets `MaxVersion: tls.VersionTLS12` on the client
leg, and `docs/mssql.md` lists TLS 1.3 as deliberately unsupported in v1.

The reason is the encapsulation. TDS carries the TLS handshake inside
PRELOGIN-typed packets and stops wrapping the moment the handshake completes,
so both peers have to agree on where that moment is. Under TLS 1.2 each side's
handshake ends on a *read*, which lands the framed→raw switch on the same byte
for both. Under TLS 1.3 the client's handshake ends on a *write*, and drivers
disagree about whether that final flight is still encapsulated. The
disagreement presents as a hang, not an error — the worst failure mode to debug
through a real client, and the reason stage 1 pinned the version rather than
guessing.

SQL Server 2022 supports TLS 1.3, and deployments with a TLS-1.3-only policy
will eventually want it. This is a security-posture item, not a functional gap:
every SQL Server client speaks TLS 1.2 today.

## Implementation

1. Establish what the real clients actually do. Drive the proxy with a TLS
   1.3-capable client and capture the bytes: `go-mssqldb`, the Microsoft ODBC
   driver, and the JDBC driver. The question to answer for each is whether the
   client's final CCS+Finished flight arrives inside a PRELOGIN packet or as a
   raw TLS record.
2. `internal/proxy/mssql/tlsconn.go` already flushes any pending packet in
   `deactivate()`, which is the server-side half of what TLS 1.3 needs. The
   missing half is the *read* side: after `HandshakeContext` returns, the
   adapter may need to tolerate the client's last flight arriving either
   framed or raw.
3. If clients disagree, the pragmatic answer is to keep 1.2 as the default and
   put 1.3 behind an opt-in (`DBB_MSSQL_TLS_MAX_VERSION`), rather than making
   the proxy guess per connection.
4. Extend `TestEncapsulatedTLSHandshake` in `tlsconn_test.go` to run the same
   assertions at both versions, and add a TLS 1.3 case to the integration
   suite so a regression shows up as a failed test rather than a hung client.

No GitHub issue exists yet — one should be filed.

## Resolved open questions

**Should a GitHub issue be filed for this spec?**

Decision (2026-08-07, repository owner): **no.** Do not run `gh issue create`.
The spec file is the record.

**Step 1 asks for byte captures from the Microsoft ODBC and JDBC drivers, which
are not available to this automation.**

Proceed on the evidence that *is* obtainable: `go-mssqldb` (already in the
integration suite) plus the encapsulation logic itself. Where a driver cannot be
tested, take the spec's own step 3 as the answer — keep TLS 1.2 as the default
and put 1.3 behind an opt-in `DBB_MSSQL_TLS_MAX_VERSION` rather than guessing
per connection. Record the untested drivers as a caveat in `docs/mssql.md`.
