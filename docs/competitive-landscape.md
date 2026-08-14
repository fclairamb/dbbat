# Competitive Landscape

*Last reviewed: August 2026. Facts below come from public docs and repos; commercial
feature gating changes often — re-verify before quoting in marketing material.*

DBBat sits in the "database access governance" space: a transparent wire-protocol
proxy that gives humans (and AI agents) governed access to production databases —
every query logged, access time-boxed and scoped, risky statements held for a
second pair of eyes. This document maps who else plays there and how they differ.

## TL;DR

- **Closest commercial competitor: [hoop.dev](https://hoop.dev)** — same
  wire-protocol-proxy architecture, same query-level approval concept, plus
  ML-based data masking. No Oracle support listed.
- **Closest open-source competitors: [Bytebase](https://www.bytebase.com)**
  (huge community, but a governed web SQL editor rather than a transparent
  proxy, and the governance features are Enterprise-only) and
  **[AccessFlow](https://github.com/bablsoft/accessflow)** (near-identical
  feature list, but a submit-review-execute model, and very early stage).
- **The incumbents to displace: [Teleport](https://goteleport.com) and
  [StrongDM](https://www.strongdm.com)** — infrastructure-wide access platforms
  where database access is one module among SSH/K8s/RDP. Far heavier to adopt
  and operate than a single-purpose proxy.
- **DBBat's defensible angles:** transparent proxy across five protocols
  including a hand-rolled **Oracle TNS/TTC** implementation (rare everywhere,
  almost nonexistent in open source), **mid-flight approval holds** on a live
  connection, protocol-agnostic **pcapng session capture** readable by
  Wireshark/tcpdump, and a deliberately small operational footprint (one Go
  binary + one PostgreSQL).

## Comparison matrix

| | DBBat | hoop.dev | Teleport | StrongDM | Border0 | Bytebase | JumpServer | AccessFlow |
|---|---|---|---|---|---|---|---|---|
| **Model** | Transparent wire proxy | Transparent wire proxy | Access platform (proxy) | Access platform (proxy) | Protocol-aware proxy / PAM | Governed web SQL editor | PAM / web sessions | Submit-review-execute proxy |
| **Client keeps native tools** (psql, DBeaver, drivers) | Yes | Yes | Yes (via `tsh`/cert) | Yes (via local client) | Yes | No (their UI) | Partly (web terminal) | No (their UI) |
| **PostgreSQL** | Yes | Yes | Yes | Yes | Yes | Yes | Yes | Yes |
| **MySQL/MariaDB** | Yes | Yes | Yes | Yes | Yes | Yes | Yes | Yes |
| **Oracle** | **Yes (hand-rolled TNS/TTC)** | Not listed | Yes | Yes | Not listed | Yes | Yes | Yes |
| **MongoDB** | Yes | Yes | Yes | Yes | Not listed | Yes | Yes | Yes |
| **SQL Server (TDS)** | Yes | Yes | Yes | Yes | Not listed | Yes | Yes | Yes |
| **Per-query audit log** | Yes | Yes (incl. responses) | Yes | Yes | Yes | Yes | Session recording | Yes (HMAC-chained) |
| **Time-boxed / JIT grants** | Yes | Yes | Yes (Access Requests) | Yes | Yes | Enterprise only | Yes | Yes |
| **Statement-level approval** | **Yes — mid-flight hold on a live connection** | Yes (query routed for review) | Request-level, not statement-level | Request-level | Request-level | Enterprise; per-issue review | Ticket-based | Yes — before execution |
| **Data masking** | No | Yes (ML, ~150 PII types) | Limited | Limited | No | Enterprise only | No | Yes (column policies) |
| **Session capture format** | **pcapng (Wireshark-readable)** | Proprietary replay | Proprietary | Proprietary | Proprietary | N/A | Video-style replay | N/A |
| **Slack-native approvals** | Yes (buttons, Socket Mode) | Yes | Via plugins | Yes | Yes | Via IM integration | Via bots | Slack/Teams/PagerDuty |
| **License** | AGPL-3.0 | Open core (CNCF member) | Open core (Community/Enterprise) | Proprietary | Proprietary | Open core; governance = Enterprise | GPLv3 + commercial | Apache-2.0 |
| **Deployment** | Self-hosted, 1 binary + PostgreSQL | SaaS + self-hosted | Self-hosted + Cloud | SaaS control plane + gateways | SaaS control plane | Self-hosted + Cloud | Self-hosted | Self-hosted |
| **Scope beyond databases** | No — databases only | HTTP APIs, terminals | SSH, K8s, RDP, apps | SSH, K8s, cloud, web | SSH, K8s, RDP, web | Schema change management | SSH, RDP, K8s | REST/SOAP/GraphQL/gRPC |

## Direct competitors

### hoop.dev — the closest match

A Layer-7 gateway that parses database wire protocols natively (PostgreSQL,
MySQL, MSSQL, MongoDB — no Oracle listed), so engineers and AI agents keep
their normal clients. Logs every query *and every response*, applies ML-based
masking of ~150 PII types at the wire, blocks dangerous commands, and routes
risky operations to human approval. Open-source core, CNCF member, commercial
SaaS and self-hosted offerings. Their positioning has pivoted hard toward
governing **AI-agent access** to production data.

*vs DBBat:* the same architectural bet and the same approval concept. They win
on masking and response inspection; DBBat wins on Oracle, on the open pcapng
capture format, and on operational simplicity. Their AI-agent framing is the
strongest marketing in the space and worth studying.

### Teleport (Database Access)

Open-core zero-trust access platform (SSH, Kubernetes, RDP, apps, databases).
Database access uses short-lived certificates via SSO, RBAC with Access
Requests (request → review → time-boxed elevation), and full audit logging.
Supports a very wide database matrix, including self-hosted and RDS Oracle
(some databases and features are Enterprise-gated). Recent releases added MCP
support so AI clients like Claude Desktop can query protected databases.

*vs DBBat:* Teleport approves *access*, not *statements* — once a request is
granted, individual queries are logged but never held for review. Adopting it
means adopting the whole platform (certificate authority, agents on every
node, `tsh` client workflows). Strong choice for orgs wanting one platform for
everything; heavy for the "just govern prod database access" problem.

### StrongDM

Proprietary Zero-Trust PAM: SaaS control plane with self-hosted gateways
proxying PostgreSQL, MySQL, Oracle, MongoDB, SQL Server, and dozens more
resource types. Per-query audit, just-in-time access with automated approval
workflows, credential vaulting and rotation, and the compliance-certification
checklist (SOC 2, HIPAA, PCI DSS, ISO 27001) enterprise buyers ask for.

*vs DBBat:* the enterprise incumbent. Closed source, per-user pricing, and a
SaaS control plane some regulated buyers can't accept — which is exactly the
gap a self-hosted AGPL proxy can fill.

### Border0

Protocol-aware Layer-7 proxy tied to SSO identity, positioned as
next-generation PAM (databases, SSH, K8s, RDP, internal web apps), with a
Tailscale partnership for network-layer reach. Database support centers on
PostgreSQL and MySQL.

*vs DBBat:* overlapping pitch, narrower database matrix, and the value
concentrates in identity federation and network integration rather than query
governance.

## Open-source competitors

### Bytebase

The largest open-source project in the space (~13k GitHub stars). A governed
**web SQL editor** plus schema change management: users query through
Bytebase's UI, which enforces JIT time-boxed access, dynamic data masking,
approval flows, and audit logging. Critically, all four of those are
**Enterprise-plan features** — the Community edition is schema change
management.

*vs DBBat:* competes on the use case, not the architecture. No wire protocol
means no native clients, no ORMs, no BI tools, no AI agents speaking the
protocol — everything funnels through their editor. But their community reach
and category marketing ("database DevSecOps") shape how buyers frame the
problem.

### JumpServer

Popular open-source PAM (GPLv3 + commercial editions): credential vaulting,
web-based terminal/DB sessions, video-style session recording, ticket-based
JIT access. Database access is one asset type among SSH/RDP/K8s.

*vs DBBat:* generalist PAM with session-replay-style audit rather than
structured per-query logs; database governance is not the center of gravity.

### AccessFlow

Apache-2.0, Java/Spring + React. On paper the most feature-complete OSS
competitor: multi-stage approval workflows with AI risk scoring, HMAC-chained
tamper-evident audit log, column masking, row-level security, JIT with
auto-revocation, break-glass access, and a very wide engine list (PostgreSQL,
MySQL, Oracle, SQL Server, MongoDB, ClickHouse, Snowflake, BigQuery, plus
NoSQL and API targets). The model is different, though: queries are
**submitted through its UI, reviewed, then executed server-side** — it holds
the credentials and runs the query for you. Extremely early stage (single-digit
stars).

*vs DBBat:* a feature-list rival but not an architecture rival — no transparent
proxy, so no existing tools or drivers work through it. Worth watching as the
clearest sign others see the same market.

### Smaller / adjacent OSS

- **[GatewayD](https://github.com/gatewayd-io/gatewayd)** — plugin-based
  cloud-native database gateway framework, PostgreSQL-focused; a toolkit for
  building something like DBBat rather than a product.
- **[database-gateway](https://github.com/kazhuravlev/database-gateway)** —
  small OPA-policy gateway for PostgreSQL with a web UI.
- **Inspektor** — early OSS access-control proxy; effectively dormant.

## Adjacent players (same budget line, different product)

- **Satori, Formal, Cyral** — commercial "data security platform" proxies:
  classification, masking, and policy enforcement in front of databases and
  warehouses, sold to security/data-governance teams rather than engineering.
  Cyral has shown little public activity recently.
- **JIT access managers (Apono, Entitle, Opal, Serval)** — orchestrate grants
  in the databases' own permission systems via APIs; no proxy, no query log.
  They solve "who may connect," not "what happened on the wire."
- **Connection poolers (ProxySQL, PgBouncer, Supavisor)** — proxies for
  performance, not governance; occasionally confused with this category.

## Where DBBat is differentiated

1. **Oracle, self-hosted, in the proxy itself.** A hand-rolled TNS/TTC
   implementation with O5LOGON proxy auth. Teleport and StrongDM have Oracle;
   almost nothing open-source and self-hostable does, and hoop.dev doesn't
   list it. For Oracle-heavy estates this is close to a category of one.
2. **Approval holds on a live connection.** A matching statement is suspended
   *mid-flight* — the client just sees a slow query — until a second human
   approves in the UI or Slack. Platform competitors gate the *session* or the
   *access request*; only hoop.dev and AccessFlow gate the statement, and
   AccessFlow does it by owning query execution.
3. **Open capture format.** Sessions dump to protocol-agnostic pcapng that
   tcpdump and Wireshark read natively, with an anonymisation tool. Every
   competitor with session recording uses a proprietary replay format.
4. **Operational footprint.** One Go binary plus one PostgreSQL. No
   certificate authority, no per-node agents, no SaaS control plane, no
   per-user pricing.
5. **Transparent to existing tooling.** Native clients, ORMs, BI tools, and
   AI agents connect with unchanged drivers across all five protocols.

## Honest gaps (what competitors have that DBBat lacks)

- **Data masking / response inspection** — hoop.dev masks PII in responses at
  the wire; Bytebase and AccessFlow do column-level masking. DBBat logs and
  gates queries but does not rewrite results.
- **Breadth beyond databases** — no SSH/K8s/RDP story; buyers consolidating on
  one access platform will lean Teleport/StrongDM.
- **Enterprise identity depth** — SSO exists (Slack OAuth), but no
  SCIM/directory sync or certificate-based auth.
- **Compliance packaging** — incumbents lead with SOC 2/PCI/HIPAA mappings;
  DBBat has the audit substance but not the paperwork.
- **Community scale** — Bytebase's ~13k stars vs DBBat's early-stage reach.

## Watch list

- **hoop.dev** — feature releases and their AI-agent narrative; the direct rival.
- **Teleport** — MCP/AI database access maturing into statement-level control.
- **Bytebase** — any move from "governed editor" toward a wire-protocol proxy.
- **AccessFlow** — whether it gains traction; validates the same thesis.
- **AI-agent DB access generally** — every player is converging on "govern the
  agent's database access"; DBBat's transparent-proxy + approval-hold design is
  naturally suited to it and the positioning window is open now.
