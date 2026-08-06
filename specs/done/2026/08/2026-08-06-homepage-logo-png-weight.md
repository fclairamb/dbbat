# The homepage logos are 1.4 MB of PNG, above the fold

## Goal

Get `website/static/img/logo-text.png` and `logo-notext.png` down to a few tens
of kilobytes each, without changing how they look.

## Why

Found while trimming the showcase payload (the
`2026-08-06-showcase-media-page-weight` spec). Once the
showcase stopped loading eagerly, the two logos became by far the heaviest
thing on a cold view of dbbat.com — and unlike the showcase they are *above*
the fold, so every visitor pays for them before anything else renders:

| Asset | Bytes | Intrinsic | Rendered |
|---|---|---|---|
| `logo-text.png` | 853 KB | 761x761 | the hero logo, ~200 CSS px tall |
| `logo-notext.png` | 594 KB | 761x761 | the navbar logo, 32 CSS px |

594 KB for a 32px navbar icon is the striking one. Measured with the built
site (`cd website && bun run build && bun run serve`), reading
`performance.getEntriesByType("resource")` on a cold load.

These are almost certainly uncompressed 32-bit RGBA exports. A quick check
before doing anything else: `magick identify -verbose` on both to confirm the
bit depth, and whether the alpha channel is actually used.

## Implementation

- `pngquant`/`oxipng` on the PNGs would already take a large bite; a WebP
  sibling served from a `<picture>` would take more. The pattern is already in
  the tree — see `ProductShowcase/index.tsx` for the `<picture>` + PNG-fallback
  shape, and the `webp_rendition` step in `scripts/showcase.sh` for the
  encoding one.
- The navbar logo is a Docusaurus theme config value
  (`docusaurus.config.ts` → `themeConfig.navbar.logo.src`), not our JSX, so it
  cannot take a `<picture>`. Either shrink the PNG in place, or point it at a
  properly sized rendition (`logo-notext-64.png` / `.webp`) — 32 CSS px at
  DPR 2 wants 64px, not 761.
- The hero logo in `website/src/pages/index.tsx` is our own `<img>`, so it can
  take the same `<picture>` treatment as the showcase stills.
- Unlike the showcase media, these are not generated artifacts — they are
  hand-committed source images, so whatever is done here has to be done to the
  files themselves, not to a pipeline. Keep an unoptimised original somewhere
  if the source is not elsewhere.

No GitHub issue yet — one should be filed.
