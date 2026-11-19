---
model: opus
effort: low
---

# Compose install docs fall back to a published encryption key

## Problem

Found by the security scan of 2026-10-30 (finding L8).

Severity: Low. Affects operators who copy the documented compose file.

The Docker Compose page, which the README links as "a complete example", sets `DBB_KEY: ${DBB_KEY:-<constant>}` (`website/docs/installation/docker-compose.md:31`). With `DBB_KEY` unset, dbbat silently runs with a key published on the website. That key encrypts every stored upstream credential and seeds the audit chain's HMAC key. Anyone holding a copy of the store, such as a backup, can then decrypt the credentials and re-seal forged audit records. The page says it is for development and testing, but nothing warns that the fallback key must not be kept.

## Proposal

1. Use `${DBB_KEY:?set DBB_KEY, e.g. openssl rand -base64 32}` so compose refuses to start without a key, or drop the variable and let dbbat create `~/.dbbat/key` in a volume.
2. Add a short warning about generating a key and backing it up.

## Tests

- Docs only. Check the page renders (`website` build) and that `docker compose config` fails without `DBB_KEY` set.
