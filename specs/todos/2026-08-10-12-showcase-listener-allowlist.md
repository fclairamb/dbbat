# Showcase runner: disable proxy listeners from one list, not one by one

No GitHub issue yet — file one when picking this up.

## Goal

Make `scripts/showcase.sh` start its throwaway dbbat with **every** proxy
listener off except the one it actually dials, driven from a single place, so
adding a sixth protocol to dbbat cannot silently break the showcase again.

## Why

The script sets `DBB_LISTEN_ORA=""`, `DBB_LISTEN_MYSQL=""` and
`DBB_LISTEN_MONGO=""` by hand. `DBB_LISTEN_MSSQL` was added to dbbat afterwards
and nobody came back, so the showcase instance bound `:1434` — and died at
startup on any machine where something else already had it, which on a
developer's laptop means their own `make dev` stack. Fixed in
`fix(showcase): disable the SQL Server listener on the throwaway instance`, but
the shape of the bug survives: the list is a denylist maintained by memory, and
the next protocol will reproduce it.

The failure mode is bad out of proportion to its cause: `dbbat exited during
startup` with a bind error buried in `${SHOWCASE_WORK}/dbbat.log`, and no hint
that the collision is with a listener the run never needed.

## Implementation

`scripts/showcase.sh`, around the `--- demo-mode dbbat ---` block.

- Keep one array of the listener variables the showcase *uses*
  (`DBB_LISTEN_PG`, `DBB_LISTEN_API`) and derive the rest by blanking every
  `DBB_LISTEN_*` dbbat knows about. The candidate list can be read off
  `internal/config` rather than retyped — a `grep -o 'DBB_LISTEN_[A-Z]*'` over
  the config package at run time is enough, and it fails loudly if the naming
  convention ever changes.
- Alternatively (simpler, arguably better): teach dbbat itself a
  `DBB_LISTEN_NONE=1`-style switch, or accept that the demo run mode should
  default every proxy listener except PostgreSQL to empty. That moves the
  knowledge into the process that owns it instead of into a shell script, and
  `DBB_RUN_MODE=demo` already carries other opinions of this kind.
- Either way, keep the preflight useful: when the instance dies during startup,
  surface the offending line from `${SHOWCASE_WORK}/dbbat.log` rather than the
  last 30 lines, when it is a `bind: address already in use`.

## Key files

- `scripts/showcase.sh` — the listener block and the startup wait
- `internal/config/` — where the `DBB_LISTEN_*` set is defined
- `front/showcase/README.md` — the isolation section, if the port story changes

## Resolved open questions

> Alternatively (simpler, arguably better): teach dbbat itself a
> `DBB_LISTEN_NONE=1`-style switch, or accept that the demo run mode should
> default every proxy listener except PostgreSQL to empty.

**Decision: keep it in the script — the derived allowlist.** Do not add a
`DBB_LISTEN_NONE` switch and do not change what `DBB_RUN_MODE=demo` does.
Changing demo-mode listener defaults alters behaviour for every demo
deployment, which is a far larger blast radius than this bug (a throwaway
showcase instance binding a port it never uses) justifies.

Implement the first option: in `scripts/showcase.sh`, keep one array naming the
listener variables the showcase actually **uses** (`DBB_LISTEN_PG`,
`DBB_LISTEN_API`), discover the full candidate set at run time by grepping
`DBB_LISTEN_[A-Z]*` out of `internal/config/`, and export every candidate not
in the used-set as empty. The point is that a sixth protocol is disabled
automatically, with no denylist to maintain.

Make the discovery fail loudly rather than silently degrading: if the grep
returns nothing, or does not contain the two variables the showcase itself
needs, abort with a message saying the `DBB_LISTEN_*` naming convention changed
— a silent empty candidate list would reintroduce exactly this bug.

> Either way, keep the preflight useful.

**In scope.** When the instance dies during startup and the log contains
`bind: address already in use`, print that line (and the variable/port it
corresponds to, if derivable) instead of the last 30 lines of
`${SHOWCASE_WORK}/dbbat.log`. Fall back to the existing tail for every other
startup failure.
