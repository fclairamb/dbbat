---
sidebar_position: 6
sidebar_label: Compliance
title: Compliance
description: How DBBat's capabilities map to SOC 2, ISO/IEC 27001:2022 and PCI DSS v4.x controls — and what it deliberately does not claim.
---

# Compliance

:::danger DBBat is not certified against anything

DBBat holds no SOC 2 report, no ISO/IEC 27001 certificate and no PCI DSS
Attestation of Compliance. It is an AGPL-3.0 project you download and run
yourself, so there is nothing to inherit.

**DBBat is a control you deploy, not a certificate you inherit.**

This page exists because the question your compliance team will ask is not
"is DBBat certified?" — it is "what evidence does DBBat produce for *our*
audit?". Everything below answers that second question.

:::

## How to read this page

- The mappings describe **your** control environment. DBBat supplies the
  mechanism and the evidence; your policies, your scope definition and your
  auditor decide whether that evidence is sufficient.
- Framework versions cited here: **AICPA Trust Services Criteria (2017, with
  2022 revised points of focus)**, **ISO/IEC 27001:2022 Annex A**, and
  **PCI DSS v4.0 / v4.0.1**. Requirement numbers were re-checked against those
  revisions — PCI DSS reorganised Requirement 10 between v3.2.1 and v4.0, so
  older mappings floating around the internet cite the wrong numbers for log
  protection and log retention.
- Every row describes a capability that ships today.
- [What DBBat does not do](#what-dbbat-does-not-do) is the part your auditor
  will care about most. Read it before you write DBBat into a control
  narrative.

## The evidence DBBat produces

The mappings below all draw on the same underlying records, so it helps to know
exactly what exists:

| Record | Contents |
|---|---|
| **Connection** | The named DBBat user, the target database, the source IP, connect/disconnect/last-activity timestamps, the grant the session ran under, a query counter, bytes transferred, whether the upstream leg was TLS, and the capture key if session capture is on |
| **Query** | SQL text, decoded bind parameters, execution timestamp, duration, rows affected, any error, COPY direction/format, and — when an approval hold fired — the matching pattern, the outcome, who resolved it, when and why |
| **Result rows** *(optional)* | The actual rows returned, captured alongside the query and bounded by `max_result_rows` / `max_result_bytes` |
| **Audit log** | Administrative events against the configuration API: user/database/grant/grant-definition/grant-request/API-key creation, update, revocation and deletion, with the acting user |
| **Session capture** *(optional)* | The whole post-auth command stream as a [pcapng file](/docs/features/session-dumps) Wireshark reads natively |
| **Chain MACs** | On every audit entry and every query row: a keyed HMAC over the record plus the previous record's MAC, so the audit log and the query history are [tamper-evident](/docs/features/audit-chain) |

One structural detail worth knowing before you write an evidence procedure: the
user, the source IP and the byte counter live on the **connection**, not on each
query. A query is attributed to a person by joining it to its connection —
which is what `GET /api/v1/queries` does for you, but it matters if you query
the store directly.

All of this is stored in the PostgreSQL database DBBat runs on. Backups,
retention beyond DBBat's own sweep, and access control on that store are yours
to operate.

## SOC 2 (AICPA Trust Services Criteria)

| DBBat capability | Criterion | How it helps |
|---|---|---|
| Per-query logging across PostgreSQL, Oracle, MySQL/MariaDB, MongoDB and SQL Server, each query attributable to a named DBBat user | **CC7.2** — monitoring system components for anomalies indicative of malicious acts and errors | Gives you a queryable record of every statement run against production, which is the population an anomaly review is performed over. |
| Time-boxed grants with `starts_at` / `expires_at`, automatic expiry and immediate revocation | **CC6.1**, **CC6.2** | Access is granted for a window and removed when the window closes, without anyone remembering to clean up; revocation also tears down sessions that are already live. |
| Self-service [grant requests](/docs/features/grant-requests) with a justification, approved by an admin before any access exists | **CC6.2** — users are registered and authorized before credentials are issued | The request, the justification, the approver and the timestamp are all recorded, which is the authorization evidence CC6.2 asks for. |
| Grant controls (`read_only`, `block_copy`, `block_ddl`) and quotas (`max_query_counts`, `max_bytes_transferred`) | **CC6.3** — least privilege | Access is scoped to what the task needs; a read-only investigation cannot become a write, and a bounded export cannot become a bulk extraction. |
| [Approval holds](https://github.com/fclairamb/dbbat/blob/main/docs/approvals.md): a statement matching a pattern is suspended mid-flight until a second person approves — **self-approval is always rejected** | **CC6.3** (segregation of duties), **CC8.1** | CC8.1 covers changes to *data* as well as software; a held statement cannot execute until someone other than its author approves it, and the approval is recorded on the query row. |
| Grant definitions are immutably versioned — an edit archives the current row and creates a successor, and a live grant keeps running the version it was issued under | **CC8.1** — changes are authorized, documented and implemented | The exact policy in force for any historical session is recoverable, so a change to a definition cannot silently rewrite the past. |
| Deactivating a grant definition withdraws the whole lineage and fails closed at authentication time; hard deletion is refused while anything still references it | **CC6.2**, **CC6.3** | Removal of access is a single, immediate, auditable action, and the audit history it is evidence for cannot be deleted out from under you. |
| Upstream database credentials encrypted with AES-256-GCM (AAD-bound to the database record, so a stolen ciphertext cannot be transplanted); DBBat user passwords and API keys hashed with Argon2id | **CC6.1** | Users never receive the production credential at all — they authenticate to DBBat, and DBBat holds the secret. |
| [HMAC-chained audit log and query history](/docs/features/audit-chain), verified with `dbbat audit verify`; the key is derived from `DBB_KEY` and never stored in the database | **CC7.1**, **CC7.2** — the integrity of the monitoring record itself | Modifying, deleting or reordering a record is detectable by anyone who can run the verification, including when the person doing it holds write access to DBBat's own PostgreSQL store. It is detection, not prevention — see [what DBBat does not do](#what-dbbat-does-not-do). |
| Per-session [pcapng capture](/docs/features/session-dumps), readable by Wireshark and tcpdump | **CC7.3** — evaluating security events | When a query log entry is not enough, the raw session is available for forensic reconstruction in an open format. |
| `DBB_QUERY_STORAGE_RETENTION` and `DBB_DUMP_RETENTION` | **C1.2** — disposal of confidential information *(only in scope if you selected the Confidentiality category)* | Query history, captured result rows and local captures age out on a schedule you set, rather than accumulating forever. |

## ISO/IEC 27001:2022 (Annex A)

| DBBat capability | Annex A control | How it helps |
|---|---|---|
| Per-query logging with user attribution across all five protocols; administrative changes recorded separately in the audit log | **A.8.15 Logging** | Produces the "user activities, exceptions and faults" record A.8.15 requires, at statement granularity rather than at session granularity. |
| [HMAC-chained records](/docs/features/audit-chain): each audit entry and each statement carries a MAC over its content plus the previous record's MAC, verified offline with `dbbat audit verify`; the key is HKDF-derived from `DBB_KEY` and never written to the database | **A.8.15 Logging** (log protection), **A.5.33 Protection of records** | A.8.15 requires logs to be protected against unauthorised alteration; the chain makes alteration, deletion and reordering detectable without the log ever leaving DBBat. Read access to the store is not enough to forge it, and neither is write access — only the key is. It detects tampering rather than preventing it, and does not cover rows written before the chain anchor. |
| Query browsing and filtering by user, database, time range and connection via the API and web UI | **A.8.16 Monitoring activities** | The recorded activity is reviewable, which is what turns a log into a monitoring control. |
| Time-boxed, scoped grants issued against reusable definitions | **A.5.15 Access control**, **A.8.3 Information access restriction** | Access rules are defined centrally as definitions and applied per user and per database, with the proxy enforcing them on the wire. |
| Grant provisioning, automatic expiry, immediate revocation, and grant listings filterable by user and database (`GET /api/v1/grants?active_only=true`) | **A.5.18 Access rights** | Covers the provision, review and removal of access rights, and gives you the who-has-access-to-what report a periodic review is performed against. |
| Approval holds requiring a second person, with self-approval rejected in code | **A.5.3 Segregation of duties** | A single individual cannot both initiate and authorize a sensitive statement, which is the outcome A.5.3 asks for. |
| Approvers are the admins plus the grant definition's designated approver groups; if the grant cannot be resolved it falls back to admins only | **A.5.3**, **A.8.2 Privileged access rights** | Authorization is bound to an explicit, configured group rather than to whoever happens to be online, and fails closed. |
| Upstream credentials encrypted with AES-256-GCM (AAD-bound); Argon2id for user passwords and API keys; TLS terminated on the client leg and available on the upstream leg | **A.8.24 Use of cryptography** | Secrets at rest are encrypted with an authenticated cipher and an explicit key supplied through `DBB_KEY` / `DBB_KEYFILE`. |
| Per-session pcapng captures plus `dbbat dump anonymise` for sharing them | **A.5.28 Collection of evidence** | Evidence is captured in a standard, tool-readable format, and can be stripped of session metadata before it leaves the organisation. |
| `DBB_QUERY_STORAGE_RETENTION` (query history and captured rows) and `DBB_DUMP_RETENTION` (local capture spool) | **A.8.10 Information deletion** | Retention is an explicit, configured duration, and the sweep runs continuously rather than as a manual clean-up. |
| Grant definitions are immutably versioned; deactivation withdraws the whole lineage and fails closed at authentication time; hard deletion is refused while referenced | **A.5.18 Access rights** (withdrawal), **A.5.33 Protection of records** | Withdrawing a definition removes access everywhere it was granted, in one action — and the record of what was authorized, under which policy version, cannot be quietly altered or removed afterwards. |

## PCI DSS v4.0 / v4.0.1

DBBat is relevant to PCI DSS only insofar as it fronts a database inside your
cardholder data environment. It does not classify, discover or mask PAN data —
see [what DBBat does not do](#what-dbbat-does-not-do).

| DBBat capability | Requirement | How it helps |
|---|---|---|
| Grants scope a named user to a specific database with explicit controls, issued from definitions that describe what may be requested | **7.2** — access to system components and data is appropriately defined and assigned | Access is assigned per person and per database with least privilege expressed as controls and quotas, rather than through shared database logins. |
| `GET /api/v1/grants?user_id=&database_id=&active_only=true`, plus the grants page in the web UI | **7.2.4** — user accounts and access privileges are reviewed at least every six months | Gives the reviewer a current, filterable list of who holds access to which database and under which controls. Note that DBBat produces the *report*; the review itself, and its sign-off, remain your process. |
| Every session authenticates as an individual named DBBat user (password or API key); the shared upstream credential is never handed out | **8.2** — user identification is strictly managed; **7.3** — access managed via an access control system | Removes the shared production credential from the human workflow, so activity in the log is attributable to a person. |
| DBBat user passwords and API keys hashed with Argon2id; upstream credentials encrypted with AES-256-GCM | **8.3.2** — strong cryptography renders authentication factors unreadable during storage | Neither the user's password nor the API key is recoverable from the store; only an 8-character key prefix is kept in clear, for lookup. |
| Per-query logging with the user, the database, the timestamp, the source IP, the statement and its success or failure | **10.2** — audit logs support detection of anomalies and forensic analysis; **10.2.2** — logs record user identification, event type, date and time, success/failure indication and the affected resource | Every element 10.2.2 enumerates is present, at statement granularity, for all five supported engines. |
| [HMAC-chained audit log and query history](/docs/features/audit-chain) with an offline `dbbat audit verify` command; the chain key is derived from `DBB_KEY` and never stored alongside the records | **10.3** — audit logs are protected from destruction and unauthorized modifications; **10.3.2** — log files are protected to prevent modifications | 10.3 asks that logs cannot be altered without detection. Each record is sealed against the one before it, so an insertion, an edit, a deletion or a reordering is caught by the verification — including one performed directly against DBBat's PostgreSQL store. Note the scope precisely for your assessor: this is **detection**, not write-protection, it does not cover rows written before the chain anchor, and on the query side it seals the statement and its parameters rather than its reported outcome. Restricting who can reach the storage database (**10.3.1**) remains yours. |
| `DBB_QUERY_STORAGE_RETENTION` (off by default — history is kept indefinitely unless you set it) | **10.5** — audit log history is retained and available for analysis | The default keeps everything, which satisfies 10.5.1's twelve months by construction; set a retention only if a data-minimisation obligation requires it, and set it above twelve months if this database is in PCI scope. |
| Per-session pcapng capture of the command stream | **10.2**; supports **12.10** — suspected and confirmed incidents are responded to immediately | Supplements the structured log with the raw session when an investigation needs byte-level detail. |

## What DBBat does not do

Skipping this section is how a control narrative gets a finding. None of the
following exists today:

- **Tamper-evidence is detection, not prevention, and it has edges.** The
  [audit chain](/docs/features/audit-chain) makes modification, deletion and
  reordering *detectable*; it does not stop anyone with write access to DBBat's
  storage database from doing it, and there is no append-only enforcement or
  WORM storage. Four limits matter in a control narrative: rows written **before
  the chain anchor** (i.e. before you upgraded to the release that introduced
  it) carry no MAC and are reported as unverifiable; on the query side the MAC
  covers the statement, its parameters and its position but **not** the outcome
  columns written after execution; a chain always verifies against itself, so
  detecting a wholesale truncate-and-re-seal by someone who holds `DBB_KEY`
  requires you to **record the head MAC outside the database**; and the key
  itself is only as protected as the host. If your control requires
  *prevention* rather than detection, keep shipping the logs to a WORM store or
  a SIEM as well.
- **No MFA and no password policy on DBBat's own accounts.** There is no TOTP,
  no WebAuthn, and no minimum length, complexity, rotation or reuse check on
  DBBat passwords. **PCI DSS 8.3.1, 8.4 and the 8.3.6 password-strength rules
  cannot be met by DBBat's local passwords.** The mitigation is to disable local
  passwords in practice and authenticate through Slack OAuth, so MFA and the
  password policy are enforced by your identity provider. The one built-in
  guard-rail is that the seeded `admin` / `admin` account is refused at login
  until its password is changed.
- **No data masking or result redaction.** DBBat logs and gates statements; it
  does not rewrite result sets. Nothing here helps with **PCI DSS 3.4**
  (restricting displays of full PAN) or **ISO A.8.11** (data masking).
- **`dbbat dump anonymise` is metadata scrubbing, not redaction.** It empties the
  session's database name, username and upstream address, rebases timestamps to
  the epoch and regenerates the synthesized addresses — but **packet payloads
  are preserved verbatim**, including SQL text, bind values and returned rows. A
  capture from a production database still contains production data after
  anonymisation.
- **No access-review workflow.** DBBat reports who has access; it has no
  attestation, recertification or reviewer sign-off record. **PCI DSS 7.2.4**
  and the review half of **ISO A.5.18** need a process you run on top of the
  report.
- **No directory integration beyond Slack OAuth.** No SCIM, no LDAP/AD sync, no
  certificate-based authentication. Joiner/mover/leaver has to be driven through
  DBBat's API or UI.
- **Blocked statements are not uniformly persisted.** A statement refused by
  `read_only` or `block_ddl` never reaches the upstream database on any
  protocol. It is recorded in query history with its error on MySQL/MariaDB,
  MongoDB and SQL Server — but on PostgreSQL and Oracle the refusal currently
  appears only in the process log and in the session capture, not as a query row.
  If logging invalid access attempts is an evidence requirement for you
  (**PCI DSS 10.2.1.4**), account for that gap.
- **Retention and backup of the evidence itself is yours.** DBBat's sweep
  deletes old records; it does not archive them. `DBB_DUMP_RETENTION` applies
  only to the local capture spool — captures uploaded to a blob bucket are never
  expired by DBBat, so remote retention is your bucket's lifecycle policy.
- **Time synchronisation is the host's job.** DBBat timestamps from the system
  clock. **PCI DSS 10.6** is satisfied by your NTP configuration, not by DBBat.
- **Scope.** DBBat governs database access only. There is no SSH, Kubernetes or
  RDP story here; those controls need a different tool.

## Using this in an audit

Everything an assessor asks for is reachable through the versioned REST API, so
evidence collection can be scripted rather than screenshotted:

```bash
# Every statement run in a period, with user and database attribution
curl -H "Authorization: Bearer $DBBAT_API_KEY" \
  "http://localhost:4200/api/v1/queries?start_time=2026-01-01T00:00:00Z&end_time=2026-03-31T23:59:59Z"

# Who holds access right now, and under what controls
curl -H "Authorization: Bearer $DBBAT_API_KEY" \
  "http://localhost:4200/api/v1/grants?active_only=true"

# Administrative changes: users, grants, definitions, API keys
curl -H "Authorization: Bearer $DBBAT_API_KEY" \
  "http://localhost:4200/api/v1/audit"

# Sessions, with source IP, timing and bytes transferred
curl -H "Authorization: Bearer $DBBAT_API_KEY" \
  "http://localhost:4200/api/v1/connections"
```

Integrity of that evidence is a separate, offline step — it needs the
encryption key, so it runs on the host rather than over the API:

```bash
# The administrative audit log; exits non-zero if the chain is broken
dbbat audit verify

# The per-connection query history
dbbat audit verify --queries
```

Record the reported `head_mac` alongside your evidence: comparing it against the
head from the previous period is what detects a chain that was truncated and
re-sealed. See [Tamper-Evident Audit Log](/docs/features/audit-chain).

Reading queries, the audit log and other users' grants requires the `admin` or
`viewer` role; a plain user only ever sees their own grants. See the
[API reference](/docs/api) for authentication, pagination and filters.

## See also

- [Access Control](/docs/features/access-control) — grants, controls, quotas, revocation
- [Grant Requests & Auto-Approval](/docs/features/grant-requests) — the request and approval flow
- [Query Logging](/docs/features/query-logging) — what is recorded and how retention works
- [Tamper-Evident Audit Log](/docs/features/audit-chain) — the HMAC chain and `dbbat audit verify`
- [Session Packet Captures](/docs/features/session-dumps) — pcapng captures and anonymisation
- [Security](/docs/security) — the cryptography and hardening detail behind these mappings
- [Approval holds](https://github.com/fclairamb/dbbat/blob/main/docs/approvals.md) — the four-eyes design in depth
