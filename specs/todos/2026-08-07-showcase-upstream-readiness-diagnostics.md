---
model: sonnet
effort: low
---

# The showcase's upstream readiness wait fails blind

No GitHub issue filed yet — one should be opened when this is picked up.

## Goal

Make `scripts/showcase.sh` say *why* the throwaway PostgreSQL container never
came up, and stop it from waiting 60 seconds on a container that is already
dead.

## Why

The rot-guard job on [#301](https://github.com/fclairamb/dbbat/pull/301) failed
with exactly this, and nothing else:

```
[showcase] waiting for the upstream to accept connections
[showcase] the upstream container never became ready
```

A rerun of the same commit passed, so it was transient — but the log gives no
way to tell that. There is no `docker logs`, no exit code, no indication of
whether the container crashed, was still booting, or lost its port. Diagnosing
it meant re-running CI and hoping the result differed, which is not a debugging
technique.

Two concrete defects in the wait at
[scripts/showcase.sh:135-143](scripts/showcase.sh):

1. **A dead container is indistinguishable from a slow one.** The container is
   started with `docker run -d --rm`, so if it exits, Docker removes it
   immediately. Every subsequent `docker exec … pg_isready` then fails because
   the container *does not exist* — the same failure the loop uses to mean
   "not ready yet". The script burns the full 60 iterations on a container that
   has been gone since second two.
2. **`die` prints no diagnostics.** By the time it fires, `--rm` has usually
   destroyed the only evidence, so even a manual `docker logs` after the fact
   returns nothing.

The same shape applies to the second loop immediately below it (the one waiting
for `init.sql` to create the `demo` database), which does not even check its own
result — it just falls through after 60s and lets the failure surface later, as
something more confusing.

## Implementation

In `scripts/showcase.sh`:

- **Detect an exited container inside the loop.** Check
  `docker inspect -f '{{.State.Status}}' "${SHOWCASE_CONTAINER}"` (or the
  container's absence) each iteration and break out immediately when it is
  `exited` or gone, instead of running out the clock.
- **Capture the logs before they vanish.** Drop `--rm` from the `docker run` at
  [:125](scripts/showcase.sh) so a crashed container survives for inspection —
  the existing named cleanup at [:72](scripts/showcase.sh) already removes it on
  exit, and `docker start` on the reuse path at [:122](scripts/showcase.sh)
  already assumes a stopped container can exist. Then have `die` dump
  `docker logs --tail 50 "${SHOWCASE_CONTAINER}"` and the container's exit code.
- **Give the `demo`-database loop a real failure.** It currently falls through
  silently after 60s; make it `die` with the output of
  `docker exec … psql -U postgres -lqt`, so a broken `init.sql` is named at the
  point it breaks rather than surfacing as a dbbat startup error later.
- Keep the isolation rules at the top of the file intact: still no
  `docker compose`, still only ever touching the container this script created,
  by name.

Optional, if it is cheap: raise the readiness budget above 60s, or make it a
knob (`SHOWCASE_PG_TIMEOUT`). A GitHub runner that has just pulled `postgres:15`
over a cold network is the slow case, and this loop is the only thing standing
between that and a red job.
