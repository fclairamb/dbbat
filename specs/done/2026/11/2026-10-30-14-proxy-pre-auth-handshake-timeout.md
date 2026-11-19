---
model: opus
effort: medium
---

# Proxies: unbounded pre-authentication resources (no handshake deadline, compressed frames, early upstream dial)

## Problem

Found by the security scan of 2026-10-30 (finding L3, with the MongoDB and Oracle pre-auth items from the Oracle/MongoDB pass).

Severity: Low. Triggered by an unauthenticated network client.

The SQL Server listener bounds the whole PRELOGIN → TLS → LOGIN7 exchange with a 30s deadline (`handshakeTimeout`, `internal/proxy/mssql/session.go:26-28`, set at `session.go:343`). The PostgreSQL proxy sets no deadline before authentication: `negotiateSSL` (`internal/proxy/postgresql/session.go:1728`) and `authenticate` (`internal/proxy/postgresql/auth.go:17`) block on reads with no time limit. No deadline turned up in the MySQL, MongoDB or Oracle session code either. Stock PostgreSQL bounds this with `authentication_timeout` (60s).

A client can open many TCP connections and send nothing, or a few bytes, and each one holds a goroutine, buffers and a file descriptor indefinitely. That exhausts the process's descriptors for every user of every proxy and of the API.

Two things make each unauthenticated connection more expensive than a socket:

- **MongoDB `OP_COMPRESSED` before authentication.** `decompressMessage` (`internal/proxy/mongodb/wire.go:194-231`) accepts any declared `uncompressedSize` up to `maxWireMessageSize` (48 MB), inflates it, then copies it into a second buffer of the same size. A frame of a few tens of KB of compressed data therefore costs about 96 MB of allocation, repeatable on the same unauthenticated connection and across connections.
- **Oracle dials the upstream before authentication.** `relayPreAuthNegotiation` (`internal/proxy/oracle/relay_preauth.go`) opens the upstream connection, and follows redirects, on the client's TNS Connect, before AUTH. Each idle unauthenticated client therefore also holds an upstream database connection.

## Proposal

1. Add one shared pre-auth deadline (30s, like SQL Server's), set on the accepted connection before the first read in each proxy, and cleared once authentication succeeds. Reuse the SQL Server constant or move it to `internal/proxy/shared`.
2. Apply it to PostgreSQL, MySQL/MariaDB, MongoDB and Oracle. For MySQL the handshake runs inside go-mysql's `NewCustomizedConn`: set the deadline on the raw conn before calling it.
3. MongoDB: refuse `OP_COMPRESSED` before authentication, or cap its `uncompressedSize` well below 48 MB until the session is authenticated (hello and the SASL exchange are small), and inflate with a hard ceiling instead of trusting the header.
4. Oracle: put the same deadline on the pre-auth relay, and consider capping concurrent pre-auth relays, since each one holds an upstream connection.

## Tests

- Per proxy, a unit or integration test that opens a connection, sends nothing, and checks the server closes it after the deadline (with the deadline shortened in tests). This fails before the fix.
- Positive control: a normal login that takes longer than the deadline after authentication (an idle session) is not closed.
- `internal/proxy/mongodb/wire_test.go`: before authentication, an `OP_COMPRESSED` frame declaring more than the pre-auth cap is refused without allocating it. A small compressed hello still works (positive control), and compressed commands after authentication keep the 48 MB limit.

## To verify

- Whether the MySQL, MongoDB and Oracle paths set any deadline in a helper this scan missed (`grep SetDeadline` found none outside SQL Server and Oracle's post-auth reassembly).
