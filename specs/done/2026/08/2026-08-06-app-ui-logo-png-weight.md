# The app UI still ships the 1.4 MB logo PNGs

## Goal

Give `front/public/logo-text.png` (853 KB) and `front/public/logo-notext.png`
(594 KB) the same treatment the website's copies just got: renditions sized for
how they actually render, instead of 761x761 RGBA.

## Why

Found while doing `2026-08-06-homepage-logo-png-weight.md` for the website. The
two files under `front/public/` are **byte-identical** to the ones the website
used to serve (`md5` matches), and they are just as oversized for their use:

| Asset | Bytes | Intrinsic | Rendered |
|---|---|---|---|
| `front/public/logo-text.png` | 853 KB | 761x761 | login card, `h-32 w-32` = 128 CSS px (`front/src/routes/login.tsx`) |
| `front/public/logo-notext.png` | 594 KB | 761x761 | sidebar, `h-10 w-10` = 40 CSS px (`front/src/components/layout/AppSidebar.tsx`) |

853 KB is the first thing an unauthenticated visitor to a dbbat instance
downloads, for a 128px box. The website work cut the equivalent pair from
1.41 MB to 70 KB on a cold homepage view; the app UI is still paying full price.

## Implementation

- The recipe and the encoder invocations are already written down in
  `website/img-src/README.md`, which also holds the pristine 761px originals —
  reuse both rather than re-deriving them.
- Login logo renders at 128 CSS px, so 256px covers DPR 2. Sidebar logo renders
  at 40 CSS px, so 128px is generous (and is exactly what the website's navbar
  rendition already is — `website/static/img/logo-notext.png`, 6 KB, could be
  copied verbatim).
- Unlike Docusaurus, the app is our own JSX end to end, so both `<img>` tags can
  take a `<picture>` + WebP source if the PNG alone does not get small enough.
  Watch the login page's e2e selectors (`data-testid="login-logo"`) — a
  `<picture>` wrapper keeps the `<img>`, so they should survive, but
  `front/e2e/` is the gate.
- `front/public/favicon.png` is a separate file and is not in scope here; check
  its weight while you are there.

No GitHub issue yet — one should be filed.
