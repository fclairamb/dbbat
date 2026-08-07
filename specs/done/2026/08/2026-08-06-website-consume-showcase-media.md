# Put the showcase media on the website

## Goal

Actually use `website/static/img/showcase/` on dbbat.com: the four screenshots
and the approval-hold video, captioned with the version they were captured
from, read out of `manifest.json`.

## Why

`make showcase` and the `Showcase media` workflow now generate the assets (see
`specs/todos/2026-08-05-website-showcase-media.md`, implemented), but nothing
on the site references them — the homepage and docs still show the older
`static/img/screenshots/*.png`. The pipeline is only worth its upkeep if the
output is on a page someone reads.

The manifest caption matters because regeneration is deliberately on demand: a
visitor (and a maintainer) should be able to see that the visuals were captured
from v0.22.0 rather than assume they are current.

## Implementation

- `website/src/pages/index.tsx` (or the components under `website/src/`): add a
  product-visuals section pulling from `/img/showcase/`.
- The video needs **both** sources, in this order — Safari only
  hardware-decodes AV1 on recent silicon:

  ```html
  <video autoplay muted loop playsinline>
    <source src="/img/showcase/approval-hold-av1.mp4" type="video/mp4; codecs=av01.0.05M.08" />
    <source src="/img/showcase/approval-hold-h264.mp4" type="video/mp4; codecs=avc1.4d002a" />
  </video>
  ```

  Respect `prefers-reduced-motion`: fall back to a poster frame rather than
  autoplaying.
- `manifest.json` is a static asset; import it directly
  (`import manifest from "@site/static/img/showcase/manifest.json"`) so the
  caption is baked at build time, and render something like
  "captured on v{version} · {generatedAt}".
- Decide what happens to `website/static/img/screenshots/*.png` — either
  migrate the pages that use them onto the showcase set and delete them, or
  document why both sets exist. Two competing screenshot directories will rot.
- Sizes: the stills are 2560x1600 PNGs (~200-350 KB each). Serve them at 1x CSS
  width with `max-width: 100%`, and consider generating WebP alongside if the
  homepage weight becomes a problem.

No GitHub issue yet — one should be filed.
