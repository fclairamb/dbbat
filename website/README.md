# DBBat Website

This website is built using [Docusaurus](https://docusaurus.io/), a modern static website generator.

## Installation

```bash
bun install
```

## Local Development

```bash
bun run start
```

This command starts a local development server and opens up a browser window. Most changes are reflected live without having to restart the server.

## Build

```bash
bun run build
```

This command generates static content into the `build` directory and can be served using any static contents hosting service.

## Deployment

The site is automatically deployed to GitHub Pages when changes are pushed to the `main` branch via GitHub Actions.

Manual deployment:

```bash
bun run deploy
```

## Makefile Commands

```bash
make dev          # Start dev server
make build        # Build for production
make serve        # Serve built site locally
make install      # Install dependencies
make clean        # Remove build artifacts
make typecheck    # Run TypeScript type checking
make rebuild      # Clean + build
make deploy       # Deploy to GitHub Pages (manual)
```

## Showcase media

`static/img/showcase/` holds product screenshots and the approval-hold video,
generated on demand from a live demo-mode dbbat instance. Do not hand-edit
them — regenerate with `make showcase` from the repo root (or run the
`Showcase media` GitHub Action manually). See `front/showcase/README.md`.

Regeneration is deliberately **not** wired into a release, so the media can
drift behind the product. `static/img/showcase/manifest.json` is how you tell:

```json
{ "version": "0.22.0", "commit": "…", "generatedAt": "…" }
```

`version` comes from `.release-please-manifest.json` — the same source the
binary's `internal/version` is stamped from. Pages that embed the media should
read the manifest and caption it ("captured on v0.22.0") rather than implying
the visuals are current. `src/components/ProductShowcase/` does exactly that:
it `import`s the manifest, so the caption is baked in at build time.

The video ships in two renditions: `approval-hold-av1.mp4` (small, modern) and
`approval-hold-h264.mp4`. A `<video>` element needs **both** `<source>`s —
Safari only hardware-decodes AV1 on recent silicon. `approval-hold-poster.png`
is the still that goes with them.

### Autoplay and `prefers-reduced-motion`

A CSS media query cannot stop a video from autoplaying, so `ProductShowcase`
never renders an `autoplay` attribute at all. The prerendered HTML is the
still, paused, with player controls; an effect calls `play()` after hydration
only when `matchMedia("(prefers-reduced-motion: reduce)")` does *not* match.
That is also why the check lives in an effect rather than at render time —
Docusaurus prerenders these pages in Node, where `window` does not exist.

### This is the only screenshot set

`static/img/screenshots/` used to hold a second, hand-captured set. It was
deleted in favour of this one: it could not be regenerated, it carried no
provenance, and five of its eight files were referenced by nothing. Put new
product visuals in the showcase suite (`front/showcase/`) so they stay
reproducible — do not start a third directory.
