---
model: sonnet
effort: low
---

# `should default start time to approximately current time` compares a local date to a UTC one

## Goal

Fix `front/e2e/grants.spec.ts`'s "should default start time to approximately current time" test
so it stops failing in the roughly two-hour window each night where the local calendar date and
the UTC calendar date disagree (any timezone ahead of UTC, e.g. CEST/UTC+2 between local midnight
and 22:00 UTC).

## Why

Found on 2026-09-23 while running the full E2E suite as the batch gate for a `/implement-todos`
run (Oracle wire-protocol specs, unrelated to this file or to grant creation). Failure:

```
Expected: "2026-09-22"
Received: "2026-09-23"
```

at `grants.spec.ts:116`. `input[type="datetime-local"]#startsAt`'s value is always the browser's
**local** date/time per the HTML spec — the app default is correct behavior. The test instead
computes its expectation with `new Date().toISOString().split("T")[0]`, which is the **UTC**
date. At the moment of the failure, local time was `2026-09-23 01:25 CEST` while UTC was still
`2026-09-22 23:25` — so the input correctly showed `2026-09-23` and the test's UTC-derived
`today` was `2026-09-22`.

Confirmed pre-existing and unrelated to the Oracle batch: no commit in that batch (or anywhere on
`main`) touches `front/e2e/grants.spec.ts` or the grants create-dialog's date-default logic
(`git log --oneline main..HEAD -- 'front/src/routes/_authenticated/grants/**'` returns nothing).
This is a latent test bug that has presumably been failing intermittently, silently, in this
same nightly window since it was written.

## Implementation

- In `front/e2e/grants.spec.ts`, compute the expectation from local date components instead of
  `toISOString()`, e.g.:
  ```ts
  const now = new Date();
  const today = `${now.getFullYear()}-${String(now.getMonth() + 1).padStart(2, "0")}-${String(now.getDate()).padStart(2, "0")}`;
  ```
  or equivalent (a small local-date-formatting helper, if the test file already has one to reuse).
- Re-run the test to confirm it passes regardless of the local/UTC date relationship — ideally
  verify by temporarily forcing the assertion to compare against the *other* of the two dates and
  confirming it fails, so the fix is proven rather than assumed.
- No application code change is expected — the datetime-local input's local-time default is
  correct; only the test's expectation is wrong.
