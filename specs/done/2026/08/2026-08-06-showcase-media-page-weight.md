# Trim the homepage's showcase payload

## Goal

Get dbbat.com's landing page under ~600 KB of media on first view, without
losing the showcase visuals it just gained.

## Why

`website/src/components/ProductShowcase/` (added 2026-08-06) puts the generated
media on the homepage. It is honest, reproducible artwork — and it is heavy:

| Asset | Bytes |
|---|---|
| `query-list.png` | 320 KB |
| `query-results.png` | 286 KB |
| `add-server.png` | 227 KB |
| `grant-request.png` | 200 KB |
| `approval-hold-poster.png` | 312 KB |
| `approval-hold-av1.mp4` | 337 KB |

The four stills are `loading="lazy"`, so they cost nothing above the fold. The
poster and the clip do not: the poster is a `poster=` attribute (always
fetched) and the AV1 rendition starts downloading the moment autoplay begins,
which is right after hydration. That is ~650 KB before a visitor has scrolled,
for a page whose first screen is a logo and three buttons.

Nothing here is urgent — it is a marketing page, not an app shell — but it is
the kind of thing that only ever gets worse.

## Implementation

- **WebP/AVIF alongside the PNGs.** `scripts/showcase.sh` already has ffmpeg;
  emit `*.webp` next to each still (and the poster) in the same step that
  transcodes the video, and serve them from a `<picture>` with the PNG as the
  `<img>` fallback. Expect 60-80% off the stills. Keep the PNGs: they are what
  the docs and any external embed link to.
- **Don't start the clip until it is on screen.** Wrap the `play()` call in
  `ProductShowcase` in an `IntersectionObserver`, and drop `preload="metadata"`
  to `preload="none"` when the poster is doing the work anyway. This is the
  bigger win and needs no new assets.
- **Consider halving the stills.** They are 2560x1600 (deviceScaleFactor 2) and
  render at ~350 CSS px wide in the grid — a 1280-wide rendition would still be
  ~2x on that layout. The full-size PNG stays behind the click-through link.

Measure before and after with `bun run build && du -sh website/build/img`, and
check the network panel on a cold load rather than trusting the file sizes.

No GitHub issue yet — one should be filed.
