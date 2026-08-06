---
model: sonnet
effort: low
---

# Standardize the client-side env vars for DBBat (`DBBAT_API_KEY`, `DBBAT_USER`, `DBBAT_URL`)

No GitHub issue yet — one should be filed.

## Problem

Every script, curl example, and CI job that talks to dbbat needs the API key
from *somewhere*, and today there is no blessed name for it. The website docs
use an anonymous `$TOKEN` in every curl example (e.g.
`website/docs/configuration/servers.md`, `website/docs/features/query-logging.md`),
the app never suggests a name when it hands you a freshly created key
(`front/src/routes/_authenticated/api-keys/index.tsx`), and existing ad-hoc
scripts have each invented their own (`DBBAT_KEY`, `DBBAT_ADMIN_KEY`, …).
Without a documented convention, no two teams' tooling composes.

## Proposal

Adopt three canonical client-side variables:

| Variable | Meaning | Needed for |
|----------|---------|-----------|
| `DBBAT_API_KEY` | The `dbb_…` API key | REST calls (Bearer token) and SQL connections (as the password) |
| `DBBAT_USER` | The dbbat username the key belongs to | SQL connections through the proxy (the key is only valid for its owner) |
| `DBBAT_URL` | API/UI base URL, e.g. `https://dbbat.example.com` | REST calls and deep links |

Why this name and not the alternatives:

- **Not `DBB_*`**: the `DBB_` prefix is the *server's* config namespace
  (`DBB_DSN`, `DBB_KEY`, …). A client-side var living there would look like
  something the dbbat process reads — and `DBB_KEY` (the AES encryption key)
  vs `DBB_API_KEY` would be a genuinely dangerous near-collision. Keeping
  client vars under `DBBAT_*` makes the two namespaces visually distinct.
- **Full product name**: `DBBAT` is greppable and unambiguous in a user's
  environment, where `DBB` means nothing.
- **`_API_KEY` suffix**: matches the dominant ecosystem convention
  (`GITHUB_TOKEN`, `OPENAI_API_KEY`, `STRIPE_API_KEY`) — people guess it
  right on the first try.

Documentation sweep (all three places the task mentions):

1. **App**: when a key is created in
   `front/src/routes/_authenticated/api-keys/index.tsx`, show a copyable
   snippet alongside the raw key — all three variables, with the username
   and URL pre-filled from the session:

   ```bash
   export DBBAT_URL=https://dbbat.example.com
   export DBBAT_USER=jane.doe
   export DBBAT_API_KEY=dbb_…
   ```
2. **Repo docs**: document the three variables in `docs/api.md` next to the
   existing key-prefix table.
3. **Website**: replace `$TOKEN` with `$DBBAT_API_KEY` across
   `website/docs/**` curl examples, and add a short "Environment variables
   for clients" note in the API/quickstart page defining all three —
   including a psql example
   (`psql "host=… user=$DBBAT_USER password=$DBBAT_API_KEY …"`).
4. **README**: one line in the API section of the top-level `README.md`.
