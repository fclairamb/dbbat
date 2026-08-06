# Retire (or repoint) `scripts/capture-screenshots.ts`

## Goal

Delete `scripts/capture-screenshots.ts`, or repoint it at the showcase pipeline,
so nothing can recreate the `website/static/img/screenshots/` directory that was
just deleted.

## Why

Found by the completeness audit of
`specs/done/2026/08/2026-08-06-website-consume-showcase-media.md`, which deleted
`website/static/img/screenshots/` (all eight PNGs) in favour of the versioned,
regenerable `website/static/img/showcase/` set.

`scripts/capture-screenshots.ts:19` still hardcodes

```ts
const SCREENSHOTS_DIR = ".../website/static/img/screenshots";
```

so running it would recreate exactly the second, unversioned screenshot
directory that spec set out to eliminate — and nothing on the site would
reference the output. The script is currently dead: it is not wired into
`package.json`, the `Makefile`, or any CI workflow, so this is rot rather than a
live bug. But a dead script that resurrects a deleted convention is precisely
how two competing screenshot directories come back.

## Implementation

- Decide between the two options, and say which in the commit:
  - **Delete it.** `front/showcase/screenshots.spec.ts` already captures the
    four marketing stills deterministically (frozen clock, fixed viewport,
    `deviceScaleFactor: 2`) and stamps them with a manifest. If
    `capture-screenshots.ts` covers no scenario the showcase runner misses, it
    is strictly superseded.
  - **Repoint it.** If it captures pages the showcase set does not (the audit
    noted the deleted set had eight images to the showcase's four — dashboard,
    login, users, databases, connections, audit), fold those scenarios into
    `front/showcase/screenshots.spec.ts` instead of keeping a second capture
    path, then delete the script.
- Before deleting, diff the two capture inventories so a genuinely useful
  scenario is not lost — the deleted PNGs are recoverable from git history
  (`git show 0e381a3:website/static/img/screenshots/...`) if you need to look.
- Grep for `capture-screenshots` once more afterwards to confirm nothing invokes
  it.

No GitHub issue yet — one should be filed.
