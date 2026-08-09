# Compliance mapping page (SOC 2 / ISO 27001 / PCI DSS)

## Goal

A website section that maps dbbat's capabilities to the specific controls
auditors ask about — so a security engineer evaluating dbbat can hand their
compliance team a ready-made answer to "how does this help our SOC 2 / ISO
27001 / PCI audit?".

## Why

The competitive landscape doc lists "compliance packaging" as an honest gap:
StrongDM and Teleport lead with SOC 2/HIPAA/PCI mappings, while dbbat has the
audit substance (per-query logs, time-boxed grants, four-eyes approvals,
encrypted credentials) but none of the paperwork. This is pure writing — no
code — and it directly converts existing features into procurement-stage
value. Buyers in regulated industries filter on this before they ever try
the product.

Companion spec: `2026-08-09-tamper-evident-audit-chain.md` — the log-integrity
row of the mapping gets much stronger once that ships, but the page should
not wait for it.

No GitHub issue yet — file one when picking this up.

## Implementation

### Content — capability → control mapping

One Docusaurus page (or a small section) under `website/docs/`, e.g.
`website/docs/compliance.md`, wired into `website/sidebars.ts`. A table per
framework, each row: dbbat capability → control reference → one-sentence
"how it satisfies it". Capabilities to map:

- **Per-query audit log** (all five protocols, attribution to a named user)
  → ISO 27001 A.8.15 (logging), SOC 2 CC7.2, PCI DSS 10.2
- **Time-boxed, scoped grants** (JIT access, auto-expiry, quotas) → ISO
  A.5.15/A.5.18 (access control & rights), SOC 2 CC6.1–CC6.3
- **Approval holds** (four-eyes on matching statements, self-approval
  rejected) → ISO A.5.3 (segregation of duties), SOC 2 CC8.1 (change
  management style controls)
- **Grant-definition immutable versioning + deactivation** → evidence of
  access-review and revocation processes (ISO A.5.18)
- **Credential encryption (AES-256-GCM), Argon2id password hashing, API-key
  encryption** → ISO A.8.24 (cryptography), PCI DSS 8.3
- **Session capture (pcapng) + anonymisation tool** → forensic readiness,
  ISO A.5.28 (evidence collection)
- **Retention controls** (`DBB_QUERY_STORAGE_RETENTION`,
  `DBB_DUMP_RETENTION`) → data-minimisation / log-retention policies
- **Log integrity** — once the HMAC audit chain ships → ISO A.8.15 / PCI DSS
  10.5 (protect logs from modification)

### Tone and honesty

- dbbat itself is not certified against anything — say so plainly. The page
  maps how dbbat helps *the customer's* audit, it does not claim compliance.
  ("dbbat is a control you deploy, not a certificate you inherit.")
- Cite control numbers conservatively and verify each against the current
  framework text before publishing (control renumbering happens; ISO 27001
  references above are the 2022 revision).

### Placement

- Add to the Docusaurus sidebar under its own "Compliance" entry.
- Link from `website/docs/intro.md` and consider a short homepage mention
  (`website/src/pages/index.tsx` is already being reworked in the current
  positioning pass — coordinate with it).
- A condensed version can later become a `docs/` markdown in the repo for
  people evaluating from GitHub.
