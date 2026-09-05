# Changelog

## [0.26.3](https://github.com/fclairamb/dbbat/compare/v0.26.2...v0.26.3) (2026-09-05)


### Bug Fixes

* **deps:** update module github.com/coreos/go-oidc/v3 to v3.21.0 ([#357](https://github.com/fclairamb/dbbat/issues/357)) ([fecd488](https://github.com/fclairamb/dbbat/commit/fecd48850a3525095a076a54138955174da0c17d))
* **deps:** update module github.com/go-sql-driver/mysql to v1.10.1 ([#354](https://github.com/fclairamb/dbbat/issues/354)) ([b802316](https://github.com/fclairamb/dbbat/commit/b80231609d48f44e5f46fac81078422b66e37501))
* **deps:** update module github.com/microsoft/go-mssqldb to v1.11.0 ([#358](https://github.com/fclairamb/dbbat/issues/358)) ([867b8cd](https://github.com/fclairamb/dbbat/commit/867b8cd9d1e39a89743f8148de8b39d24c636603))
* **deps:** update module github.com/moby/moby/api to v1.56.0 ([#359](https://github.com/fclairamb/dbbat/issues/359)) ([2035bdc](https://github.com/fclairamb/dbbat/commit/2035bdce65e1908f490f437890e6a7f0691ad43f))
* **deps:** update module go.mongodb.org/mongo-driver/v2 to v2.9.0 ([#363](https://github.com/fclairamb/dbbat/issues/363)) ([e8dde86](https://github.com/fclairamb/dbbat/commit/e8dde860cb11270b3daddf802815dff8df21110d))
* **deps:** update module golang.org/x/crypto to v0.56.0 ([#360](https://github.com/fclairamb/dbbat/issues/360)) ([9149805](https://github.com/fclairamb/dbbat/commit/914980543b081a891e8e12f3aba210aeaed69e2d))

## [0.26.2](https://github.com/fclairamb/dbbat/compare/v0.26.1...v0.26.2) (2026-09-02)


### Bug Fixes

* **oracle:** a SQL verb that is merely a *word in a comment* is no longer read as a statement's verb. 0.26.1 taught the header-anchored decode to step over leading comments, but the last-resort keyword scan it used to fall through to was left unchanged — and that scan is still reached whenever an exec header is unreadable, including on the unnameable-piggyback path where a false reading ends the session rather than refusing a call. There, `-- MERGE s'execute` still read as a statement opening at `MERGE`; with the `--` dropped, the apostrophe two words later opened a quoted run that never closed, and a statement that had arrived whole was refused as unreadable. Production shows the same scan gating French prose as SQL: `/queries` recorded `UPDATE d’instances ne pouvait faire le travail --` and `TRUNCATE : la table peut porter le mapping…`. The scan now re-anchors on the comment's opener, so the text handed to the gate is the run the *server* would read. Three bounds keep it from becoming the backward extension the extraction survey rejected: the search never leaves the keyword's own printable run (a `--` on the far side of TTC framing bytes is not a comment over this text), the opener has to be free-standing (`a--b` in an expression is not one), and a run that is comment all the way down falls back to the previous reading — which is refused, because handing the gate verbless text would forward the frame unexamined and a keyword in a comment must not become the way past a control. ([#348](https://github.com/fclairamb/dbbat/issues/348)) ([c8ac89c](https://github.com/fclairamb/dbbat/commit/c8ac89ccf2e5d60702dfca1cb17cb89a0d8ecde0))
* **deps:** update kubernetes monorepo to v0.37.0 ([#346](https://github.com/fclairamb/dbbat/issues/346)) ([cb577bd](https://github.com/fclairamb/dbbat/commit/cb577bdeab250b9f3e0439aff4c619b978719169))

## [0.26.1](https://github.com/fclairamb/dbbat/compare/v0.26.0...v0.26.1) (2026-09-01)


### Bug Fixes

* **oracle:** two ways the statement extractor rejected the very run its exec header named — both verified byte-for-byte in production captures from a python-oracledb thin client, and both funnelling into the same misbehavior: the decode fell back to the last-resort keyword scan and the gate (and `/queries`) got a fragment. First, a statement opening with a SQL comment failed the leading-verb check, so `-- MERGE s'execute` was re-read from the `MERGE` *inside* the comment; with the `--` gone, the apostrophe opened a quoted run that never closed and the client was refused with "a quoted run was left open" for a statement that arrived whole, whatever the grant. The verb check now steps over leading `--` and `/* … */` comments, exactly as the server does. Second, a statement past 32767 bytes travels in the CLR long form — `0xFE` marker, 32767-byte chunks, a compressed-int length prefix *in the middle of the text* — so the contiguous run of exactly `sqlLen` bytes structurally does not exist, and the reported failure boundary sat at exactly 32768: refused when the keyword scan's cut landed inside a string literal, silently gated-and-recorded short otherwise, which also left blocked/approval patterns past the first chunk unseen. The locate now walks the long form in both chunk-length encodings (the same pair the AUTH leg reads), accepting only chunks that concatenate to exactly `sqlLen` printable bytes behind a verb, and reports where the statement's wire bytes end so bind capture keeps its floor on a statement that can no longer be anchored by byte search. Replayed against the captured frames (37 B repro up to a 117 KB MERGE): all decode whole; large generated statements no longer need splitting into batches. ([#344](https://github.com/fclairamb/dbbat/issues/344)) ([748606a](https://github.com/fclairamb/dbbat/commit/748606a0d71fc063cbf2c730fcd0eb7f9986e605))

## [0.26.0](https://github.com/fclairamb/dbbat/compare/v0.25.2...v0.26.0) (2026-08-31)


### Features

* **api, ui:** connections and queries filter server-side on the dimensions people actually investigate by — `server_group_uid` (live membership, so an empty group yields zero rows rather than everything), `grant_uid`, `grant_definition_uid` (matched across the definition's `lineage_uid`, so an edit-archival no longer splits a grant's history in two), and `grant_provenance` (`approved`/`auto`/`direct`, where a connection with no grant matches *no* value; `direct` means "no approval on record", not "provably admin-issued"). `/queries` additionally filters on `approval_status`, kept deliberately distinct from grant provenance. Both listings share one filter builder so they cannot drift apart, malformed UUIDs on the new params are a 400 while the legacy `user_id`/`database_id` keep their silent-ignore, and the connector scoping overwrite still runs strictly last so a connector cannot widen its own scope with a crafted `user_id`. The users view gains `last_login_at` — stamped on interactive logins only (password and both OAuth paths, never a JWT refresh, API key or MCP call) and never able to fail a login — plus a bulk DB-computed `GET /users/last-connections`. Filter state lives in URL search params and resets the pagination cursor on change. ([#337](https://github.com/fclairamb/dbbat/issues/337)) ([de3f57f](https://github.com/fclairamb/dbbat/commit/de3f57fcd2d26764e4ae86f21d2262118ffe1038))


### Bug Fixes

* **security (oracle):** an Oracle statement larger than the negotiated SDU (~8.1 KB of SQL at the 8192 default) was gated on its **first TNS fragment only**. TTC carries no message-length field and, unlike the AUTH phase, the data phase never reassembled — so everything the gate matches beyond the leading verb was evadable by padding a statement past the SDU: `oracleBlockedPatterns` (`UTL_HTTP`, `DBMS_SCHEDULER`, …), approval-hold patterns, and the dynamic-SQL scan. Under a `read_only` grant, `BEGIN NULL; /* ~8 KB of padding */ EXECUTE IMMEDIATE 'DROP ...'; END;` reached the upstream whole, because the controls only ever saw the padding. The same truncation was an audit hole — `/queries` stored the prefix as if it were the statement, and the per-connection chain MACs sealed it — and, when the prefix happened to end inside a string literal, the cause of the reported incident: a misleading `ORA-01031 … dynamic SQL that is itself built from dynamic SQL` refusal for statements containing none, after which the orphaned continuation packet was forwarded upstream alone and desynced the session. Statement-carrying messages are now reassembled before gating; an allowed statement's original packets are forwarded byte-unchanged, and a refused one has every fragment dropped and exactly one OER answered, so the session survives and the next call proceeds. Reassembly is bounded and fails closed — capped at `execMaxSQLLen` plus slack, bounded by a read deadline, and grown as packets actually arrive, since the declared length is attacker-controlled input. A statement dbbat still cannot read to its end is no longer validated as if complete: it is refused honestly under statement-shaped controls and recorded as partial otherwise, the fallback extractor no longer mutilates non-ASCII statements at the first byte above `0x7E`, and "the scan fell off the end of the text" is now its own error instead of being reported as nested dynamic SQL. ([#341](https://github.com/fclairamb/dbbat/issues/341)) ([36c88df](https://github.com/fclairamb/dbbat/commit/36c88dfb6d6b18cf8ff77f74079df7a01c3acce6))
* **dump:** session captures now contain the refusal frames dbbat synthesizes itself, not only the bytes relayed from the upstream. A capture is forensic evidence and a refusal is precisely the event an investigator replays one to see, yet a refused statement showed the statement and then silence — which reads as a dropped connection rather than an enforced control. Oracle, MongoDB and PostgreSQL all had the gap, each now recorded through the dump tap with exactly one recording point per direction; PostgreSQL was additionally recording every relayed frame twice, and its previous tap wiring silently dropped client bytes that were already buffered when the capture opened. MySQL and SQL Server were already correct. Captures are confirmed to record plaintext above TLS — a claim that had rested on one reading of a dependency's internals, and that a comment in the MySQL proxy asserted backwards, steering operators away from capturing exactly the sessions they most want captured; it is now pinned by tests driving real TLS handshakes. Frames written before authentication succeeds remain uncaptured by construction on all five protocols: the capture file is named after a connection UID that does not exist until then, so a refused login has no capture at all. ([#341](https://github.com/fclairamb/dbbat/issues/341)) ([36c88df](https://github.com/fclairamb/dbbat/commit/36c88dfb6d6b18cf8ff77f74079df7a01c3acce6))
* **deps:** update module github.com/stretchr/testify to v1.12.1 ([#334](https://github.com/fclairamb/dbbat/issues/334)) ([4424f3a](https://github.com/fclairamb/dbbat/commit/4424f3af4bbab29463f4b7613f4627d668d482a0))
* **deps:** update module go.mongodb.org/mongo-driver/v2 to v2.8.2 ([#338](https://github.com/fclairamb/dbbat/issues/338)) ([30c9e9c](https://github.com/fclairamb/dbbat/commit/30c9e9c0ce267f436fbfab325899e8ad032df4af))

## [0.25.2](https://github.com/fclairamb/dbbat/compare/v0.25.1...v0.25.2) (2026-08-17)


### Bug Fixes

* **proxy:** a load balancer's TCP health check — open the socket, close it without sending a byte — is no longer logged as a session failure on any of the five listeners. Behind an NLB the PostgreSQL listener alone produced ~22 identical `ERROR "Session error"` lines every 3 minutes, which is exactly how a reader learns to ignore the `ERROR` level. A shared sentinel now marks the one demoted case, and it is deliberately narrow: the demotion is keyed on the session's own client-read counter being **zero**, so a client that hung up halfway through its startup packet is a truncated client and keeps its loud line. Every quieted probe stays observable at `DEBUG` with its remote address, and the paired "session ended" line is replaced rather than doubled. Oracle gains the most: its previous `strings.Contains(err, "EOF")` demotion also swallowed `unexpected EOF` and every connect-packet read failure whatever the cause — those are errors again. ([#332](https://github.com/fclairamb/dbbat/issues/332)) ([6bf0b96](https://github.com/fclairamb/dbbat/commit/6bf0b961df489c47ca625326a0abaefc486bf098))
* **deps:** update kubernetes monorepo to v0.36.3 ([#319](https://github.com/fclairamb/dbbat/issues/319)) ([4fb5a69](https://github.com/fclairamb/dbbat/commit/4fb5a6993117266dce7b733c509a9d5a9c19ce2b))
* **deps:** update module github.com/slack-go/slack to v0.29.0 ([#327](https://github.com/fclairamb/dbbat/issues/327)) ([2d1fc5f](https://github.com/fclairamb/dbbat/commit/2d1fc5fbc688a739df5e988a508a3ac31f15d2c5))
* **deps:** update module github.com/stretchr/testify to v1.12.0 ([#331](https://github.com/fclairamb/dbbat/issues/331)) ([f02bceb](https://github.com/fclairamb/dbbat/commit/f02bceb4d11afd4c50904502d13d4ad31bbc8ec6))
* **deps:** update module github.com/urfave/cli/v3 to v3.11.0 ([#330](https://github.com/fclairamb/dbbat/issues/330)) ([2f466dd](https://github.com/fclairamb/dbbat/commit/2f466dd09687503a04a6adf68dfa41d8e6a82a79))

## [0.25.1](https://github.com/fclairamb/dbbat/compare/v0.25.0...v0.25.1) (2026-08-15)


### Bug Fixes

* **store:** a demo instance no longer reports its own query chain as broken. The demo seeder closed its sessions with a raw `UPDATE`, bypassing the only writer that seals a session's chain head — and a closed session holding statements but no head stamp is, by design, indistinguishable from someone deleting the stamp to hide trailing deletions, so it verified as a break on every boot. Seeding now goes through `CreateConnectionAt` / `CloseConnectionAt`, which share the real create and close routines rather than reimplementing them, so the chain is sealed exactly as a live session's is and both the `connection.opened` and `connection.closed` evidence entries carry the session's staged historical timestamp instead of disagreeing with the row on wall-clock time. ([#325](https://github.com/fclairamb/dbbat/issues/325)) ([a72ff74](https://github.com/fclairamb/dbbat/commit/a72ff748e4000e511ce42d9195ad77bb34f61326))

## [0.25.0](https://github.com/fclairamb/dbbat/compare/v0.24.0...v0.25.0) (2026-08-15)


Everything below landed as one squashed batch ([#320](https://github.com/fclairamb/dbbat/issues/320)) ([fab64ac](https://github.com/fclairamb/dbbat/commit/fab64acea9c390dc30a1387ac1355eba3b625fbe)).

### ⚠ BREAKING CHANGES

* **api:** the pre-rename `group_uids` / `approver_group_uids` input shims are retired. Send `user_group_uids` / `approver_user_group_uids`; the old spellings are refused with a 400 naming their replacement rather than silently ignored, because dropping a scope restriction on the floor would fail open.
* **api:** `legacy_stamps` is gone from `GET /audit/verify/queries`.
* **store:** a pre-0.24 unkeyed query-chain head stamp is now reported as a **break**, not a weaker pass, with no opt-out on either the CLI or the REST surface. Only a store written by a pre-0.24 development build can hold one, and such a store stays unverifiable by design: tolerating it was a standing downgrade path, since replacing a sealed stamp with a raw head MAC needs no key.

### Features

* **proxy:** reach databases inside a Kubernetes cluster through a `pods/portforward` tunnel, with the API server's CA captured and pinned on first connect, a changed CA reported by the connectivity check, and cluster rows surfaced in the API and UI.
* **auth:** a generic OIDC SSO provider — any issuer, verified ID tokens, PKCE — with dbbat roles resolved from the directory's groups claim on *every* login, per-provider auto-provisioning, and Entra's groups-overage claim detected and treated as *unknown* rather than empty so a login can never silently strip roles.
* **grants:** grant definitions scope on **server groups** instead of enumerating databases, and grants bind to the group that covers the target. Membership resolves live, so adding a server to a group widens every grant bound to it with no edit — and, because definitions are immutably versioned, no new definition version. One quota budget is shared across the whole group.
* **api:** access approvers and query approvers live on servers and server groups, resolved through a single fallback chain, so someone other than an admin can decide grant requests and release approval holds. Each pending item reports which hat the caller wears.
* **api:** a governed MCP endpoint for AI agents that executes statements by dialing dbbat's *own* proxy listener over loopback — never a parallel internal path — so grants, quotas, logging and approval holds apply unchanged across all five protocols.
* **store:** captured result rows are HMAC-chained, one chain per capture, sealed at the flush barrier; open sessions get their chain stamp swept forward and a crash orphan is sealed by the reconcile. Verification is exposed over the REST API alongside the audit and query chains.
* **oracle:** the synthetic AUTH fallback now covers wide/OCI clients, the fixed-width OCI summary object is read as well as written, and service names claimed by two upstream spellings are detected and flagged.
* **ui:** server groups, Kubernetes clusters, per-provider login buttons, directory-managed role badges, server rename, audit-chain verification, Oracle-capability on API keys, and a request dialog narrowed to what the requester can actually ask for.
* **deploy:** the `demo.dbbat.com` deployment is reproducible from `charts/dbbat` with a committed values file — extra ingresses, the Traefik redirect middleware, and a `GOMEMLIMIT` ceiling included.

### Bug Fixes

* **security (postgresql, mysql, mongodb):** cap length-prefixed reads *before* they size an allocation on the unauthenticated path. A PostgreSQL startup packet declaring `0xffffffff` asked the runtime for ~4 GiB from four bytes on a listener open to the internet — an instant OOM from a single connection that never authenticates. MySQL and MongoDB carried the same shape as amplification (16 MiB per connection from a 4-byte header; 48 MB pre-sized from a 16-byte header). SQL Server and Oracle are bounded by field width and now have tests pinning that.
* **proxy:** close several cross-protocol ways a statement could leave its granted database — a text `USE`, a `USE` hidden in `PREPARE ... FROM '<literal>'`, `EXEC('...')` and `sp_executesql` dynamic SQL, and a MongoDB `$out`/`$merge` target — plus `read_only` and `block_ddl` now reaching inside dynamic SQL, and validators matching a comment-stripped copy so an executable comment cannot smuggle one past.
* **proxy:** every detached store write and session goroutine on all five protocols is panic-guarded, and a recovered watchdog panic ends the session instead of quietly killing one goroutine.
* **store:** grant windows and short-lived TTLs are stamped from the database clock rather than the process clock, a chain append retries when a peer replica moves the cached head, and a later writer can never lower a chain stamp.
* **grants:** a group-bound grant covers its group, not its group *plus* its anchor database.
* **ui:** the create dialogs on the servers and API-keys pages render only while open. A closed Radix dialog keeps its full-viewport overlay in the DOM through its exit animation, so a second click landed on the dying overlay and toggled the dialog back shut — and, left mounted, the form reopened pre-filled with the previously created server's credentials, password included.
* **mongodb:** legacy wire opcodes are refused after authentication, and relay teardown no longer races the peer pump.

## [0.24.0](https://github.com/fclairamb/dbbat/compare/v0.23.2...v0.24.0) (2026-08-14)


### ⚠ BREAKING CHANGES

* **grants:** a grant definition scopes on server groups instead of enumerating databases, and a bare "group" no longer exists in the API. `database_uids` becomes `server_group_uids`: a definition names stable sets of servers, so adding a server to the fleet stops meaning "edit every relevant definition" — and, because definitions are immutably versioned, "archive and re-insert every one of them". Membership resolves live, exactly as the user-group scope already did, so a server added to a scoped group is requestable immediately with no edit and therefore no new definition version; empty still means every database. Because two kinds of group now exist, `group_uids` and `approver_group_uids` are renamed `user_group_uids` and `approver_user_group_uids` on grant definitions, `group_uids` becomes `user_group_uids` on the update-user body, and the user detail response's `groups` key becomes `user_groups`. Every retired spelling, `database_uids` included, is refused with a 400 naming its replacement rather than silently ignored, because dropping a scope restriction on the floor would fail open. A migration mirrors every existing per-database scope into a real server group — one per *distinct* set of databases, named after the definition that first used it — and is idempotent and reversible.

### Features

* **api:** AI agents can query through dbbat over MCP, governed exactly like a human session. A Streamable-HTTP Model Context Protocol server is mounted at `POST /api/v1/mcp`, authenticated with an ordinary `dbb_` API key, offering `list_databases`, `query`, `describe` and `await_approval`. Every statement an agent runs is executed by dialing dbbat's *own* proxy listener over loopback as the key's owner — there is deliberately no internal execution path, so auth, grants, `read_only` / `block_ddl` / `block_copy`, quotas, query logging and the mid-flight approval gate are the same code reached over the same wire and this endpoint cannot drift from them. All five protocols are covered; on MongoDB the statement is `<command> <extended JSON>` rather than SQL, which keeps the tool surface at four and keeps an approval pattern meaning one thing whether the command came from mongosh or from an agent. A statement still parked after a short grace window returns a structured `approval_pending` result naming the held query, and `await_approval` long-polls it, so an agent never silently times out on a hold — every return names the next action. `DBB_MCP_ENABLED` defaults to true; `false` removes the routes entirely. ([01314ac](https://github.com/fclairamb/dbbat/commit/01314ac9aefa5ef4ea92ebc8b391f04f5decc535))
* **api:** all three HMAC chains can be verified over the REST API and from the admin UI. `GET /api/v1/audit/verify`, `/audit/verify/queries` and `/audit/verify/rows` are admin-only and return the same numbers as the CLI — counts, the head MAC in hex, the pre-anchor unverifiable count and the first break — and never the key or the content of an audited record; the row endpoint narrows on `?connection=` or `?query=`, which cannot be combined. A walk is O(rows), so each scope's outcome is cached for a minute and one walk at a time is admitted per instance; a windowed resume was rejected because starting from a caller-supplied position trusts a `prev_mac` an attacker controls. The audit page gains a Chain verification panel that walks all three, renders a break loudly with its `chain_seq`, uid and reason, and offers the head MAC with a copy button because the head is meant to be recorded outside the database. The panel also says what the answer is worth: it is served by the process under audit, the result may be up to a minute old, and `dbbat audit verify` is what someone who does not trust this server runs. ([01314ac](https://github.com/fclairamb/dbbat/commit/01314ac9aefa5ef4ea92ebc8b391f04f5decc535))
* **api:** the two kinds of approver live on the fleet, not on the policy. Servers and server groups each carry an access-approver list (who may decide **grant requests** for that database) and a query-approver list (who may release **approval holds** on statements against it); neither implies the other, and an org wanting overlap lists the same user group in both. Resolution is one fallback chain implemented in one place, because a second SQL-shaped copy of it is exactly the drift that would be an authorization bug: for a grant request, the server's list, then the union of its server groups' lists, then admins; for a hold, the definition's own approver list wins outright when non-empty, then the same chain. Empty everywhere is admin-only, which is precisely the previous behaviour, so nothing changes until an approver group is named. The lists are read at decision time and never snapshotted, so an edit — or moving a server between groups — immediately changes who may decide requests already filed and statements already parked; a departed lead's replacement is effective now. Self-approval is refused on every path, both Slack transports included, and each pending item reports which hat the caller wears (`admin`, `definition_approver`, `server_approver`) so the UI can say why they may decide it. ([01314ac](https://github.com/fclairamb/dbbat/commit/01314ac9aefa5ef4ea92ebc8b391f04f5decc535))
* **auth:** any OIDC issuer can be dbbat's identity provider, not just Slack. `DBB_OIDC_ISSUER` plus a client id and secret enables a generic provider that works against Google Workspace, Okta, Microsoft Entra, Keycloak or Authentik, registered under the `oidc` provider key with its own callback. Unlike the Slack provider, which trusts a userInfo endpoint, this one only accepts an identity carried by an ID token whose signature, issuer, audience and expiry were verified against the issuer's JWKS, and the optional `DBB_OIDC_EMAIL_DOMAINS` allowlist is checked against that verified email claim — which matters on a multi-tenant issuer, where "any account the issuer vouches for" means any account on the internet. PKCE (S256) is used on every flow, with the verifier carried on the existing OAuth state row so a callback landing on another replica still works. Discovery is lazy, so an unreachable IdP delays the first login instead of blocking startup, and the login screen renders one button per configured provider with the label the operator chose. ([01314ac](https://github.com/fclairamb/dbbat/commit/01314ac9aefa5ef4ea92ebc8b391f04f5decc535))
* **auth:** dbbat roles can follow directory group membership, resolved on every login. `DBB_OIDC_ROLE_MAPPING` binds roles to the groups in the ID token's groups claim (`admin=db-admins,viewer=analysts`), and because it is applied at every sign-in rather than only at account creation, an admin who leaves the directory group loses the role at their next login with nobody remembering to click. It is authoritative only for the roles it names — a role granted by hand in the UI is never revoked by a login — the default role is the floor when nothing matches, and the last remaining admin is retained exactly as user management already refuses to strip it. Values match exactly, case included, because Entra sends group object ids rather than names, and both claim encodings are accepted while a lone string is one group and never a delimited list. A token that delegates the groups claim through `_claim_names` — Entra's groups overage, past roughly 200 memberships — is read as membership *unknown* rather than empty: the login succeeds, the mapping is skipped entirely, roles are left untouched with no default-role floor, and a WARN names the user and the claim. dbbat never follows the pointer to Microsoft Graph. Every resolved change writes a `user.roles_synced` audit entry carrying the groups that caused it, the users page badges a directory-managed role and warns before editing one, and a dedicated endpoint reports the last sync per user exactly — the page used to derive it from the newest 200 audit entries, so a user whose sync had aged out rendered as "Never synced", indistinguishable from one the directory never touched. ([01314ac](https://github.com/fclairamb/dbbat/commit/01314ac9aefa5ef4ea92ebc8b391f04f5decc535))
* **config:** OAuth auto-provisioning is provider-agnostic, and can be overridden per provider. `DBB_AUTH_AUTO_CREATE_USERS` and `DBB_AUTH_DEFAULT_ROLE` replace the Slack-shaped names an OIDC-only operator had to discover; the legacy `DBB_SLACK_AUTH_*` spellings stay accepted as instance-wide aliases, with the canonical setting winning against whichever source each was actually configured from. `DBB_AUTH_AUTO_CREATE_USERS_<PROVIDER>` and `DBB_AUTH_DEFAULT_ROLE_<PROVIDER>` then override both per provider, resolving per-provider, then instance-wide, then the default — which is what lets a deployment trust a tightly-gated Entra tenant to mint accounts while refusing the same from a Slack workspace full of contractors. Gating a provider blocks account *creation* only; an account that already exists can still sign in through it. Two things fail closed at startup rather than becoming an override that quietly does not apply: a default role is validated against the known roles exactly, `Admin` refused rather than folded to `admin`, and a provider name outside the known set is a startup error. ([01314ac](https://github.com/fclairamb/dbbat/commit/01314ac9aefa5ef4ea92ebc8b391f04f5decc535))
* **grants:** a grant binds to a server group, and its quotas and priority span that group. A grant keeps an anchor, the database it was issued for, but when its definition scopes on server groups it binds to whichever of those groups currently holds that database and then covers exactly what the group holds right now — so adding a server extends every already-live grant bound to it, with no re-issuance and no new grant row, and removing one narrows them, the anchor included. Membership is read live and never snapshotted: the deliberate exception to "a live grant's behaviour never changes under it", accepted because group membership is operational data, with the admin UI warning about the blast radius at the point of edit. One `max_query_counts` and one `max_bytes_transferred` budget is consumed across the whole group, and `priority` ranks group-bound grants against each other where their groups overlap. The auth path changes in exactly one place — the single function all five protocols resolve their session's grant through — so PostgreSQL, Oracle, MySQL, MongoDB and SQL Server are covered at once, and the listing endpoint shares the same predicate so the UI cannot list grants the proxy would not pick. The anchor keeps two jobs: it is what an *unbound* grant covers, and it is the fallback if the group is deleted outright, which narrows access back to where it started and never widens it. ([01314ac](https://github.com/fclairamb/dbbat/commit/01314ac9aefa5ef4ea92ebc8b391f04f5decc535))
* **grants:** a grant definition scopes on server groups instead of enumerating databases, and a bare "group" no longer exists in the API. `database_uids` becomes `server_group_uids`: a definition names stable sets of servers, so adding a server to the fleet stops meaning "edit every relevant definition" — and, because definitions are immutably versioned, "archive and re-insert every one of them". Membership resolves live, exactly as the user-group scope already did, so a server added to a scoped group is requestable immediately with no edit and therefore no new definition version; empty still means every database. Because two kinds of group now exist, `group_uids` and `approver_group_uids` are renamed `user_group_uids` and `approver_user_group_uids` on grant definitions, `group_uids` becomes `user_group_uids` on the update-user body, and the user detail response's `groups` key becomes `user_groups`. Every retired spelling, `database_uids` included, is refused with a 400 naming its replacement rather than silently ignored, because dropping a scope restriction on the floor would fail open. A migration mirrors every existing per-database scope into a real server group — one per *distinct* set of databases, named after the definition that first used it — and is idempotent and reversible. ([01314ac](https://github.com/fclairamb/dbbat/commit/01314ac9aefa5ef4ea92ebc8b391f04f5decc535))
* **oracle:** a held mid-reply refusal records what its handoff actually cost, and names the case where its two fail-safes cross. Every exit now logs the two quantities the bounds exist to cap — the bytes relayed since the hold began, and how long the client took to announce its boundary — where both were previously inferred from a client's row count, which can say neither. The bounds themselves meet at about 280 KiB/s: below that the flat 30-second grace runs out before the byte bound can, so on a slow link a handoff that was going to succeed is cut by the clock, and the symptom was indistinguishable from the three other fail-safes firing. Nothing is cut differently — no bound moves, this is report-only — but the grace-expiry path now emits a WARN naming the crossover, and only when the abandoned hold was still being fed right up to the deadline and the byte bound was out of reach at the observed rate. ([01314ac](https://github.com/fclairamb/dbbat/commit/01314ac9aefa5ef4ea92ebc8b391f04f5decc535))
* **oracle:** the synthetic AUTH fallback covers wide/OCI clients, and every AUTH path speaks the session's negotiated chunk form. The synthetic builders emitted only the thin, compressed encoding, but an OCI client — sqlplus, the Instant Client — negotiates a different dialect during the pre-auth relay and the upstream parses AUTH at those capabilities, so a thin body handed to an OCI-conditioned upstream is unreadable and draws two break markers and `ORA-03120`; the fallback was a safety net for thin clients only. The wide form is built from a real capture rather than from first principles, because two of its five differences cannot be derived: every key and value length is a 4-byte little-endian field carrying a UTF-8 max-expansion buffer size on the Instant Client but a plain length on the DB-bundled client (mixing the two conventions draws `ORA-28041`), and an empty value is four zero bytes followed straight by the flag with no character-set byte at all. The data flags, the pointer-run preamble and a logon mode carrying client-specific high bits are taken from the client rather than invented. Separately, the client challenge, both rewrite fallbacks, the thin and wide synthetic builders and the Phase 2 finders now all read and write the long chunk form when the session negotiated it, which is read from the capability byte clients actually read. ([01314ac](https://github.com/fclairamb/dbbat/commit/01314ac9aefa5ef4ea92ebc8b391f04f5decc535))
* **proxy:** a connection-setup `ALTER SESSION SET …` is allowed under `read_only` and `block_ddl` when every parameter it sets is allowlisted. Once the Oracle gate could see a full statement rather than a fragment, DBeaver's connection setup — `ALTER SESSION SET CURRENT_SCHEMA=…` and its `NLS_…` siblings — started tripping both controls, because `ALTER` is in the write and the DDL keyword lists. A multi-parameter statement is refused whole unless every parameter on it is on the list, and `CONTAINER` is deliberately not on it and is separately blocked outright, so the allowlist and the block cannot disagree. ([01314ac](https://github.com/fclairamb/dbbat/commit/01314ac9aefa5ef4ea92ebc8b391f04f5decc535))
* **proxy:** a database running in a Kubernetes pod can be proxied without exposing it, through a new `kubernetes` tunnel protocol. A server row with protocol `kubernetes` carries the API server, a ServiceAccount bearer token and a namespace, and — exactly like an `ssh` bastion row — is a `via` dial path rather than something a grant can ever be issued on. The target row's host is a pod name or `svc/<name>` and its port is the container port, because the tunnel is a `pods/portforward` stream to the pod's *own* port: a database that is merely routable from the cluster network is out of scope, and the server form states that where an operator will read it. Tunnels are pooled per server exactly as SSH clients are, upstream TLS still applies inside, and a cluster whose API server itself sits behind an SSH bastion is supported (forcing the SPDY transport, the only one that takes a dial function) while the reverse nesting is refused explicitly. The CA bundle is optional: with none supplied the API server's CA is pinned on first connect and persisted separately from an operator-supplied one, the connectivity check reports both the learned pin and a CA that has since changed, and the UI surfaces the pin, the new protocol and a Tunnel Servers table listing both kinds of dial path. The whole path is exercised end to end against a real k3s cluster, including a pod deleted mid-suite. ([01314ac](https://github.com/fclairamb/dbbat/commit/01314ac9aefa5ef4ea92ebc8b391f04f5decc535))
* **store:** `audit_log` and per-connection query history are sealed with an HMAC chain, so a deletion or an edit cannot go unnoticed. Every audit entry and every `queries` row carries a MAC over its own content plus the previous record's MAC, keyed by an HKDF subkey of `DBB_KEY` that is derived per purpose and never stored in the database — so an attacker holding the store cannot recompute a chain. `audit_log` is one chain; `queries` is one chain per connection, a scope chosen so `DBB_QUERY_STORAGE_RETENTION` can never sever a chain by deleting exactly what it is meant to delete. `dbbat audit verify` walks the audit chain and `dbbat audit verify --queries` the per-connection ones, reporting counts, the head MAC and the first break, with a non-zero exit on a break. An append whose cached head a peer replica has since moved retries rather than failing, so replicas sharing a store stay correct. ([01314ac](https://github.com/fclairamb/dbbat/commit/01314ac9aefa5ef4ea92ebc8b391f04f5decc535))
* **store:** a session's query-chain head stamp is a keyed, versioned seal that is refreshed on live sessions, and a cleared one is a break. The stamp is what catches a deletion from the *end* of a session's history, and a verbatim head MAC is readable out of the table it came from, so copying one after a trailing deletion needed no key: it is now a keyed MAC over the head, carrying a format version inside its own MAC so a sealed row cannot be relabelled to earn a weaker rule. The reconcile seals a crash-orphaned session in the same transaction that writes `disconnected_at`, and a third writer sweeps this run's still-open sessions on the reclaim tick, which is what bounds how long a live session's tail goes unsealed; verification follows that distinction, judging an open session's stamp as a prefix and going back to exact the moment it closes. Clearing the stamp used to be cheaper than forging one — a single `UPDATE … SET query_chain_mac = NULL` read as "never stamped" and the session verified — so a NULL stamp is now judged rather than skipped: not a break for a session that logged nothing or one younger than a sweep, a break for a closed session whose chained statements survive, and a break with its own reason when a chain length outlived its MAC. The walk enumerates stamped connections rather than only connections still present in `queries`, so a session whose statements were deleted *in full* is judged instead of skipped, excused only when its `connected_at` precedes the configured retention cutoff. ([01314ac](https://github.com/fclairamb/dbbat/commit/01314ac9aefa5ef4ea92ebc8b391f04f5decc535))
* **store:** captured result rows are chained too, one chain per capture. `query_rows` carries the same HMAC construction, positioned by `row_number` — an ordering rather than a dense sequence, because dropped or unencodable rows leave gaps — and the chain is sealed at the flush barrier when the capture finishes, so what the proxy stored of a result set is as tamper-evident as the statement that produced it. The batched writer still lands ~1000 rows in a single bulk `INSERT` inside one transaction, so the hottest write path keeps amortising its round trip. Verify with `dbbat audit verify --rows`, optionally narrowed to one connection. ([01314ac](https://github.com/fclairamb/dbbat/commit/01314ac9aefa5ef4ea92ebc8b391f04f5decc535))
* **store:** every session open and close is recorded in the audit chain, so deleting a whole session leaves evidence. `connections` carries no MAC and deliberately never will — retention deletes from it, and a chain over it would report a truncated prefix after every sweep — which left `DELETE FROM connections WHERE uid = …` removing a session, every statement it ran and every row it captured, cascading through both child tables and breaking nothing a chain walk checks. It was the last uncovered deletion and the cheapest attack on query history. The evidence now goes where the delete cannot reach it: a `connection.opened` entry at creation and a `connection.closed` entry from whichever writer closes the session — the normal path or the crash reconcile, recorded as `closed_by` — carrying the row's immutable identity (connection uid, user, database, source IP, `connected_at`, instance and run, grant) plus, on close, `disconnected_at` and the session's sealed query-chain head. Mutable counters stay out. The write is never fatal to a live session and never joins the caller's transaction. Volume being what it is, both event types are excluded from an unfiltered `GET /api/v1/audit` and reached with `?event_type=`. ([01314ac](https://github.com/fclairamb/dbbat/commit/01314ac9aefa5ef4ea92ebc8b391f04f5decc535))
* **ui:** deactivated grant definitions are hidden behind a toggle. The admin list fetches active definitions by default and a "Show deactivated" switch in the page header flips it back, keeping the existing badge for the mixed view — so a long-lived instance's list reflects what can actually be assigned today rather than everything it has ever withdrawn. ([01314ac](https://github.com/fclairamb/dbbat/commit/01314ac9aefa5ef4ea92ebc8b391f04f5decc535))
* **ui:** the access-request dialog offers only the databases a definition actually covers, for every requester rather than only for admins. It resolved scope through an admin-only endpoint, which returned nothing to a non-admin and therefore offered them every database in the fleet — a picker full of choices the server would reject on submit. Grant definitions now carry the concrete set of database uids their scoped server groups currently hold, resolved in one batched query per response, with `null` meaning unscoped. This is a convenience only: the server still re-resolves scope on submit and remains the gate. ([01314ac](https://github.com/fclairamb/dbbat/commit/01314ac9aefa5ef4ea92ebc8b391f04f5decc535))


### Bug Fixes

* **deps:** update module github.com/gopacket/gopacket to v1.7.1 ([#310](https://github.com/fclairamb/dbbat/issues/310)) ([7b1c83e](https://github.com/fclairamb/dbbat/commit/7b1c83e9f48e3cc926c521d8875f0ddd2393ba6e))
* **deps:** update module golang.org/x/crypto to v0.55.0 ([#312](https://github.com/fclairamb/dbbat/issues/312)) ([97e049b](https://github.com/fclairamb/dbbat/commit/97e049b64b4fa41577df93f19ef66c4d66667d6b))
* **mongodb:** an aggregation pipeline can no longer reach a database the grant does not cover, and one dbbat cannot fully read now fails closed. `$out` and `$merge` accept an explicit `{db, coll}` target that the message's `$db` never reveals, so under any grant that is not `read_only` a write landed in a database the grant does not cover while the `$db` check passed honestly and `queries` attributed it to the granted database; `$lookup`, `$graphLookup` and `$unionWith` take the same shape on the read side, where `read_only` never looked at all. Every one of them is now held to the same per-message database policy and refused with the usual Unauthorized (13). More seriously, the nested-pipeline scan gave up at its depth cap and on any parse failure and reported an empty result — and the same walk is what decides whether a command writes, so a `$merge` nested past the cap was classified as a *read* and ran under `read_only` (measured at depths 9 through 12). Every give-up path now yields a refusal, the scan runs once up front before any grant control is consulted, so no grant can permit a command whose effect could not be established, and `explain`, which wraps a whole command in a nested document, is descended into. A benign pipeline within the cap is unaffected, and the string forms of those stages, which name the same database, stay allowed. ([01314ac](https://github.com/fclairamb/dbbat/commit/01314ac9aefa5ef4ea92ebc8b391f04f5decc535))
* **mongodb:** legacy wire opcodes are refused after authentication. Anything that was not `OP_MSG` was forwarded verbatim, so a hand-crafted `OP_QUERY` against `otherdb.$cmd` never reached validation at all — no database check, no `read_only` check, and nothing shaped like a statement in the query log. `OP_QUERY` and `OP_GET_MORE` now receive an Unauthorized (13) `OP_REPLY`, the fire-and-forget legacy writes are dropped, and the attempt is recorded as a query; the refusal is independent of grant controls, like the database check itself, and the pre-auth handshake, which legitimately uses `OP_QUERY` for the first `hello`, is untouched. This is a deliberate compatibility change rather than a pure fix: MongoDB removed these opcodes in 5.1 and a modern driver against a supported server never sends them, but a hand-crafted legacy client talking to a MongoDB older than 5.1 through the proxy will now be refused. Parsing a path MongoDB itself deleted was weighed and rejected. ([01314ac](https://github.com/fclairamb/dbbat/commit/01314ac9aefa5ef4ea92ebc8b391f04f5decc535))
* **mongodb:** tearing down a relay no longer races the pump that is still running. When either direction of a MongoDB session ended, the teardown closed the upstream connection and then cleared the pointer to it, while the opposite pump was still reading that same field to service its own traffic — a data race on every session close, latent since the relay was written and reachable on any session, not just a failing one. The pointer is no longer cleared: the upstream close is already idempotent, so the second close on the session's own teardown path was always harmless, and the field is now written once at connect time before either pump exists. The equivalent teardown on the other four proxies was checked and does not share the shape. ([01314ac](https://github.com/fclairamb/dbbat/commit/01314ac9aefa5ef4ea92ebc8b391f04f5decc535))
* **mssql:** a `USE` that leaves the granted database is refused, and dynamic SQL is checked instead of stepped over. TDS pins nothing — the LOGIN7 database field sets the initial context only — and batch validation ran the shared checks plus a bulk-copy pattern and nothing else, with no SQL Server blocked-pattern list at all, so `USE otherdb` was an ordinary batch that moved the session. The check now scans the whole batch rather than its leading statement (a T-SQL batch needs no separator), steps over string literals and quoted identifiers, skips the `OPTION (USE PLAN …)` and `OPTION (USE HINT …)` query hints, and refuses whatever the grant says. `EXEC('…')` made "no statement begins inside a string literal" — the invariant every prefix-shaped validator rests on — false: measured before the fix, `EXEC('USE otherdb; SELECT * FROM secret')` passed with `read_only`, `block_ddl` and `block_copy` all set, and `EXECUTE('DROP TABLE t')` passed `block_ddl`. The inner statement now runs through exactly the same checks as the outer one, one level deep, because a laxer rule set inside would turn every control into a suggestion. `sp_executesql` is found whichever way T-SQL names its statement argument — `[@stmt](https://github.com/stmt)`, `[@statement](https://github.com/statement)`, `[@tsql](https://github.com/tsql)`, `EXEC sys.sp_executesql`, in any argument order — where before only the positional form was recognised and every named spelling walked past as an inert literal. `EXEC dbo.p` still falls through as ordinary text and `EXEC('SELECT 1')` still runs under a read-only grant; `EXEC([@sql](https://github.com/sql))` is not statically decidable and is documented as the undecidable case. ([01314ac](https://github.com/fclairamb/dbbat/commit/01314ac9aefa5ef4ea92ebc8b391f04f5decc535))
* **mysql:** a text `USE otherdb` is refused instead of forwarded. `COM_INIT_DB` had always been checked, but go-mysql routes `COM_QUERY` straight to the query handler, so the SQL text `USE otherdb` — which is what `mysql -e` and most drivers' `Exec` send — never reached that check; `USE` matches no write keyword, no DDL keyword and no blocked pattern, so it was forwarded under every grant, read-only included, and every subsequent `queries` row named the granted database rather than the one the statement actually ran against. Both paths now share one decision, taken against the comment-normalised text so a version-gated or `#` line comment between the keyword and the name cannot slip through, and `USE <the granted database>` is still allowed and answered without an upstream round trip. `PREPARE s FROM 'USE otherdb'` earns the same refusal: the literal is unwrapped one level and put through the same decision, since `PREPARE` matches no control and the switch scan used to read only the outer statement. A nested `PREPARE`, an unterminated literal or adjacent literals fail closed; `PREPARE s FROM [@sql](https://github.com/sql)` and `CONCAT(...)` remain undecidable and are documented as such rather than implied to be covered. ([01314ac](https://github.com/fclairamb/dbbat/commit/01314ac9aefa5ef4ea92ebc8b391f04f5decc535))
* **oracle:** `ALTER SESSION SET CONTAINER` is refused outright, whatever the grant says. Switching pluggable database steps outside the one server row the grant covers, so every `queries` row written after the switch names the wrong database — but because the statement merely starts with `ALTER`, only `read_only` and `block_ddl` refused it and the default full-write grant allowed the escape. It now joins `ALTER SYSTEM` in the Oracle blocked patterns, and the pattern scans to the end of the statement rather than anchoring on `SET CONTAINER`, so `ALTER SESSION SET CURRENT_SCHEMA=X CONTAINER=Y` is the same refusal. `CURRENT_SCHEMA` on its own stays allowed: it moves name resolution, not the database the session is talking to. ([01314ac](https://github.com/fclairamb/dbbat/commit/01314ac9aefa5ef4ea92ebc8b391f04f5decc535))
* **oracle:** a refused statement no longer hangs sqlplus and the DB-bundled OCI client. Under a grant carrying statement controls, the refusal was written in an encoding and framing those clients do not wait for, so the call never ended and the client simply sat there — a refusal that hangs the session is worse than one that is denied. The refusal now ends the client's call with a real OER frame, in the dialect the session negotiated, in the shape learned from what the server itself sent, and stamped with the client's own call number; the learned shape is guarded against concurrent access and every learned field is carried through the fallback rather than partially. The 64-bit OCI close-cursors header and the thick client's wide-encoded close list are walked in full instead of a sequence number being read out of them, so a refusal ends the right call, a cursor id is never read out of a piggyback dbbat cannot walk, and a frame dbbat cannot read is not allowed to travel under a held refusal. ([01314ac](https://github.com/fclairamb/dbbat/commit/01314ac9aefa5ef4ea92ebc8b391f04f5decc535))
* **oracle:** a statement that failed on an OCI client is recorded as a failure instead of a success. The OER decoder read TTC compressed integers only, so on sqlplus, the Instant Client and SQL*Developer over OCI every summary object the server sent was refused — and *every* failing statement on those sessions was written to `queries` as a success, while cursor-id learning went blind on the same connections. The fixed-width layout dbbat could already write is now also read, at the very offsets that encoder writes, anchored on the error number repeated as the trailing return code plus a non-zero call status rather than on a trusted length, with the layout asked of the session's learned shape and both tried under that anchor until something has been learned. A failure raised *mid-fetch*, after rows have already started flowing, likewise left a query row carrying no error text at all; its ORA text is now recorded, on thin, JDBC and OCI clients alike. This is an audit-record correctness fix: rows already stored are not repaired. ([01314ac](https://github.com/fclairamb/dbbat/commit/01314ac9aefa5ef4ea92ebc8b391f04f5decc535))
* **oracle:** cursor-id learning no longer dies after a session's 255th call, and a re-execution dbbat cannot resolve is refused under statement controls. The field the learner reads is the end-to-end ECID sequence, a uint16 counting up across the whole session, but it was bounded at 255 on the belief that TTC numbers calls with a wrapping byte; measured against Oracle 23ai Free, a session crosses 255 after a few dozen statements and from that point every OER was rejected. Because Oracle recycles cursor ids, that did not surface as an untracked cursor — the re-executions that followed resolved to whatever stale statement last held the id, so the gate ran the wrong SQL and `/queries` recorded the wrong SQL, silently. Captured mid-churn: five runs of `SELECT 1 AS n FROM dual`, all gated as `SELECT 35 AS churn FROM dual`. All three frames that name a cursor and carry no SQL — the SQL-less `OALL8`, a fresh-query `OFETCH`, and the piggyback re-execution every modern thin client actually sends — now answer an untracked cursor identically: `ORA-01031` under a grant carrying a statement-shaped control, forwarded with a WARN under one carrying none. The wire op a client picks is no longer a cheaper way past the same grant, and the three share one table so they cannot drift apart again. Licensed by measurement rather than argument: 124 live re-executions from go-ora v3 and python-oracledb thin, across prepared loops, bind-heavy statements, interleaved cursors, DML, PL/SQL, a REF cursor and a churned statement cache, none of them naming a cursor dbbat could not resolve. ([01314ac](https://github.com/fclairamb/dbbat/commit/01314ac9aefa5ef4ea92ebc8b391f04f5decc535))
* **oracle:** the connectivity check reports a rejected password as an auth failure rather than a network fault. go-ora enables Oracle 23ai's one-round-trip fast login by default, and its fast path reads the login reply as a protocol-negotiation message without first checking whether it is a TTC error, so a perfectly readable `ORA-01017` was rendered as "message code error: received code 4 and expected code is 1" and classified as `db_handshake_failed` — sending the admin to look at the network instead of at the credentials. Fast login is now disabled on the probe's connect string, which costs one round trip on a check that only runs when someone presses "test connection"; pre-23ai servers never offered it, so nothing changes against them. ([01314ac](https://github.com/fclairamb/dbbat/commit/01314ac9aefa5ef4ea92ebc8b391f04f5decc535))
* **oracle:** the statement gate reads the execute header's declared SQL length instead of guessing at it. A window-and-keyword scan returned a mid-statement fragment for 48 of the 137 execute ops in the test corpus — `ALTER SESSION SET CURRENT_SCHEMA=TESTADM` read as `SET CURRENT_SCHEMA=TESTADM` among them — so `read_only`, `block_ddl` and every approval pattern were judged against text that was not what the upstream ran, and query history recorded the fragment. Three further defects in the decode are closed with it: a length shorter than the statement returned a silent prefix while reporting success, so the gate enforced against truncated text believing it was precise; a twelve-byte payload could slice past the end of the buffer, and the recover that contained the panic meant the frame was forwarded ungated; and capping statement text at `0x7e` truncated non-ASCII SQL at the first accented byte, losing any blocked or approval pattern in the tail for the WE8ISO8859P1 client population the Oracle notes themselves call the common European case. Statement text is now admitted under the same sanitisation already shipped for OER diagnostics, and bind capture anchors on the wire bytes rather than the repaired text, so a repaired multi-byte character cannot make the tail scan walk back into the statement and report its own text as a bind value. ([01314ac](https://github.com/fclairamb/dbbat/commit/01314ac9aefa5ef4ea92ebc8b391f04f5decc535))
* **proxy:** a panic on a proxy goroutine ends the session it belongs to, not the whole process. Every protocol starts relay and bookkeeping goroutines whose panics the recover on the connection handler cannot reach — a different goroutine catches nothing they raise — so packet framing, the dump writer, the mid-stream limit check, a held refusal's teardown, and the detached goroutines that write a query record, a completion or an API-key usage bump were each a process-wide fault on one malformed session: every live session on every database, on every protocol, dropped. All of them now run under shared guards, and so do the retention sweeps, the batched row writer's drain loop, and package `main`'s maintenance loops, where a panic in one housekeeping tick had the same blast radius. The limit watchdog is guarded differently on purpose: it owns nothing it closes and enforces by calling back into the session, so a recover that merely let it exit would leave the session running with no expiry, no byte quota and no revocation check — and on MongoDB, SQL Server and MySQL that watchdog is the whole of mid-stream enforcement. Its guard therefore performs the teardown explicitly, because trading a loud process death for a quietly unmetered session is not a trade an access-control proxy should make. The listener accept loops stay deliberately unguarded, and the reasoning is recorded where the next reader will find it. ([01314ac](https://github.com/fclairamb/dbbat/commit/01314ac9aefa5ef4ea92ebc8b391f04f5decc535))
* **proxy:** a statement refused by access control is written to query history on Oracle and PostgreSQL. MySQL/MariaDB, MongoDB and SQL Server have always recorded one, but on those two protocols a refusal by `read_only`, `block_copy` or `block_ddl` left nothing behind but an slog WARN — and, if capture happened to be on, the pcapng — so the UI and any log-based alerting saw what ran and never what was attempted. Refusals now go through the same store path as any other query row, with `duration_ms` and `rows_affected` at 0 and the refusal text as `error`. On Oracle this covers every gated path (the `OALL8`, the v315+ piggyback exec, the JDBC thin driver's dedicated exec, and a re-execution re-gated against its cursor's SQL) and also quota exhaustion, expiry and mid-session revocation, which had been checked too early in the pipeline to have a statement to record the refusal against. ([01314ac](https://github.com/fclairamb/dbbat/commit/01314ac9aefa5ef4ea92ebc8b391f04f5decc535))
* **proxy:** an inline SQL comment no longer walks a statement past the blocked-pattern lists. Every check in the shared validator is regex- or prefix-shaped and matched the raw statement, while a database ignores a comment wherever whitespace is allowed — so `ALTER/**/SESSION SET CONTAINER=PDB2` and `SET/**/ROLE postgres` went straight through blocks the un-commented spelling is refused by, and a comment before the leading keyword changed what `read_only` and `block_ddl` believed a statement was. Matching now runs against a normalised scratch copy in which every comment becomes a single space; the statement relayed upstream stays byte-identical, so optimizer hints still reach the database exactly as the client wrote them and simply stop being an evasion channel. The stripper is literal-aware — `'…'` with `''` escaping, `"…"`, Oracle's `q'[…]'` quote-operator forms, MySQL backticks and backslash escapes — and fails closed onto the raw text on an unterminated literal, because a `/*` inside a literal is not a comment and getting that wrong turns a parser bug into an authorization bug. MySQL's version-gated executable comments are consumed the way the server consumes them, marker digits and closing delimiter included, so a write wrapped in `/*!50000 … */` cannot read as a non-write. The PostgreSQL read-only bypass list and its `COPY` prefix check, and the SQL Server bulk-copy pattern, are routed through the same normalisation rather than left matching raw text one line away from a validator that no longer does. ([01314ac](https://github.com/fclairamb/dbbat/commit/01314ac9aefa5ef4ea92ebc8b391f04f5decc535))
* **store:** grant windows and short-lived TTLs are stamped from the database clock instead of the process clock. A grant is admitted with `starts_at <= NOW()`, which PostgreSQL evaluates against its *own* clock, but both issuance paths — the approval transaction and the admin `POST /grants` default — stamped that window from the process clock; dbbat and its store are two machines, so a process running even a few milliseconds ahead issued grants that all five proxies refused until the skew elapsed: approved, visible in the UI, and unusable. Device authorization requests, login code exchanges and OAuth CSRF states had the mirror-image problem, writing `expires_at` locally and reading it back SQL-side, so their real lifetime was the TTL plus or minus the skew — a process ahead of its store made a device authorization, and the API key it mints, redeemable past its intended life, while one behind could produce a login code born expired. All of them now take their clock from the store, the TTLs stamped inside the insert itself. ([01314ac](https://github.com/fclairamb/dbbat/commit/01314ac9aefa5ef4ea92ebc8b391f04f5decc535))


### Performance Improvements

* **api:** the pending-approvals and grant-request listings resolve approvers and grants once per page instead of once per row. Both walked the approver fallback chain twice per row and looked up each row's grant individually, so a page's cost grew with the number of distinct databases on it. They now prefetch the caller's user groups, the approver groups of every distinct database on the page and the grants behind it, then decide every row — and compute every approver hat — against that one answer, in a fixed number of queries. Same chain, same rule, self-approval refusal included. ([01314ac](https://github.com/fclairamb/dbbat/commit/01314ac9aefa5ef4ea92ebc8b391f04f5decc535))

### Upgrade notes

**Seven migrations**, all forward-only and reversible:

| Migration | What it does |
|---|---|
| `20260809000000_server_groups` | adds `server_groups` / `server_group_members`, a literal mirror of the user-group tables |
| `20260809010000_grant_definitions_server_group_scope` | adds `grant_definitions.server_group_uids`, and mirrors every existing per-database scope into a real server group so no definition changes behaviour |
| `20260810000000_audit_chain` | HMAC-chains `audit_log` (one chain) and `queries` (one chain per connection) |
| `20260810010000_query_row_chain` | HMAC-chains `query_rows`, one chain per capture |
| `20260810020000_connections_query_chain_stamp_version` | records which format `connections.query_chain_mac` is in |
| `20260812000000_audit_log_control_plane_index` | keeps the default audit listing cheap now every session writes to `audit_log` |
| `20260812010000_server_approver_groups` | adds the two approver lists to servers and server groups |

**The grant-definition scope API is the compatibility break in this release.** A definition now scopes on server *groups* rather than enumerating databases: `database_uids` becomes `server_group_uids`. Because two kinds of group now exist, `group_uids` and `approver_group_uids` are renamed `user_group_uids` and `approver_user_group_uids`, `group_uids` becomes `user_group_uids` on the update-user body, and the user detail response's `groups` key becomes `user_groups`. **Every retired spelling is refused with a 400 naming its replacement rather than silently ignored** — dropping a scope restriction on the floor would fail open. The migration mirrors existing scopes into real groups, so existing definitions keep working; any client or script that *writes* them must be updated.

**MongoDB legacy wire opcodes are now refused after authentication.** Anything that is not `OP_MSG` — `OP_QUERY`, `OP_GET_MORE`, `OP_INSERT`, `OP_UPDATE`, `OP_DELETE`, `OP_KILL_CURSORS` — was previously forwarded verbatim, skipping the `$db` check, `read_only`, and statement logging entirely. It is now refused regardless of grant controls. This is a deliberate client-compatibility break: a modern driver against a supported server never sends these, but hand-crafted clients and very old drivers talking to MongoDB < 5.1 will stop working. MongoDB itself removed these opcodes in 5.1.

**`read_only` and `block_ddl` do not reach inside dynamic SQL.** They are evaluated on the statement dbbat receives, and that check is prefix-shaped, so a statement whose real payload is a string literal executed at runtime is classified on its wrapper. Under a grant carrying both controls, `PREPARE s FROM 'DELETE FROM t'` (MySQL), `BEGIN EXECUTE IMMEDIATE 'DELETE FROM t'; END;` (Oracle) and `EXEC('DELETE ' + 'FROM t')` (SQL Server) are all permitted. This release narrows the gap — SQL Server's single-literal `EXEC('…')` / `EXECUTE('…')` / `sp_executesql` forms and MySQL's `PREPARE … FROM '<literal>'` database switch are now checked — but it does not close it, and variable-built forms (`EXEC(@sql)`, `PREPARE … FROM @var`) are not statically decidable at all. The blocked-pattern lists are substring matches and are unaffected. `docs/mssql.md` and `docs/mysql.md` state the boundary per protocol.

**A store written by a pre-0.24 development build cannot be verified.** The query-chain head stamp is a keyed, versioned seal; an earlier unreleased revision wrote it as a verbatim copy of the last statement's MAC, which is readable straight out of `queries` and therefore forgeable without the key. Such a row is reported as a break, with no opt-out. The chain, the keyed stamp and the version column all ship together here, so only a store written from a development build of this branch is affected — a store upgraded from 0.23.x is not.

**Approval holds remain evadable by comment.** The statement validators now match against a comment-normalised copy, so `ALTER/**/SESSION SET CONTAINER=PDB2` no longer walks past the blocked-pattern lists. Approval-hold patterns are matched separately, against the raw statement, so that `DELETE/**/FROM t` still dodges a `(?i)^DELETE` hold. That path is unchanged in this release because the pattern-preview UI mirrors the same normalisation and changing one without the other would misrepresent what a pattern matches.


## [0.23.2](https://github.com/fclairamb/dbbat/compare/v0.23.1...v0.23.2) (2026-08-08)


### Bug Fixes

* **oracle:** re-executing a cached cursor no longer skips the approval gate and the static controls. An Oracle client can re-run a statement it already parsed by naming the cursor id alone, with no SQL text on the wire — a SQL-less `OALL8`, or an `OFETCH` arriving when no query is in flight. Both were forwarded ungated, so an approval decision applied to the *parse* rather than to each execution: a client that parsed once and re-executed many times got one hold and then a free run. Both are now validated and held against the SQL the cursor was parsed with, on every execution. A fetch that merely continues a query already in flight is untouched — nothing holds mid-result-set.
* **oracle:** a SQL-less `OALL8` naming a cursor dbbat never saw parsed now fails closed — but only under a grant carrying statement-shaped controls (approval patterns, `read_only` or `block_ddl`), where it is refused with `ORA-01031`. Under a grant with none of those it is forwarded and logged. An untracked cursor is not by itself an attack (dbbat may have attached mid-session), so refusing unconditionally would break permissive sessions for no security gain. `docs/approvals.md` documents the asymmetry, the two enforcement gaps that remain, and the fact that the real-world re-execution rate is unmeasured — this is hardening against a shape the TTC decoder accepts, not a response to an observed exploit.
* **api:** `429` is documented as the ambient outcome it actually is. It was declared on 33 of 77 operations, arbitrarily — `createDatabase`, `listUsers` and `getQuery` had it while `approveQuery` and `POST /auth/logout` did not — which invited readers to infer that the operations *without* it could not rate-limit. Since every endpoint sits behind a rate limiter, the convention is now stated once in the spec description and declared per-operation only on the six session-validation endpoints where a client must *branch* on a 429 rather than simply retry (`login`, `logout`, `getCurrentUser`, `changePasswordPreLogin`, `oauthExchange`, `deviceAuthorization`). A parity test pins that set, so a seventh has to be argued for in review. No runtime behaviour changes — every endpoint still answers 429 with the same `Error` body. ([#308](https://github.com/fclairamb/dbbat/issues/308)) ([62d9451](https://github.com/fclairamb/dbbat/commit/62d94515b5bd1b7e8c76770dc58d1c85a00ad6c9))

## [0.23.1](https://github.com/fclairamb/dbbat/compare/v0.23.0...v0.23.1) (2026-08-08)


### Features

* **ui:** the connection detail page is one live query table instead of three disconnected surfaces. The stream, pending approval holds and the REST history are merged into a single table that is live from the moment an active connection is opened — no "Watch live" click, no `?watch=1` required. The toggle survives as a pause. Held rows are amber, sit at the top, and carry Approve / Deny inline alongside the held-for counter and the matched pattern, so a hold that predates the page load is visible and actionable immediately. A streamed query and its historical row are now the same row.

### Bug Fixes

* **oracle:** the JDBC thin driver's dedicated exec op forwarded statements without ever consulting the approval gate or the `read_only` / `block_ddl` controls. It recorded a query row and passed the statement upstream ungated, so a hold pattern that matched simply never fired for that client. Every statement-carrying TTC op now runs the same normalize → validate → hold → record sequence, and fails closed. `docs/approvals.md`'s no-bypass claim is corrected to describe what the gate actually guarantees.
* **ui:** a rate-limited session check no longer destroys a valid session. `GET /auth/me` answering 429 was treated as "this token is invalid", so hitting the rate limit — several tabs booting at once, or noise from the same source address — deleted the stored token and dropped the user on the login screen. Only a 401 is now definitive; a 429, a 5xx or a network error keeps the session, shows a retry notice, and honours `Retry-After` with a single automatic re-check.
* **ui:** a 429 on login says "too many login attempts" instead of a generic "Login failed", for both of the rate limiters that guard the endpoint.
* **ui:** the "Active only" toggle on the connections list updates in place. It assigned `window.location.search`, forcing a full document reload that re-bootstrapped the SPA and rebuilt the auth and query caches — for a filter that is applied client-side anyway.
* **api:** every rate limiter answers 429 with the same body. Three middlewares hand-rolled an ad-hoc `{error, message, retry_after}` envelope while the rest of the API used the canonical `Error` schema, so anything matching on `code == "RATE_LIMITED"` silently never matched behind them. `GET /auth/me` also now declares the 429 it has always been able to return.
* **test:** `TestCheck_OracleTarget_ThroughTunnel` no longer flakes. The fake TNS listener's refusal write raced its own teardown, so go-ora saw EOF before the ORA-01017 packet and the probe degraded to the same code a genuine connectivity fault produces — indistinguishable from a real regression, and it blocked an unrelated dependency bump twice ([#305](https://github.com/fclairamb/dbbat/issues/305)). The refusal is now ordered via half-close, and both Oracle probe tests additionally assert on what the classifier actually saw. ([#306](https://github.com/fclairamb/dbbat/issues/306)) ([88f3d6c](https://github.com/fclairamb/dbbat/commit/88f3d6c6aa09d94c6f68e5a63624010d4591595b))

## [0.23.0](https://github.com/fclairamb/dbbat/compare/v0.22.0...v0.23.0) (2026-08-07)


### ⚠ BREAKING CHANGES

* **grants:** a grant no longer carries a shape of its own. Every grant is now an *instance* of a versioned grant definition, and `POST /api/v1/grants` assigns a definition to a user + database instead of accepting inline controls. Clients that built grants from `read_only` / `block_ddl` / `block_copy` / quota fields must now reference a definition. In the UI, "Create Grant" is replaced by "Assign Grant". ([#301](https://github.com/fclairamb/dbbat/issues/301))
* **api:** `GET /api/v1/connections/{uid}/dump` now requires the admin role; viewer tokens receive 403. ([#300](https://github.com/fclairamb/dbbat/issues/300))

### Features

* **mssql:** Microsoft SQL Server is now a fully proxied protocol, the fifth after PostgreSQL, Oracle, MySQL/MariaDB and MongoDB. Hand-rolled TDS: packet framing, PRELOGIN negotiation, the TLS handshake encapsulated in TDS packets, LOGIN7 parsing and password descramble, client auth against dbbat, and relay to a real upstream. Statements are intercepted and logged, grants enforced (including inside RPC batches), result rows accounted against quotas, and approval holds honoured — including releasing a hold when the client sends an ATTENTION. Listens on `:1434` by default (`DBB_LISTEN_MSSQL`). ([#301](https://github.com/fclairamb/dbbat/issues/301)) ([85209da](https://github.com/fclairamb/dbbat/commit/85209da031e83b5cb0b4b5ef095236e12d279294))
* **mssql:** the client leg can be raised to TLS 1.3 with `DBB_MSSQL_TLS_MAX_VERSION=1.3`. Off by default and verified against `go-mssqldb` only, because TDS un-wraps the handshake the moment it completes and drivers disagree on whether the client's last flight is still framed. ([#301](https://github.com/fclairamb/dbbat/issues/301))
* **grants:** grants are instances of an immutably versioned grant definition. Editing a definition archives the current row and inserts a successor sharing its lineage, so a live grant's behaviour never changes under it. Deactivating a definition withdraws the whole lineage and fails closed at auth time. ([#301](https://github.com/fclairamb/dbbat/issues/301))
* **grants:** an explicit `priority` column decides which grant wins when several are active for the same user and database. It is auto-derived from the selected controls (a stricter grant outranks a looser one) and can be pinned by hand from the API or the UI. ([#301](https://github.com/fclairamb/dbbat/issues/301))
* **connections:** every connection is now stamped with the grant it authenticated under, surfaced through the API and shown in the UI, so quota attribution and approval resolution are traceable back to a specific grant. ([#301](https://github.com/fclairamb/dbbat/issues/301))
* **dump:** session captures can live in blob storage. They still spool to local disk and upload on session close via `gocloud.dev/blob` (`s3://`, `gs://`, `azblob://`, `file://`), never streamed mid-session, with the object key recorded on the connection row so reads never need a bucket LIST. New `DBB_DUMP_UPLOAD_URL`; empty keeps the local-only behaviour exactly. A startup sweep uploads captures left finished-but-not-uploaded by a crash. ([#300](https://github.com/fclairamb/dbbat/issues/300)) ([f83e511](https://github.com/fclairamb/dbbat/commit/f83e5117efbdd488443ee6aceac53a286a16f57e))
* **ui:** the raw session capture can be downloaded from the connection detail page. `GET /connections/{uid}` carries `dump: { available, size_bytes }`, resolved through a single locator that checks the local spool then blob storage. ([#300](https://github.com/fclairamb/dbbat/issues/300))
* **ui:** upstream TLS is visible on the connections screen — quiet when encrypted, an amber badge when the leg fell back to plaintext, paired with the server's `ssl_mode` policy for admins. Oracle sessions, always plaintext because the proxy never upgrades that leg, are labelled not-applicable rather than flagged. ([#300](https://github.com/fclairamb/dbbat/issues/300))
* **grant-definitions:** definitions carry sample queries, so a control pattern can be authored against realistic SQL. ([#301](https://github.com/fclairamb/dbbat/issues/301))
* **auth:** the OAuth callback hands back a single-use code that the frontend exchanges for a session token, instead of putting the session token itself in the redirect. ([#301](https://github.com/fclairamb/dbbat/issues/301))
* **ui:** the API-key creation dialog shows a ready-to-paste export snippet for the canonical client environment variables (`DBBAT_API_KEY`, `DBBAT_USER`, `DBBAT_URL`), now used consistently across the docs and website examples. ([#301](https://github.com/fclairamb/dbbat/issues/301))
* **docs:** the website's marketing media is regenerated on demand from a live demo-mode instance (`make showcase`, plus a `workflow_dispatch` Action), stamped with a manifest recording the version and commit captured. The approval clip is a real held `UPDATE` released by a click in the UI, not a mockup. ([#300](https://github.com/fclairamb/dbbat/issues/300))

### Bug Fixes

* **api:** the approvals stream leaked other grants' SQL. `approvals/pending` was authorized per *topic*, so anyone who could approve on a single grant received the SQL text, database and requesting user of **every** held query on the instance. Authorization is now per *event*, delegating to the same `mayViewQuery` the REST endpoint uses so the two cannot drift, with every uncertain branch failing closed. Reported by a security audit. ([#300](https://github.com/fclairamb/dbbat/issues/300))
* **store:** `text[]` elements beginning with `(` were split on read, silently corrupting approval patterns — the most common way to write one. Existing corrupted patterns are detectable and repairable; see `docs/approvals.md`. ([#301](https://github.com/fclairamb/dbbat/issues/301))
* **api:** access-log query redaction was a denylist, so any query shape nobody had thought of was logged verbatim. It is now an allowlist that defaults to redacting. ([#301](https://github.com/fclairamb/dbbat/issues/301))
* **api:** `GET /users/:uid` returned more than the caller was entitled to see; it is now scoped to the caller's visibility. ([#301](https://github.com/fclairamb/dbbat/issues/301))
* **api:** the login redirect target is kept through the Slack OAuth flow, so a device registration and an account registration in the same process no longer collide. Sanitized on both ends to same-app absolute paths, so it cannot become an open redirect. ([#298](https://github.com/fclairamb/dbbat/issues/298)) ([6dcc27c](https://github.com/fclairamb/dbbat/commit/6dcc27c36d8fc802dd93a3a500db584134bfdc7c))
* **api:** every failure branch of the OAuth flow now forwards that same sanitized redirect target, so a retry from the login page still lands where the user started. ([#300](https://github.com/fclairamb/dbbat/issues/300))
* **proxy:** the accept loop could deregister itself while `Shutdown` was waiting on it, racing `wg.Add` against `wg.Wait`. Covered by a race-stress job in CI. ([#301](https://github.com/fclairamb/dbbat/issues/301))
* **proxy:** a statement released from an approval hold now carries the hold's outcome on its completion event, so the live watch feed no longer forgets an approval once the statement runs. ([#301](https://github.com/fclairamb/dbbat/issues/301))
* **store:** `ListGrants(ActiveOnly)` and `GetActiveGrant` disagreed about what counts as an active definition state. ([#301](https://github.com/fclairamb/dbbat/issues/301))
* **migrations:** the grants-to-definitions backfill could attach a grant to a definition that does not cover its database; the scope condition is fixed and a follow-up migration repairs rows already backfilled that way. ([#301](https://github.com/fclairamb/dbbat/issues/301))
* **ui:** the TanStack Query cache survived login, logout and stale-token invalidation, so one identity could briefly see the previous session's rows. ([#301](https://github.com/fclairamb/dbbat/issues/301))
* **ui:** a grant whose definition is missing now renders an explicit unknown state instead of blank. ([#301](https://github.com/fclairamb/dbbat/issues/301))
* **deps:** update module github.com/microsoft/go-mssqldb to v1.10.0 ([#303](https://github.com/fclairamb/dbbat/issues/303)) ([f3ecbce](https://github.com/fclairamb/dbbat/commit/f3ecbcece963eca2ced261a42da752892b51b83e))
* **deps:** update testcontainers-go monorepo to v0.44.0 ([#302](https://github.com/fclairamb/dbbat/issues/302)) ([a7145ef](https://github.com/fclairamb/dbbat/commit/a7145efb59a5e62d8400d665d664f680abb35e84))

### Performance Improvements

* **website:** the homepage showcase ships as WebP with the clip held until it is scrolled into view, and the logos ship as renditions rather than 761px originals — removing ~1.4 MB of PNG from above the fold. ([#301](https://github.com/fclairamb/dbbat/issues/301))

### Upgrade notes

**Six migrations**, all forward-only and reversible:

| Migration | What it does |
|---|---|
| `20260805120000_connections_dump_key` | adds `connections.dump_key` for blob-stored captures |
| `20260806000000_grants_priority` | adds `priority` to grants and grant definitions |
| `20260806010000_connections_grant_uid` | adds `connections.grant_uid` |
| `20260806020000_grants_reference_definitions` | links every grant to a grant definition, backfilling one per distinct existing grant shape |
| `20260807000000_grant_definitions_sample_queries` | adds sample queries to definitions |
| `20260807010000_grants_rescope_mismatched_definitions` | repairs grants the first backfill attached to an out-of-scope definition |

**The grants API is the main compatibility break in this release.** `POST /api/v1/grants` no longer accepts a grant's shape inline — it assigns an existing definition to a user and a database. Every control (`read_only`, `block_copy`, `block_ddl`), the duration, the quotas and the approval patterns now live on the definition and are read through it. The backfill means existing grants keep working unchanged: each distinct shape in your database becomes a generated definition. Any client or script that *creates* grants must be updated to reference a definition.

**Capture download is admin-only.** `GET /api/v1/connections/{uid}/dump` now requires the admin role and returns 403 to viewer tokens. A session capture is raw wire traffic, so it is not viewer-grade data.

**The backfill's scope was verified against real data and did need the follow-up repair.** `20260806020000` could pair a grant with a definition that does not cover the grant's database; `20260807010000` re-scopes those rows. If you already ran the former in an earlier build, the latter is what fixes it — no manual intervention is required.

**Approval patterns starting with `(` may already be corrupt.** The `text[]` read path split them on the leading paren, so a pattern like `(?i)DELETE` was stored intact but read back broken. Upgrading fixes the codec; it does not repair rows written before. `docs/approvals.md` explains how to detect and repair them.

**If you approve queries, update promptly.** Before this release the pending-approvals stream was authorized per topic, so any user who could approve on one grant received the SQL text, database and requesting user of every held query on the instance. There is no configuration workaround — the fix is the upgrade.

**SQL Server is new and its listener is on by default** at `:1434` (`DBB_LISTEN_MSSQL`, empty disables). 1434/tcp is free — the SQL Server Browser that owns 1434 is UDP-only. TLS is terminated with an auto-generated self-signed certificate unless you point `DBB_MSSQL_TLS_CERT_FILE` / `DBB_MSSQL_TLS_KEY_FILE` at your own.

## [0.22.0](https://github.com/fclairamb/dbbat/compare/v0.21.0...v0.22.0) (2026-08-05)


### Features

* **api:** address grant definitions by slug, and de-flake the protocol integration suites ([#291](https://github.com/fclairamb/dbbat/issues/291)) ([1632627](https://github.com/fclairamb/dbbat/commit/16326275fde1f75c1ac3f561f21e6e7c5f6097e7))


### Bug Fixes

* **oracle:** stop the legacy TTC Response decoder inventing errors from row data ([#292](https://github.com/fclairamb/dbbat/issues/292)) ([2905143](https://github.com/fclairamb/dbbat/commit/290514319ad6168080af017581dd79cc5e8ef472))
* **proxy:** never let a connectivity probe panic take down the proxy ([#296](https://github.com/fclairamb/dbbat/issues/296)) ([aee9032](https://github.com/fclairamb/dbbat/commit/aee90326eb440d2c79bca2fd70f8987c5860e18b))


### Upgrade notes

**One migration**, `20260805000000_grant_definition_slug`. It adds `grant_definitions.slug` nullable, backfills every existing row with `legacy-<first 8 hex of uid>-<n>`, then applies `NOT NULL` + `UNIQUE`. Safe on a populated table and reversible. The backfilled values are placeholders — renaming them is the point of the feature, since a CLI or agent can then say `read-only-prod` instead of a UUID.

**Creating a grant definition now requires a `slug`.** Any API client that creates definitions must send one; the server deliberately does not auto-generate it. It must be lowercase alphanumeric segments (max 64) and must **not** be a UUID — that exclusion is what keeps slugs and uids in disjoint namespaces, so resolving `/grant-definitions/{uid}` stays unambiguous. This is the one compatibility break in this release.

**The Oracle fix is not retroactive.** Query rows already stored may carry fabricated `ORA-<number>` errors with row data in the error field, and their `rows_affected` and captured rows may be truncated at the misparse point with `results_truncated` left `false`. Upgrading stops new ones; it does not repair old ones. A clean-up is written up in `specs/todos/2026-08-05-clean-up-fabricated-query-errors.md` and was deliberately not executed.

**Connectivity checks could previously crash the proxy.** A malformed TNS Accept packet from an Oracle host panicked go-ora on an unrecovered goroutine, taking the whole process down and dropping every live session on every protocol — reachable from `POST /servers/{uid}/test` and from `test_connection: true` on server create/update. Such a probe now reports `target_auth` / `internal_error`, with the stack trace in the server log.

## [0.21.0](https://github.com/fclairamb/dbbat/compare/v0.20.0...v0.21.0) (2026-08-05)


### ⚠ BREAKING CHANGES

* **proxy:** the instances registry primary key becomes (instance_id, run_id). A v0.20.x replica's heartbeat upserts ON CONFLICT (instance_id) and starts failing the moment this migration runs; after the 15-minute grace period a new-build replica reclaims the connections it is still serving, and a reclaimed connection immediately becomes eligible for the retention sweep. Complete the upgrade from v0.20.x within 15 minutes, and do not roll back to v0.20.x once migrated. Only affects deployments with replicaCount > 1.

### Features

* **proxy:** one upstream-connect path for the proxy and the connectivity probe ([#288](https://github.com/fclairamb/dbbat/issues/288)) ([766688e](https://github.com/fclairamb/dbbat/commit/766688e4d519d5baf1c72f2b561413af1b8991a5))


### Bug Fixes

* **deps:** update module github.com/knadh/koanf/parsers/toml/v2 to v2.2.2 ([#284](https://github.com/fclairamb/dbbat/issues/284)) ([12e1a7d](https://github.com/fclairamb/dbbat/commit/12e1a7d8fa14c65ee38b4de43b9f506dcf171a0b))
* **deps:** update module github.com/knadh/koanf/parsers/yaml to v1.1.1 ([#286](https://github.com/fclairamb/dbbat/issues/286)) ([bfce514](https://github.com/fclairamb/dbbat/commit/bfce514d933c3ebcbe4baae6f404cb10a6de4a63))
* **deps:** update module github.com/knadh/koanf/providers/env/v2 to v2.0.1 ([#287](https://github.com/fclairamb/dbbat/issues/287)) ([69cd341](https://github.com/fclairamb/dbbat/commit/69cd34140d5f60c4a4c4194018f6431509d62cc9))
* **deps:** update module github.com/knadh/koanf/providers/structs to v1.0.1 ([#289](https://github.com/fclairamb/dbbat/issues/289)) ([15d703b](https://github.com/fclairamb/dbbat/commit/15d703b3ef02bd5632a0a4d73e4f0ebcc015befb))
* **deps:** update module github.com/knadh/koanf/v2 to v2.3.6 ([#290](https://github.com/fclairamb/dbbat/issues/290)) ([7616b93](https://github.com/fclairamb/dbbat/commit/7616b930ac54ea53a9c019c239043607ad27d939))

## [0.20.0](https://github.com/fclairamb/dbbat/compare/v0.19.1...v0.20.0) (2026-08-04)


Lands the batch of specs accumulated in `specs/todos/` in one squash-merged PR ([#280](https://github.com/fclairamb/dbbat/issues/280)) ([8ddd21b](https://github.com/fclairamb/dbbat/commit/8ddd21b7abee4abbbe30b09d007ef4a00ded4351)); the individual changes are broken out below.

### ⚠ BREAKING CHANGES

* **postgresql:** the default PostgreSQL proxy listen port is now `:5433` instead of `:5434`. Deployments relying on the implicit default must set `DBB_LISTEN_PG=:5434` to keep the previous behaviour. `5433` is also the conventional pgbouncer port — on a host already running one, bind the proxy elsewhere.
* **dump:** session captures are written as pcapng, and the bespoke `.dbbat-dump` v1/v2 read path is removed — existing `.dbbat-dump` files can no longer be read by dbbat. The retention sweep does reap them, so operational dump directories drain on their own. New captures open directly in Wireshark/tcpdump/tshark, with native dissectors for the wrapped protocols.

### Features

* **api,proxy,ui:** live query stream over WebSocket. `GET /api/v1/stream` carries topic subscriptions (`connection/<uid>/queries`, `approvals/pending`, `connections`), with per-topic authorization evaluated at subscribe time *and* re-checked before every send, so the stream is never a wider read path than `GET /api/v1/queries`. Backed by an in-process broker with a bounded per-subscriber buffer, `lagged` notices on overflow, and PostgreSQL `LISTEN`/`NOTIFY` fan-out across replicas.
* **grants,proxy:** pattern-triggered approval holds. Grant definitions carry RE2 patterns that suspend a matching statement mid-flight until an admin or approver-group member decides. There is no timeout — a hold ends on approve, deny, or client disconnect (recorded as `abandoned`, distinctly from `denied`). Self-approval is always rejected, the approver is persisted and broadcast, and quotas, expiry and revocation keep running while a query is parked. Ships behind `DBB_APPROVAL_ENABLED`, off by default.
* **api:** approval endpoints — `POST /queries/{uid}/approve`, `POST /queries/{uid}/deny`, `GET /queries/pending`, and a `POST /queries/pending/deny-all` safety valve. Every decision writes an audit-log entry and publishes a resolution event.
* **auth:** Slack escalation for pending holds, on a 30 s timer (`DBB_APPROVAL_SLACK_DELAY`), with the message updated in place the moment the hold resolves by any route — no stale Approve button. SQL text is truncated and can be switched off (`DBB_APPROVAL_SLACK_SQL`).
* **dump:** tcpdump-compatible pcapng session captures via pure-Go `gopacket/pcapgo` — session metadata in the Section Header Block comment, per-packet direction in `epb_flags`, nanosecond timestamps, and synthesized Ethernet/IP/TCP framing so Wireshark's dissectors fire.
* **proxy,store:** batched result-row persistence. One process-wide writer replaces both extremes — Oracle was issuing a synchronous `INSERT` per captured row (up to 100k round-trips on a single query), while PostgreSQL and MySQL held the whole capture in RAM until the query ended. A bounded channel drained opportunistically batches to load (1000 rows or 8 MB, whichever trips first), and sends are non-blocking, so dbbat's own storage can never stall a proxied query. Rows lost because the writer fell behind are recorded as `results_dropped`, distinct from `results_truncated`.
* **docs:** auto-generated `llms.txt` and `llms-full.txt` on dbbat.com, built from the docs tree with a rot guard that fails the build if a generated URL no longer resolves. Every instance also serves an unauthenticated `GET /llms.txt` describing itself — a local response, never a redirect, carrying no instance topology.
* **store:** crash-orphaned connections are reconciled at startup. `disconnected_at` was only ever written on clean teardown, so a crash or pod reschedule left rows open forever — invisible to the retention sweep and still counted as "currently connected". Reconciliation is scoped by an `instances` liveness registry (`DBB_INSTANCE_ID`, heartbeat plus deregister-on-shutdown) so one replica can never close another live replica's connections.

### Bug Fixes

* **postgresql:** a COPY capture that hit the byte limit discarded everything it had already buffered and stored zero rows. It now keeps the prefix, dropping only the trailing partial line.
* **api:** `GET`/`DELETE /connections/{uid}/dump` had shipped entirely absent from the OpenAPI spec, so the capture-download API was invisible in Swagger UI and missing from generated clients. Documented, and a parity test now asserts in both directions that every `/api/v1` route is described and every described path is registered.
* **dump:** the retention sweep reaps leftover `.dbbat-dump` files from before the pcapng switch, which would otherwise never expire.
* **ui:** the connection detail page no longer stamps `?watch=false` onto its URL when the live panel is off.
* **ci:** the website is rebuilt when the root `CHANGELOG.md` changes — the published changelog had been stuck at v0.17.0 because release commits touch no `website/` path.
* **deps:** update module github.com/knadh/koanf/parsers/json to v1.0.1 ([#282](https://github.com/fclairamb/dbbat/issues/282)) ([6d74e51](https://github.com/fclairamb/dbbat/commit/6d74e51ee8f29565f0bde7557b824e2866c7c47a))

## [0.19.1](https://github.com/fclairamb/dbbat/compare/v0.19.0...v0.19.1) (2026-08-02)


### Bug Fixes

* **deps:** complete the `go-ora` v2→v3 migration — the earlier version bumps ([#268](https://github.com/fclairamb/dbbat/issues/268), [#276](https://github.com/fclairamb/dbbat/issues/276)) only added the `v3` module requirement without touching any `.../v2` imports, which left the build broken; this rewrites the Oracle conncheck probe and test suite to import `v3` and drops the now-unused `v2` requirement ([#272](https://github.com/fclairamb/dbbat/issues/272)) ([43b1aba](https://github.com/fclairamb/dbbat/commit/43b1abab7e071938d08433d614550809b3188a25))
* **ui:** show full query columns on the home page ([#274](https://github.com/fclairamb/dbbat/issues/274)) ([ab7f879](https://github.com/fclairamb/dbbat/commit/ab7f87961acdb99e17f862696a8bb04ec981641b))

## [0.19.0](https://github.com/fclairamb/dbbat/compare/v0.18.0...v0.19.0) (2026-07-22)


### Features

* **auth:** add a device authorization flow for API key provisioning, following the OAuth 2.0 Device Authorization Grant (RFC 8628). A CLI or desktop app opens a request (`POST /auth/device`), the user approves it in the browser on a consent page (with a manual code-entry fallback), and the app polls the token endpoint (`POST /auth/device/token`) for a `dbb_` key — no manual copy/paste from the web UI. Approval mints a key owned by the approving user; the key is delivered exactly once and never transits a browser URL, and the flow works over SSH/headless ([#269](https://github.com/fclairamb/dbbat/issues/269)) ([5fa16ee](https://github.com/fclairamb/dbbat/commit/5fa16ee2a919f5e397f8b60bfeea8d148b9eaf99)).
* **auth:** the login redirect now preserves the originally requested URL, so opening a deep link (such as the device consent page) while logged out returns you to that page after signing in instead of dropping you on the dashboard ([#269](https://github.com/fclairamb/dbbat/issues/269)).

## [0.18.0](https://github.com/fclairamb/dbbat/compare/v0.17.0...v0.18.0) (2026-07-21)

Lands the batch of work accumulated on local `main` in one squash-merged PR ([#266](https://github.com/fclairamb/dbbat/issues/266)) ([421ceae](https://github.com/fclairamb/dbbat/commit/421ceae8d72ab0e45c625b74383a5e1ed1303429)); the individual changes are broken out below.

### Features

* **user-groups:** add user groups with full CRUD — new `user_groups` tables plus grant-definition scope columns (migrations + store).
* **grants:** grant definitions can now be scoped to specific user groups and databases, surfaced through the API and a new UI admin page with scope pickers.
* **proxy,api,ui:** add server connectivity testing — a `POST /servers/{uid}/test` endpoint with an opt-in inline check, a staged connectivity check that probes SSH bastions and DB targets (including an Oracle login probe), and a per-server "Test connection" action in the UI with staged feedback.

### Bug Fixes

* **postgresql:** resolve bind-parameter OIDs from `ParameterDescription` so prepared statements report correct parameter types.
* **proxy:** guard the PostgreSQL, MySQL, Oracle and MongoDB proxy listeners with a mutex to fix an `Addr`/`Start` data race.
* **api:** make `test_connection` optional in the database request schemas.
* **ui:** render the breadcrumb separator as a sibling of the item so it aligns correctly.
* **docs:** silence the `baseUrl` deprecation and fix React 19 JSX types in the website typecheck.

## [0.17.0](https://github.com/fclairamb/dbbat/compare/v0.16.0...v0.17.0) (2026-07-18)

### ⚠ BREAKING CHANGES

* **api:** `/api/v1/databases` is renamed to `/api/v1/servers` (and `/api/v1/ssh-servers` is added for bastion management); no `/databases` alias is kept ([#262](https://github.com/fclairamb/dbbat/issues/262)) ([7d5008b](https://github.com/fclairamb/dbbat/commit/7d5008b7b5456ae9d2654f76f9626ab70a7f9a44))

### Features

* **grants:** grant definitions can be flagged `auto_approve` — matching requests are instantly approved with no admin decision needed, with a required justification, a Slack notification without action buttons, and a dedicated audit trail ([#262](https://github.com/fclairamb/dbbat/issues/262)) ([7d5008b](https://github.com/fclairamb/dbbat/commit/7d5008b7b5456ae9d2654f76f9626ab70a7f9a44))
* **ui:** inline auto-approve toggle on the grant-definitions table, plus an "approve & enable auto-approve" action on pending grant requests ([#262](https://github.com/fclairamb/dbbat/issues/262)) ([7d5008b](https://github.com/fclairamb/dbbat/commit/7d5008b7b5456ae9d2654f76f9626ab70a7f9a44))
* **proxy,store,api,ui:** SSH tunnel support for upstream connections across all four proxied protocols (PostgreSQL, Oracle, MySQL, MongoDB) — the `databases` table/model is renamed to `servers`, gains a self-referencing `via_uid` for SSH bastions, and a shared pooled dialer with host-key TOFU routes upstream connections through the tunnel when configured ([#262](https://github.com/fclairamb/dbbat/issues/262)) ([7d5008b](https://github.com/fclairamb/dbbat/commit/7d5008b7b5456ae9d2654f76f9626ab70a7f9a44))
* **ui:** the "Databases" page becomes `/servers`, listing SSH bastions alongside database servers, with create/edit UI for SSH servers; `/databases` redirects to `/servers` ([#262](https://github.com/fclairamb/dbbat/issues/262)) ([7d5008b](https://github.com/fclairamb/dbbat/commit/7d5008b7b5456ae9d2654f76f9626ab70a7f9a44))
* **api:** creating a server, grant definition, or user with a name that already exists now returns `409 DUPLICATE_NAME` instead of a generic error ([#262](https://github.com/fclairamb/dbbat/issues/262)) ([7d5008b](https://github.com/fclairamb/dbbat/commit/7d5008b7b5456ae9d2654f76f9626ab70a7f9a44))
* **ui:** the query detail breadcrumb now shows the connection it belongs to ([#262](https://github.com/fclairamb/dbbat/issues/262)) ([7d5008b](https://github.com/fclairamb/dbbat/commit/7d5008b7b5456ae9d2654f76f9626ab70a7f9a44))
* **api,ui:** connections now have a detail page, with connector access properly scoped ([#262](https://github.com/fclairamb/dbbat/issues/262)) ([7d5008b](https://github.com/fclairamb/dbbat/commit/7d5008b7b5456ae9d2654f76f9626ab70a7f9a44))

### Bug Fixes

* **api:** block admin password change in demo mode ([#257](https://github.com/fclairamb/dbbat/issues/257)) ([55d6ee1](https://github.com/fclairamb/dbbat/commit/55d6ee1f0ef84bc168bd087800582605f3a94b6e))
* **api:** silence exhaustive lint on grant-status switch ([#260](https://github.com/fclairamb/dbbat/issues/260)) ([83a5e14](https://github.com/fclairamb/dbbat/commit/83a5e1466fb025d028b4165dce7200b7b10f88c8))
* **deps:** update module github.com/go-mysql-org/go-mysql to v1.16.0 ([#259](https://github.com/fclairamb/dbbat/issues/259)) ([c986832](https://github.com/fclairamb/dbbat/commit/c986832538ecc4086d6a192f91bf54ec214f0a33))
* **ui:** the SSH server create dialog now includes `ssl_mode`/`listable` defaults in its payload ([#262](https://github.com/fclairamb/dbbat/issues/262)) ([7d5008b](https://github.com/fclairamb/dbbat/commit/7d5008b7b5456ae9d2654f76f9626ab70a7f9a44))
* **store:** the test-mode wipe now also drops the legacy `databases` table ([#262](https://github.com/fclairamb/dbbat/issues/262)) ([7d5008b](https://github.com/fclairamb/dbbat/commit/7d5008b7b5456ae9d2654f76f9626ab70a7f9a44))
* **ci:** pin the `bun-version` used in CI to avoid a `setup-bun` latest-tag lookup failure ([#262](https://github.com/fclairamb/dbbat/issues/262)) ([7d5008b](https://github.com/fclairamb/dbbat/commit/7d5008b7b5456ae9d2654f76f9626ab70a7f9a44))
* **dev:** `make dev` no longer depends on `scripts/run-backend-dev.sh` ([#262](https://github.com/fclairamb/dbbat/issues/262)) ([7d5008b](https://github.com/fclairamb/dbbat/commit/7d5008b7b5456ae9d2654f76f9626ab70a7f9a44))

## [0.16.0](https://github.com/fclairamb/dbbat/compare/v0.15.5...v0.16.0) (2026-07-14)

Lands the batch of work accumulated on local `main` in one squash-merged PR ([#255](https://github.com/fclairamb/dbbat/issues/255)) ([e6cbeeb](https://github.com/fclairamb/dbbat/commit/e6cbeebef133018297c8102c6b6a73303db298fe)); the individual changes are broken out below.

### Features

* **oracle:** upgrade legacy per-key-salt O5LOGON verifiers to the newer per-user-salt format automatically on successful login ([#255](https://github.com/fclairamb/dbbat/issues/255))
* **api,ui:** scope the API-key list to the caller's own keys by default, with an admin-only toggle (`all_users`) to review other users' keys ([#255](https://github.com/fclairamb/dbbat/issues/255))
* **store,api,ui:** add a configurable `web_ui_url` (falling back to `cfg.PublicURL`) and split the settings page's single port field into distinct HTTP/TCP listener fields, replacing hard-coded values ([#255](https://github.com/fclairamb/dbbat/issues/255))
* **proxy:** shared `BuildUpstreamName` helper encodes the dbbat user name into the upstream connection metadata (`application_name` / `program_name` / `AUTH_PROGRAM_NM`) for PostgreSQL, Oracle, and MySQL ([#255](https://github.com/fclairamb/dbbat/issues/255))
* **proxy:** shared `LimitGuard` enforces grant time/bandwidth limits mid-stream (not only between commands) across PostgreSQL, Oracle, and MySQL ([#255](https://github.com/fclairamb/dbbat/issues/255))
* **grants:** shared revocation registry — revoking a grant now blocks queries and disconnects live sessions across all three protocols ([#255](https://github.com/fclairamb/dbbat/issues/255))
* **api,ui:** global queries list now shows user, database, and connection columns ([#255](https://github.com/fclairamb/dbbat/issues/255))
* **ui:** replace the stale "PostgreSQL Proxy" subtitle with generic wording, since dbbat now proxies PostgreSQL, Oracle, MySQL/MariaDB, and MongoDB ([#255](https://github.com/fclairamb/dbbat/issues/255))
* **store,postgresql,oracle,mysql:** persist the bytes transferred by a query aborted mid-stream by a grant limit, so byte quotas stay accurate ([#255](https://github.com/fclairamb/dbbat/issues/255))
* **mongodb:** full MongoDB wire-protocol proxy (PLAIN-over-TLS + SCRAM upstream auth, command classification/enforcement, query + result logging, API/UI integration, mid-session revoke/quota rejection), plus phase-5 enhancements — stored per-user SCRAM-SHA-256 verifiers, configurable per-database `authSource`, `loadBalanced` support, `OP_COMPRESSED` compression, filtered `listDatabases`, and `getMore` cursor lineage linking ([#255](https://github.com/fclairamb/dbbat/issues/255))

## [0.15.5](https://github.com/fclairamb/dbbat/compare/v0.15.4...v0.15.5) (2026-07-12)


### Bug Fixes

* **oracle:** migration name collision left users.protocol_data missing; don't mask store errors as user-not-found ([#245](https://github.com/fclairamb/dbbat/issues/245)) ([f8e343c](https://github.com/fclairamb/dbbat/commit/f8e343cc30e384bb9ec0748f1c9b256fd8dfdc11))

## [0.15.4](https://github.com/fclairamb/dbbat/compare/v0.15.3...v0.15.4) (2026-07-12)


### Bug Fixes

* **oracle:** per-user O5LOGON salts — any API key works for Oracle login ([#243](https://github.com/fclairamb/dbbat/issues/243)) ([2b3e8c0](https://github.com/fclairamb/dbbat/commit/2b3e8c0e16c39ad52fb1b2d4c3a26412ff06a640))
* **oracle:** shared-service-name resolution by grants + dbbat-name connect strings + dotted thin usernames ([#242](https://github.com/fclairamb/dbbat/issues/242)) ([430e704](https://github.com/fclairamb/dbbat/commit/430e704655950d148028d6a9c233b017c70fab21))

## [0.15.3](https://github.com/fclairamb/dbbat/compare/v0.15.2...v0.15.3) (2026-07-12)

Implements seven backlog specs in one squash-merged PR ([#240](https://github.com/fclairamb/dbbat/issues/240)) ([caa73cb](https://github.com/fclairamb/dbbat/commit/caa73cb7ed99742556f1179f1c9500084fa85bc1)); the individual changes are broken out below.

### Features

* **ui:** query detail breadcrumb now reads `Queries › <sql-preview>` — a link back to the queries list plus the first ~40 chars of the SQL — instead of a generic "Details"; the parent crumb now appears on every detail route and a bare UUID no longer collapses to "Details" ([#240](https://github.com/fclairamb/dbbat/issues/240))
* **ui:** grants list and grant-definitions always show the applied limits (`9 / 100 queries`, `169.8 MB / 1 GB`) with a usage bar, a warning colour ≥80%, destructive ≥100%, and an explicit `unlimited` marker when no limit is set ([#240](https://github.com/fclairamb/dbbat/issues/240))
* **api:** rename the connection-URL password placeholder `{API_KEY}` → `{DBBAT_KEY}` so it unambiguously names a dbbat-issued `dbb_…` token ([#240](https://github.com/fclairamb/dbbat/issues/240))


### Bug Fixes

* **ui:** the grant-definition edit dialog now opens pre-filled with the definition's current values instead of an empty form (which silently blanked the definition on save) ([#240](https://github.com/fclairamb/dbbat/issues/240))
* **ui:** the "New Definition" dialog now opens blank on consecutive opens instead of retaining the previously-submitted values ([#240](https://github.com/fclairamb/dbbat/issues/240))
* **oracle:** surface grant/auth denials as a clean error — no active grant → `ORA-01045`, bad credentials → `ORA-01017` — instead of tearing the socket down and letting the client report a generic `ORA-12566` / `ORA-03113` (root cause: the auth-reject frame used legacy TNS framing that v315+ clients misread) ([#240](https://github.com/fclairamb/dbbat/issues/240))
* **oracle:** rename the misleading `isPrintableASCII` helper (it only accepted the Oracle identifier set) and fix three latent call sites that truncated dotted usernames or rejected special-character passwords — the same class of bug as [#235](https://github.com/fclairamb/dbbat/issues/235) ([#240](https://github.com/fclairamb/dbbat/issues/240))

## [0.15.2](https://github.com/fclairamb/dbbat/compare/v0.15.1...v0.15.2) (2026-07-12)


### Features

* **ui:** hide the Active Connections stat card when there are none ([#238](https://github.com/fclairamb/dbbat/issues/238)) ([242bd8c](https://github.com/fclairamb/dbbat/commit/242bd8cd1f11ce206bd225b1e18068e23fbe7ead))


### Bug Fixes

* **deps:** update docusaurus monorepo to v3.10.2 ([#236](https://github.com/fclairamb/dbbat/issues/236)) ([25f4adb](https://github.com/fclairamb/dbbat/commit/25f4adb43ca67c66e60cbe79ef8f9a85a69724f1))
* **oracle:** harden Phase 2 rewrite against big-CLR-chunk connect strings ([#238](https://github.com/fclairamb/dbbat/issues/238)) ([242bd8c](https://github.com/fclairamb/dbbat/commit/242bd8cd1f11ce206bd225b1e18068e23fbe7ead))

## [0.15.1](https://github.com/fclairamb/dbbat/compare/v0.15.0...v0.15.1) (2026-07-10)


### Bug Fixes

* **ci:** prevent shell injection from commit message in image workflow ([#233](https://github.com/fclairamb/dbbat/issues/233)) ([a436a05](https://github.com/fclairamb/dbbat/commit/a436a05bac6e0b593fbc81272d28587f5e2991f7))
* **oracle:** handle dotted usernames in OCI/sqlplus auth (both phases) ([#235](https://github.com/fclairamb/dbbat/issues/235)) ([d822022](https://github.com/fclairamb/dbbat/commit/d822022fbb84466f473b3ac7db9f98ae73244803))

## [0.15.0](https://github.com/fclairamb/dbbat/compare/v0.14.0...v0.15.0) (2026-07-09)

Implements five backlog specs in one squash-merged PR ([#230](https://github.com/fclairamb/dbbat/issues/230)) ([42c4e37](https://github.com/fclairamb/dbbat/commit/42c4e3713c95091fef3b51a15dd54489813300c8)); the individual changes are broken out below.

### Features

* **ui:** admin user-management UI — edit users and promote/demote admin rights from the users page, guarded so the last admin can't be demoted or deleted (UI lock plus a backend `409` on update/delete) ([#230](https://github.com/fclairamb/dbbat/issues/230))

### Bug Fixes

* **oracle:** make sqlplus / OCI instant client work through the proxy — root-caused the long-standing stall to a malformed wide-encoding AUTH challenge (not the TCP-urgent OOB break probe that was long assumed), fixing four wide-encoding bugs; works even over an OOB-dropping network path ([#230](https://github.com/fclairamb/dbbat/issues/230))
* **config:** accept the documented `DBB_SLACK_SIGNING_SECRET` env var for the Slack signing secret, with the legacy `DBB_SLACK_NOTIFY_SIGNING_SECRET` kept as an accepted alias (canonical wins if both are set) ([#230](https://github.com/fclairamb/dbbat/issues/230))

### Documentation

* document the three Slack interactivity deployment shapes and Socket Mode (`DBB_SLACK_NOTIFY_APP_TOKEN`) for gated deployments, plus a startup warning when the inbound endpoint must be reachable from Slack ([#230](https://github.com/fclairamb/dbbat/issues/230))
* document HTTPRoute (Gateway API) exposure on the website and fix the Docusaurus build ([#230](https://github.com/fclairamb/dbbat/issues/230))

## [0.14.0](https://github.com/fclairamb/dbbat/compare/v0.13.0...v0.14.0) (2026-07-08)


### Features

* **api:** add Slack interactive grant approval (Approve/Deny buttons) ([#223](https://github.com/fclairamb/dbbat/issues/223)) ([516d4ca](https://github.com/fclairamb/dbbat/commit/516d4ca7b1152dcd5122a49dea071ef46b5961ea))
* **api:** add Slack Socket Mode transport for Approve/Deny interactions ([#229](https://github.com/fclairamb/dbbat/issues/229)) ([d1d33d4](https://github.com/fclairamb/dbbat/commit/d1d33d4c80c60cbb6b2cdeeddade0b095359a701))


### Bug Fixes

* **deps:** update module golang.org/x/crypto to v0.54.0 ([#226](https://github.com/fclairamb/dbbat/issues/226)) ([d53ede1](https://github.com/fclairamb/dbbat/commit/d53ede1b36bf573498d87982e11ab1ca24f1782f))
* **deps:** update module golang.org/x/text to v0.39.0 ([#221](https://github.com/fclairamb/dbbat/issues/221)) ([c10f410](https://github.com/fclairamb/dbbat/commit/c10f4101bc9b6c828d0a8165915213d6230019fe))
* **deps:** update module golang.org/x/text to v0.40.0 ([#225](https://github.com/fclairamb/dbbat/issues/225)) ([abf43ff](https://github.com/fclairamb/dbbat/commit/abf43ffcb91d1734123973855892f3f562f7fab6))

## [0.13.0](https://github.com/fclairamb/dbbat/compare/v0.12.0...v0.13.0) (2026-07-05)


### Features

* add Gateway API HTTPRoute support to the Helm chart ([#218](https://github.com/fclairamb/dbbat/issues/218)) ([1de8a62](https://github.com/fclairamb/dbbat/commit/1de8a627393779c075e17c2cf82241d8f902dccc))
* **oracle:** modern client support — sqlplus/OCI auth, SQLcl result capture, verifier-18453 ([#205](https://github.com/fclairamb/dbbat/issues/205)) ([a9858f6](https://github.com/fclairamb/dbbat/commit/a9858f6ccca16f26755fe126e961b816261cea6e))
* **oracle:** type-aware row & value capture with describe-record parser and bind extraction ([#195](https://github.com/fclairamb/dbbat/issues/195)) ([933a91e](https://github.com/fclairamb/dbbat/commit/933a91e5611adb985e82d0f500014a5f6212e1ef))


### Bug Fixes

* **ci:** restore unified dbbat-proxy service (pg+oracle+mysql) ([#220](https://github.com/fclairamb/dbbat/issues/220)) ([7bd9aa0](https://github.com/fclairamb/dbbat/commit/7bd9aa0793e77d62e037109b13acc4b0dd53e52f))
* **deps:** update module github.com/jackc/pgx/v5 to v5.10.0 ([#190](https://github.com/fclairamb/dbbat/issues/190)) ([d6a86c5](https://github.com/fclairamb/dbbat/commit/d6a86c55e2d4c1f88ae0296770f6e99fedd8e5e0))
* **deps:** update module github.com/knadh/koanf/v2 to v2.3.5 ([#187](https://github.com/fclairamb/dbbat/issues/187)) ([88f286e](https://github.com/fclairamb/dbbat/commit/88f286e519085eb4c4ea163eafc3428e185ef584))
* **deps:** update module github.com/slack-go/slack to v0.24.0 ([#184](https://github.com/fclairamb/dbbat/issues/184)) ([c532e11](https://github.com/fclairamb/dbbat/commit/c532e11a744671e1b5ac08de52eb057c314375ce))
* **deps:** update module github.com/slack-go/slack to v0.25.0 ([#191](https://github.com/fclairamb/dbbat/issues/191)) ([b6d2b01](https://github.com/fclairamb/dbbat/commit/b6d2b01aeafd2f22288228babfbf878e8c34a37a))
* **deps:** update module github.com/slack-go/slack to v0.26.0 ([#200](https://github.com/fclairamb/dbbat/issues/200)) ([5d72fc1](https://github.com/fclairamb/dbbat/commit/5d72fc18e99232444d1dcb1d0197a1fbd8fa7370))
* **deps:** update module github.com/slack-go/slack to v0.27.0 ([#210](https://github.com/fclairamb/dbbat/issues/210)) ([294d902](https://github.com/fclairamb/dbbat/commit/294d9020c058be7f8fe108395018abfb4b285049))
* **deps:** update module github.com/urfave/cli/v3 to v3.10.0 ([#201](https://github.com/fclairamb/dbbat/issues/201)) ([02d077e](https://github.com/fclairamb/dbbat/commit/02d077eb60e61a03bde4abb82e12cb3109f068c7))
* **deps:** update module github.com/urfave/cli/v3 to v3.10.1 ([#211](https://github.com/fclairamb/dbbat/issues/211)) ([76d52b9](https://github.com/fclairamb/dbbat/commit/76d52b914a08eb696ca09d44c684f759c3f8a7f7))
* **deps:** update module github.com/urfave/cli/v3 to v3.9.1 ([#196](https://github.com/fclairamb/dbbat/issues/196)) ([78b61fa](https://github.com/fclairamb/dbbat/commit/78b61fa5837953614bdf583c1e7f240e25b91ddd))
* **deps:** update module golang.org/x/crypto to v0.52.0 ([#180](https://github.com/fclairamb/dbbat/issues/180)) ([eadd0ba](https://github.com/fclairamb/dbbat/commit/eadd0bab2b5e8f27ad6909ba51e398f4580c48be))
* **deps:** update module golang.org/x/crypto to v0.53.0 ([#194](https://github.com/fclairamb/dbbat/issues/194)) ([0566f88](https://github.com/fclairamb/dbbat/commit/0566f88dc61a170ffe2396f885b29e6f69cc58e9))
* **deps:** update testcontainers-go monorepo to v0.43.0 ([#203](https://github.com/fclairamb/dbbat/issues/203)) ([6d9e06f](https://github.com/fclairamb/dbbat/commit/6d9e06fc47d43ee0fda06f818b6e80230dc861c7))

## [0.12.0](https://github.com/fclairamb/dbbat/compare/v0.11.0...v0.12.0) (2026-05-15)


### Features

* **ui:** add favicon from logo-notext.png ([#171](https://github.com/fclairamb/dbbat/issues/171)) ([35d82ba](https://github.com/fclairamb/dbbat/commit/35d82baea8dcb7fb15392c9fd21f21bf8aa277f3))


### Bug Fixes

* **dev:** fix dev mode routing so DBB_REDIRECTS proxy works without built frontend ([#173](https://github.com/fclairamb/dbbat/issues/173)) ([cb945d5](https://github.com/fclairamb/dbbat/commit/cb945d50c08097e049456467e011e57c8e1ff875))
* **ui:** fix favicon path and point preview at port 4200 ([#175](https://github.com/fclairamb/dbbat/issues/175)) ([863d123](https://github.com/fclairamb/dbbat/commit/863d123d38142eba2979f01d12fe339b0914a3b4))

## [0.11.0](https://github.com/fclairamb/dbbat/compare/v0.10.1...v0.11.0) (2026-05-15)


### Features

* **db:** add listable flag to databases ([#166](https://github.com/fclairamb/dbbat/issues/166)) ([c23c179](https://github.com/fclairamb/dbbat/commit/c23c17945ad2d03dac5004692b3f8f5ff847b9b4))
* grant definitions, grant requests, Slack notifications, global settings, connection URLs, auth fixes ([#168](https://github.com/fclairamb/dbbat/issues/168)) ([5136fc6](https://github.com/fclairamb/dbbat/commit/5136fc6833d0c9be5812249c395513096073e005))


### Bug Fixes

* **deps:** update module github.com/urfave/cli/v3 to v3.9.0 ([#163](https://github.com/fclairamb/dbbat/issues/163)) ([3a320b2](https://github.com/fclairamb/dbbat/commit/3a320b25ab2960bf02564927769c58bb3243d59b))
* **ui:** avoid setState-in-effect in PublicAdvertisementSection ([#169](https://github.com/fclairamb/dbbat/issues/169)) ([7c036fb](https://github.com/fclairamb/dbbat/commit/7c036fb9ad8591d14e89737fcc2a4376e1ac14d9))

## [0.10.1](https://github.com/fclairamb/dbbat/compare/v0.10.0...v0.10.1) (2026-05-11)


### Bug Fixes

* **auth:** re-creation of Slack-authenticated users after deletion ([#161](https://github.com/fclairamb/dbbat/issues/161)) ([a68af3a](https://github.com/fclairamb/dbbat/commit/a68af3ac01df0b1ca46d8d5a6abec4ca051ed77f))

## [0.10.0](https://github.com/fclairamb/dbbat/compare/v0.9.0...v0.10.0) (2026-05-10)


### Features

* **grants:** grant request workflow with Slack notifications and auto-refresh ([#157](https://github.com/fclairamb/dbbat/issues/157)) ([743fe20](https://github.com/fclairamb/dbbat/commit/743fe20201f96fc49173210431d54a6b5e68ee0b))
* **proxy:** PostgreSQL upstream TLS and SCRAM-SHA-256 auth ([#154](https://github.com/fclairamb/dbbat/issues/154)) ([196d5cc](https://github.com/fclairamb/dbbat/commit/196d5cc277882646e9628a4b68157078f3a58afb))


### Bug Fixes

* **config:** default Slack notify channel to #dbbat ([#159](https://github.com/fclairamb/dbbat/issues/159)) ([d47eba2](https://github.com/fclairamb/dbbat/commit/d47eba2fa9e1013923f49206c3f0dd08ec56b9a1))
* **deps:** update module github.com/knadh/koanf/parsers/toml/v2 to v2.2.1 ([#158](https://github.com/fclairamb/dbbat/issues/158)) ([616a10f](https://github.com/fclairamb/dbbat/commit/616a10f4de5a34c70bcac33210eeef5abacaf5b6))
* **deps:** update module github.com/slack-go/slack to v0.23.1 ([#160](https://github.com/fclairamb/dbbat/issues/160)) ([7d732e2](https://github.com/fclairamb/dbbat/commit/7d732e29149113a0cb6d5d1995d4235bb7128cdb))
* **deps:** update module golang.org/x/crypto to v0.51.0 ([#155](https://github.com/fclairamb/dbbat/issues/155)) ([7e04182](https://github.com/fclairamb/dbbat/commit/7e04182a16b3498ccc00af0a612056508c67dc4b))
* **deps:** update module golang.org/x/text to v0.37.0 ([#152](https://github.com/fclairamb/dbbat/issues/152)) ([58b3265](https://github.com/fclairamb/dbbat/commit/58b32655ac50c007c381f17a10fff79a51d77aa6))
* **grants:** populate query_count and bytes_transferred + UI polish ([#156](https://github.com/fclairamb/dbbat/issues/156)) ([e63de8c](https://github.com/fclairamb/dbbat/commit/e63de8cb0cc0fe185615b3e7d6e01d4abac88562))

## [0.9.0](https://github.com/fclairamb/dbbat/compare/v0.8.0...v0.9.0) (2026-05-08)


### Features

* **oracle:** customHash mode in O5LOGON server ([#143](https://github.com/fclairamb/dbbat/issues/143)) ([ff1a700](https://github.com/fclairamb/dbbat/commit/ff1a700429461ae7887f17d4da7fd4f8c6c2b465))
* **oracle:** derive combined key for empty-AUTH_PASSWORD path ([#148](https://github.com/fclairamb/dbbat/issues/148)) ([ce4bb2c](https://github.com/fclairamb/dbbat/commit/ce4bb2c709134256c50a2342f120e0fb9f50960b))
* **oracle:** forward client's actual Phase 1 to upstream ([#138](https://github.com/fclairamb/dbbat/issues/138)) ([126caa1](https://github.com/fclairamb/dbbat/commit/126caa18bc1f52fa7ac9b2f43f71d88f5f0f1da6))
* **oracle:** forward client's actual Phase 2 to upstream ([#144](https://github.com/fclairamb/dbbat/issues/144)) ([ebc9df1](https://github.com/fclairamb/dbbat/commit/ebc9df1aa3bdb5faf17a7ae29f6ba9389dbc8baf))


### Bug Fixes

* **oracle:** trim AUTH challenge end-marker to 33 bytes (SQLcl unblocks) ([#150](https://github.com/fclairamb/dbbat/issues/150)) ([189f011](https://github.com/fclairamb/dbbat/commit/189f0117883c178126ba15af20d544f2121d41f5))
* **oracle:** unbreak upstream auth parser; patch AUTH_SVR_RESPONSE ([#136](https://github.com/fclairamb/dbbat/issues/136)) ([db28eb6](https://github.com/fclairamb/dbbat/commit/db28eb673d5fb8e54bf6540b015ceb29436887a5))

## [0.8.0](https://github.com/fclairamb/dbbat/compare/v0.7.0...v0.8.0) (2026-05-06)


### Features

* **proxy:** PostgreSQL TLS termination ([#131](https://github.com/fclairamb/dbbat/issues/131)) ([8c76c00](https://github.com/fclairamb/dbbat/commit/8c76c00530244dd6fb50f98b7d9c324747e223a5))


### Bug Fixes

* **api:** fold accents in Slack OAuth username generation ([#130](https://github.com/fclairamb/dbbat/issues/130)) ([08b7fd7](https://github.com/fclairamb/dbbat/commit/08b7fd7b89fd0b853433574c52893e9a5861c19a))
* **oracle:** use user_id_len for structured Phase 1 username parsing ([#134](https://github.com/fclairamb/dbbat/issues/134)) ([5593564](https://github.com/fclairamb/dbbat/commit/5593564e81e274cfa7a9674722967459a4f695db))

## [0.7.0](https://github.com/fclairamb/dbbat/compare/v0.6.0...v0.7.0) (2026-05-06)


### Features

* **proxy:** add MySQL/MariaDB proxy with caching_sha2_password and TLS ([#112](https://github.com/fclairamb/dbbat/issues/112)) ([b916818](https://github.com/fclairamb/dbbat/commit/b916818e9ae3eec205d32db62d016265599b2a0f))
* **proxy:** harden MySQL upstream against LOCAL INFILE + verify binary row capture ([#115](https://github.com/fclairamb/dbbat/issues/115)) ([4a17b6f](https://github.com/fclairamb/dbbat/commit/4a17b6f135959d60ce1edb645e3bc31e4b2c0406))
* **proxy:** Oracle terminated auth — go-ora end-to-end working ([#118](https://github.com/fclairamb/dbbat/issues/118)) ([3a27833](https://github.com/fclairamb/dbbat/commit/3a278333936aadbc8180fc8d5d52cd443c1ff90f))
* **proxy:** wire up MySQL session packet dumps ([#116](https://github.com/fclairamb/dbbat/issues/116)) ([f7a81b8](https://github.com/fclairamb/dbbat/commit/f7a81b87b9693aef8caa7a5b3ec342ef93502a5f))


### Bug Fixes

* **deps:** update docusaurus monorepo to v3.10.1 ([#122](https://github.com/fclairamb/dbbat/issues/122)) ([102bd08](https://github.com/fclairamb/dbbat/commit/102bd08f83e099b7204482f30f23e578a210dec1))
* **deps:** update module github.com/go-mysql-org/go-mysql to v1.15.0 ([#128](https://github.com/fclairamb/dbbat/issues/128)) ([ad6b2a3](https://github.com/fclairamb/dbbat/commit/ad6b2a338fbb6d56d40969561c8427dd55a2649b))
* **deps:** update module github.com/go-sql-driver/mysql to v1.10.0 ([#132](https://github.com/fclairamb/dbbat/issues/132)) ([14be368](https://github.com/fclairamb/dbbat/commit/14be3682a8fed488fe4d5994a63e15068f179298))
* **deps:** update module github.com/jackc/pgx/v5 to v5.9.2 ([#108](https://github.com/fclairamb/dbbat/issues/108)) ([b837285](https://github.com/fclairamb/dbbat/commit/b837285f589cba224177cbd51fe523e31d995ec1))
* **proxy:** keep relay socket through AUTH so SQLcl avoids ORA-03120 ([#129](https://github.com/fclairamb/dbbat/issues/129)) ([a23c060](https://github.com/fclairamb/dbbat/commit/a23c0607bfbcd188c402e47e7499809d52c4feca))

## [0.6.0](https://github.com/fclairamb/dbbat/compare/v0.5.2...v0.6.0) (2026-04-15)


### Features

* **proxy:** activate Oracle terminated auth with O5LOGON and API keys ([#105](https://github.com/fclairamb/dbbat/issues/105)) ([d90d64e](https://github.com/fclairamb/dbbat/commit/d90d64e9da0a88907cbeb1b6dd2f5a9fdbf55395))

## [0.5.2](https://github.com/fclairamb/dbbat/compare/v0.5.1...v0.5.2) (2026-04-14)


### Bug Fixes

* **auth:** redirect OAuth callback to /login route instead of root ([#103](https://github.com/fclairamb/dbbat/issues/103)) ([842b5ba](https://github.com/fclairamb/dbbat/commit/842b5ba1bb9484f60949057cb249a08a360617ba))

## [0.5.1](https://github.com/fclairamb/dbbat/compare/v0.5.0...v0.5.1) (2026-04-14)


### Bug Fixes

* **config:** remove redundant slack_ prefix from SlackAuthConfig koanf tags ([#101](https://github.com/fclairamb/dbbat/issues/101)) ([e3882cd](https://github.com/fclairamb/dbbat/commit/e3882cd926b93378ea61acd2eb401a35e62354fa))

## [0.5.0](https://github.com/fclairamb/dbbat/compare/v0.4.0...v0.5.0) (2026-04-14)


### Features

* **proxy:** API key auth for PG proxy, Oracle auth, and Oracle SERVICE_NAME handling rewrite ([#79](https://github.com/fclairamb/dbbat/issues/79)) ([83858a1](https://github.com/fclairamb/dbbat/commit/83858a136648ca866ef4906fbc79c170ae2dfa4b))


### Bug Fixes

* **deps:** update dependency zod to v4 ([#91](https://github.com/fclairamb/dbbat/issues/91)) ([c92061a](https://github.com/fclairamb/dbbat/commit/c92061a03fe38e373f9d5199435c5f3f8e87b864))
* **deps:** update docusaurus monorepo to v3.10.0 ([#89](https://github.com/fclairamb/dbbat/issues/89)) ([a6c417c](https://github.com/fclairamb/dbbat/commit/a6c417c9fdffcaa5dd59f1c8beceefe6ded3a0d2))
* **deps:** update module golang.org/x/crypto to v0.50.0 ([#93](https://github.com/fclairamb/dbbat/issues/93)) ([13a91fa](https://github.com/fclairamb/dbbat/commit/13a91fade65ee4ea68719980a39bb9eae458b148))
* **deps:** update testcontainers-go monorepo to v0.41.0 ([#88](https://github.com/fclairamb/dbbat/issues/88)) ([79fecd7](https://github.com/fclairamb/dbbat/commit/79fecd78ee8a2c0498f5275597a687feedb1d159))
* **deps:** update testcontainers-go monorepo to v0.42.0 ([#94](https://github.com/fclairamb/dbbat/issues/94)) ([9977e30](https://github.com/fclairamb/dbbat/commit/9977e30b9a60afd6a6e2745de0bd285b921ec9a5))

## [0.4.0](https://github.com/fclairamb/dbbat/compare/v0.3.0...v0.4.0) (2026-04-04)


### Features

* **proxy:** Add a first draft of Oracle support ([#75](https://github.com/fclairamb/dbbat/issues/75)) ([908abbb](https://github.com/fclairamb/dbbat/commit/908abbbca1c87d1aa3cf82cad9d0088df52c9df0))
* **proxy:** Oracle session TNS dump capture ([#78](https://github.com/fclairamb/dbbat/issues/78)) ([7e0b45a](https://github.com/fclairamb/dbbat/commit/7e0b45a4e1d86754b93b6a59779925a998027896))


### Bug Fixes

* **ci:** autoupdate ([#57](https://github.com/fclairamb/dbbat/issues/57)) ([bb9b9ad](https://github.com/fclairamb/dbbat/commit/bb9b9ad913fbaab67750a9d44783b24106bed175))
* **deps:** update dependency openapi-fetch to ^0.16.0 ([#68](https://github.com/fclairamb/dbbat/issues/68)) ([567412d](https://github.com/fclairamb/dbbat/commit/567412dca3ea95ad2ea2cfca6e70004e18cccc6f))
* **deps:** update module github.com/knadh/koanf/v2 to v2.3.2 ([#59](https://github.com/fclairamb/dbbat/issues/59)) ([9b57552](https://github.com/fclairamb/dbbat/commit/9b575525abe7887f3f524c46e28d10573d4269a5))
* **deps:** update module golang.org/x/crypto to v0.48.0 ([#69](https://github.com/fclairamb/dbbat/issues/69)) ([ff7856b](https://github.com/fclairamb/dbbat/commit/ff7856b26c426b6dfbba678f2327ef34c67d8e9a))
* resolve test races, E2E error format, and lint issues ([#77](https://github.com/fclairamb/dbbat/issues/77)) ([c95b303](https://github.com/fclairamb/dbbat/commit/c95b303bbed9936509100932dc017a73b25678c6))

## [0.3.0](https://github.com/fclairamb/dbbat/compare/v0.2.0...v0.3.0) (2026-01-24)


### Features

* **api:** add admin password reset endpoint ([#51](https://github.com/fclairamb/dbbat/issues/51)) ([529fc92](https://github.com/fclairamb/dbbat/commit/529fc92da65b8e1ffe831d06530a93493b4c4602))
* **ui:** add quota fields to grant creation form ([#49](https://github.com/fclairamb/dbbat/issues/49)) ([626fa90](https://github.com/fclairamb/dbbat/commit/626fa90e74ac4de7d4be0970a63cf0fc1ffbc234))


### Bug Fixes

* **deps:** update dependency lucide-react to ^0.563.0 ([#48](https://github.com/fclairamb/dbbat/issues/48)) ([e7ca0b6](https://github.com/fclairamb/dbbat/commit/e7ca0b664b943bc1489f9685c0984f9a783adbe4))
* **deps:** update module github.com/knadh/koanf/v2 to v2.3.1 ([#52](https://github.com/fclairamb/dbbat/issues/52)) ([8b26ec9](https://github.com/fclairamb/dbbat/commit/8b26ec9259873c239c611ba4f1cffc948f524bd8))
* **deps:** update module github.com/urfave/cli/v3 to v3.6.2 ([#46](https://github.com/fclairamb/dbbat/issues/46)) ([32684fe](https://github.com/fclairamb/dbbat/commit/32684fed5d6e94b04c7715c104f08a4bfde66f8a))
* reduce argon2id memory and protect admin in demo mode ([#50](https://github.com/fclairamb/dbbat/issues/50)) ([fc90f6d](https://github.com/fclairamb/dbbat/commit/fc90f6d34581042e22bedc6d846c8bf31299fe64))
* **test:** remove flaky toBeDisabled assertions in E2E tests ([#54](https://github.com/fclairamb/dbbat/issues/54)) ([2dc8e1a](https://github.com/fclairamb/dbbat/commit/2dc8e1a4d01bcd396bba4cd01923488a7060e81f))

## [0.2.0](https://github.com/fclairamb/dbbat/compare/v0.1.0...v0.2.0) (2026-01-13)


### Features

* **ui:** add time precision to grant date inputs ([#39](https://github.com/fclairamb/dbbat/issues/39)) ([e465e2c](https://github.com/fclairamb/dbbat/commit/e465e2cd53ce75ff817f74cc5e4b061dd68d8d8b))


### Bug Fixes

* **deps:** update module github.com/knadh/koanf/parsers/toml to v2 ([#35](https://github.com/fclairamb/dbbat/issues/35)) ([7c56f99](https://github.com/fclairamb/dbbat/commit/7c56f9932f87309259f29f5a66a72eeb4f255cf5))
* **deps:** update module github.com/knadh/koanf/parsers/toml to v2 ([#37](https://github.com/fclairamb/dbbat/issues/37)) ([767bcc8](https://github.com/fclairamb/dbbat/commit/767bcc8dcd9e3271d1229ec3053f3af19b770925))
* **deps:** update module github.com/knadh/koanf/providers/env to v2 ([#38](https://github.com/fclairamb/dbbat/issues/38)) ([bfe821c](https://github.com/fclairamb/dbbat/commit/bfe821cc0db20bf3692b1f3f0eca2c677709ed21))

## [0.1.0](https://github.com/fclairamb/dbbat/compare/v0.0.2...v0.1.0) (2026-01-12)


### Features

* **config:** add configurable log level with strict sloglint compliance ([#31](https://github.com/fclairamb/dbbat/issues/31)) ([51fa451](https://github.com/fclairamb/dbbat/commit/51fa451df2e90150e59dd1dad586c9bfd70998af))


### Bug Fixes

* **deps:** update module golang.org/x/crypto to v0.47.0 ([#34](https://github.com/fclairamb/dbbat/issues/34)) ([c18a454](https://github.com/fclairamb/dbbat/commit/c18a454c6b29f2a371cadc478a4d51601b479c79))


### Performance Improvements

* **auth:** extend AuthCache to API key and web session verification ([#32](https://github.com/fclairamb/dbbat/issues/32)) ([fa21f84](https://github.com/fclairamb/dbbat/commit/fa21f846f404fe8161abf5620c0fc0bfb655de56))

## [0.0.2](https://github.com/fclairamb/dbbat/compare/v0.0.1...v0.0.2) (2026-01-11)


### Bug Fixes

* use PAT for release-please to trigger release workflow ([#28](https://github.com/fclairamb/dbbat/issues/28)) ([1566de4](https://github.com/fclairamb/dbbat/commit/1566de4c2bd26628ef6303ce853f4163fce0f2a9))

## [0.0.1](https://github.com/fclairamb/dbbat/compare/v0.0.0...v0.0.1) (2026-01-11)

### Bug Fixes

* **ui:** handle 401 errors by redirecting to login page ([#24](https://github.com/fclairamb/dbbat/issues/24)) ([b8d205c](https://github.com/fclairamb/dbbat/commit/b8d205c8d08eaf890ff885bf28f34e46baf76d5c))

### Performance Improvements

* **auth:** implement password verification cache and configurable hash parameters ([#25](https://github.com/fclairamb/dbbat/issues/25)) ([ea0dd0b](https://github.com/fclairamb/dbbat/commit/ea0dd0b2ccc139f650e1908da0a532e0d7af63e4)), closes [#22](https://github.com/fclairamb/dbbat/issues/22)
