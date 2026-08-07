# Document every proxy's TLS env vars on the website

## Goal

`website/docs/configuration/index.md` documents **only** the MySQL proxy's TLS
variables. PostgreSQL, MongoDB and SQL Server each have the same
`*_TLS_DISABLE` / `*_TLS_CERT_FILE` / `*_TLS_KEY_FILE` trio and none of them
appear, and SQL Server now has a fourth (`DBB_MSSQL_TLS_MAX_VERSION`) with real
operational consequences.

## Why

The website is the documentation an operator actually reads; the root
`CLAUDE.md` table is a contributor artefact. Right now someone deploying dbbat
in front of SQL Server has no public way to learn that the listener terminates
TLS with a self-signed certificate by default, or that a TLS-1.3-only client
policy needs an opt-in.

`DBB_MSSQL_TLS_MAX_VERSION` makes it worse than a symmetry gap: it is a knob
whose wrong setting produces a *hang* on some drivers, and the caveat (verified
against `go-mssqldb` only, ODBC/JDBC untested) exists nowhere a user will see.

## Implementation

- Add three sections to `website/docs/configuration/index.md` next to the
  existing "MySQL Proxy TLS" one: PostgreSQL, MongoDB, SQL Server. Keep the
  same table shape (Variable / Description / Default).
- The SQL Server section needs the `DBB_MSSQL_TLS_MAX_VERSION` row plus a short
  prose note: default `1.2`, `1.3` is opt-in and verified against `go-mssqldb`
  only, a mismatch presents as a client that connects and then hangs. The long
  form is in `docs/mssql.md` under "TLS version" — link to it rather than
  duplicating the encapsulation explanation.
- Check `website/docs/security.md` too: it discusses TLS termination and may
  want the same cross-reference.
- Source of truth for the variable list is the root `CLAUDE.md` table and
  `internal/config/config.go`.

No GitHub issue exists yet — one should be filed.
