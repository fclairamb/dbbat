---
model: sonnet
effort: medium
---

# Server names reject `-`, but hyphenated names are the common convention

## Problem

`store.IsValidServerName` enforces `^[a-z0-9_]{1,63}$`
([internal/store/servers.go:26](internal/store/servers.go:26)), so `-` is
refused at creation and at rename with `ErrServerNameInvalid` → 400
([internal/api/servers.go:303](internal/api/servers.go:303),
[:322](internal/api/servers.go:322)).

That was a deliberate call, not an oversight: the "Resolved open questions"
section of [specs/done/2026/08/2026-08-13-23-server-names-must-be-slugs.md](specs/done/2026/08/2026-08-13-23-server-names-must-be-slugs.md)
chose underscore-only over `[a-z0-9_-]` on the grounds that an unquoted `-` is
an operator in MySQL/Oracle identifier positions, and every hyphenated test
fixture (`cluster-scope`, `bastion-excl`, …) was renamed to match. **This spec
reverses that decision**, so it should say so where the rule is documented
rather than quietly widening the regex.

The reversal is justified because the stated risk does not actually apply here.
A dbbat server name is never an *identifier* on the upstream — it is the
client-facing **selector** carried in the connect handshake, and on every one of
the five protocols that field is an opaque string, not a parsed identifier:

- PostgreSQL — startup-message `database` parameter
  ([internal/proxy/postgresql/auth.go:62](internal/proxy/postgresql/auth.go:62))
- MySQL — handshake-response schema field
  ([internal/proxy/mysql/auth.go:189](internal/proxy/mysql/auth.go:189))
- MongoDB — `authSource` / db name
  ([internal/proxy/mongodb/auth.go:258](internal/proxy/mongodb/auth.go:258))
- SQL Server — LOGIN7 database
  ([internal/proxy/mssql/auth.go:148](internal/proxy/mssql/auth.go:148))
- Oracle — the EZ-Connect service part
  ([internal/proxy/oracle/session.go:782](internal/proxy/oracle/session.go:782))

Oracle is the one that had to be checked, and it is already settled the other
way: `isEZConnectSafeName`
([internal/api/connection_url.go:157](internal/api/connection_url.go:157))
explicitly lists `-` among the characters safe to embed verbatim in
`user/key@host:port/name`. So the codebase already holds two rules that
disagree about the hyphen.

Meanwhile `-` is the dominant convention for host/service naming (`prod-eu-1`,
`billing-replica`), and operators hit a 400 for a name their whole fleet uses.

## Proposal

Widen the charset to `[a-z0-9_-]`, keeping the 63-byte cap and the ASCII-only
property (byte length still equals rune count).

**Anchor the hyphen so it cannot lead or trail.** A leading `-` makes the name
unusable on the command line — `psql -d -prod` parses as a flag, likewise
`mysql -D`, `sqlplus`, `mongosh` — which would hand operators a name dbbat
accepts and no client can type. Trailing is barred for symmetry and to keep the
grammar a single readable expression:

```go
var serverNamePattern = regexp.MustCompile(`^[a-z0-9_][a-z0-9_-]{0,61}[a-z0-9_]$|^[a-z0-9_]$`)
```

Keep the existing comment block above it (it explains *why* the format is
constrained at all) and extend it with the hyphen rationale and the
anchoring rule.

Files to change:

- [internal/store/servers.go:19-32](internal/store/servers.go:19) — the pattern
  and its doc comment.
- [internal/store/errors.go:39](internal/store/errors.go:39) — the
  `ErrServerNameInvalid` message spells the regex out to the operator; it must
  match whatever the new expression is, in a form a human can read.
- [internal/api/openapi.yml:4773](internal/api/openapi.yml:4773),
  [:4905](internal/api/openapi.yml:4905) — the `pattern:` on the create and
  rename request schemas; and the prose at
  [:4459](internal/api/openapi.yml:4459) describing the grandfathering rule.
- `front/src/api/schema.ts` — regenerated from the spec, not hand-edited.
- [front/src/routes/\_authenticated/servers/index.tsx:91](front/src/routes/_authenticated/servers/index.tsx:91),
  [:157](front/src/routes/_authenticated/servers/index.tsx:157),
  [:1033](front/src/routes/_authenticated/servers/index.tsx:1033) — the
  client-side `SERVER_NAME_PATTERN` and the two `pattern=` attributes. The UI
  also flags non-conforming grandfathered names; widening the regex silently
  un-flags some of them, which is the desired outcome but worth a glance.
- [specs/done/2026/08/2026-08-13-23-server-names-must-be-slugs.md](specs/done/2026/08/2026-08-13-23-server-names-must-be-slugs.md)
  — append a short note recording that the hyphen decision was reversed here,
  so the resolved-question section does not read as current policy.

**No migration and no data change.** This only widens what is accepted;
every name valid before is still valid, and no stored row becomes invalid.

**Tests.** Extend the table in
[internal/store/servers_test.go](internal/store/servers_test.go) with the new
accept/reject cases — `prod-eu-1` accepted, `-lead` / `trail-` / `--` at the
edges rejected, a 63-byte hyphenated name accepted and 64 rejected — and cover
the rename path too, since it shares the check. Nothing forces the fixtures
renamed by the earlier spec back to hyphens; leave them alone.

## Open question

Should `.` be allowed in the same breath? `isEZConnectSafeName` already treats
it as safe, and dotted names (`prod.eu.1`) are as conventional as hyphenated
ones. Left out here deliberately: `.` is a qualifier separator in SQL
(`schema.table`) and in MongoDB namespaces, so it deserves its own reasoning
rather than riding along on the hyphen. Decide explicitly before implementing
whether to widen once or twice.

## Resolved open questions

> Should `.` be allowed in the same breath? `isEZConnectSafeName` already treats
> it as safe, and dotted names (`prod.eu.1`) are as conventional as hyphenated
> ones. Left out here deliberately: `.` is a qualifier separator in SQL
> (`schema.table`) and in MongoDB namespaces, so it deserves its own reasoning
> rather than riding along on the hyphen. Decide explicitly before implementing
> whether to widen once or twice.

**Decision: widen once, hyphen only. Do NOT allow `.` in this spec.**

Implement exactly the charset in the Proposal —
`^[a-z0-9_][a-z0-9_-]{0,61}[a-z0-9_]$|^[a-z0-9_]$` — and leave `.` rejected.
The dot collides with the qualifier separator in SQL (`schema.table`) and in
MongoDB namespaces (`db.collection`), and a server name is precisely the
client-facing selector carried in those positions, so it needs its own analysis
rather than riding along on the hyphen. If dotted names are wanted later they
get their own spec.

Concretely, for the implementer:

- The doc comment above the pattern, the `ErrServerNameInvalid` message, the
  OpenAPI `pattern:` fields and the frontend `SERVER_NAME_PATTERN` must all
  describe the hyphen-only charset. None of them should mention `.` as accepted.
- Add a reject case for a dotted name (e.g. `prod.eu.1`) to the
  `internal/store/servers_test.go` table, so the exclusion is pinned by a test
  rather than left implicit.
