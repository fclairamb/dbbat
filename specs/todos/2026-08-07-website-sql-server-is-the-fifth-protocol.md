# The website still says dbbat speaks four protocols

## Goal

SQL Server is a first-class proxied protocol, but the Docusaurus site under
`website/` was written when there were four. Bring the site up to five
everywhere: the listener variable, the "all four protocols" claims, the
SSH-bastion coverage list, and the supported-databases page.

## Why

`DBB_LISTEN_MSSQL` — the variable that turns the SQL Server listener on and
picks its port — appears **nowhere** on the website. An operator reading
`website/docs/configuration/index.md` sees a Listeners table with PG, Oracle,
MySQL, Mongo and the API, and concludes dbbat cannot proxy SQL Server. Several
other pages state "all four proxied protocols" outright, which is now simply
wrong rather than merely incomplete.

This was noticed while adding the per-proxy TLS sections
(`specs/todos/2026-08-07-website-proxy-tls-env-var-sections.md`), which
documents `DBB_MSSQL_TLS_*` on a page whose listener table has no SQL Server
row — an odd state to leave behind.

## Implementation

- `website/docs/configuration/index.md`
  - Add `DBB_LISTEN_MSSQL` to the **Listeners** table (default `:1434`, empty
    disables). Worth a note that 1434/tcp is free — the SQL Server Browser that
    owns 1434 is UDP-only.
  - Add `listen_mssql: ":1434"` to the YAML config-file example.
- Replace the "four" claims with five, listing SQL Server:
  - `website/docs/intro.md:48` (and the protocol-notes link list around line 39
    — `docs/mssql.md` is missing from it)
  - `website/docs/features/supported-databases.md:19` — plus a SQL Server
    section alongside the Oracle/MySQL/MongoDB ones, linking
    `https://github.com/fclairamb/dbbat/blob/main/docs/mssql.md`
  - `website/docs/features/ssh-tunnels.md:9`
  - `website/docs/configuration/servers.md:174`
  - `website/docs/api/index.md:644`
  - `website/docs/security.md:264`
- Confirm against the code before writing: SSH-bastion support for the MSSQL
  upstream lives in `internal/proxy/upstream/` + `internal/proxy/shared/`. If
  the bastion path does **not** in fact cover SQL Server, say "four" there and
  five elsewhere rather than assuming symmetry.
- Sources of truth: root `CLAUDE.md` env-var table, `internal/config/config.go`,
  `docs/mssql.md`.

No GitHub issue exists yet — one should be filed.
