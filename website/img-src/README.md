# Logo originals

The pristine, full-resolution logo exports. **Not** deployed: this directory sits
outside `static/`, so Docusaurus never copies it into `build/`.

`static/img/logo-text.png` and `static/img/logo-notext.png` used to *be* these
files — 853 KB and 594 KB of 761x761 RGBA — served as-is above the fold on every
page of dbbat.com. They are now shipped as renditions sized for how they actually
render. These are hand-made source images, not build artifacts, so the originals
live here (and, of course, in the git history predating this directory).
`front/public/` used to carry byte-identical copies for the app UI; those are now
renditions too, regenerated from here (see below).

## Regenerating the shipped renditions

Requires ImageMagick (`magick`) and `cwebp` (the `webp` package).

Hero logo — `src/pages/index.tsx`, `max-width: 300px`, so 600px covers DPR 2.
A `<picture>` serves the WebP; the PNG is the fallback:

```sh
magick img-src/logo-text.png -filter Lanczos -resize 600x600 -strip PNG32:/tmp/hero600.png
magick /tmp/hero600.png -dither Riemersma -colors 160 \
  -define png:compression-level=9 PNG8:static/img/logo-text.png
cwebp -quiet -q 80 -m 6 -sharp_yuv -alpha_q 70 /tmp/hero600.png -o static/img/logo-text.webp
```

Navbar logo — `docusaurus.config.ts` `themeConfig.navbar.logo.src`, rendered at
32 CSS px. Kept at 128px so it stays crisp past DPR 2, and quantized. Docusaurus
theme config takes a single `src`, so there is no `<picture>` and no WebP here:

```sh
magick img-src/logo-notext.png -filter Lanczos -resize 128x128 -strip PNG32:/tmp/nav128.png
magick /tmp/nav128.png -dither Riemersma -colors 255 \
  -define png:compression-level=9 PNG8:static/img/logo-notext.png
```

`-alpha_q 70` matters: the logos have a soft, sparkly alpha channel, and WebP
compresses alpha losslessly by default — that alone was ~55 KB of the 600px
encode.

## Regenerating the app UI renditions (`front/public/`)

Same originals, different sizes. Run from the repo root (the commands above are
run from `website/`).

Login logo — `front/src/routes/login.tsx`, `h-32 w-32` = 128 CSS px, so 256px
covers DPR 2. A `<picture>` serves the WebP; the PNG8 is the fallback (here the
WebP is both smaller *and* visibly cleaner than the quantized PNG, so the PNG is
only ever fetched by a browser without WebP support):

```sh
magick website/img-src/logo-text.png -filter Lanczos -resize 256x256 -strip PNG32:/tmp/login256.png
magick /tmp/login256.png -dither Riemersma -colors 160 \
  -define png:compression-level=9 PNG8:front/public/logo-text.png
cwebp -quiet -q 80 -m 6 -sharp_yuv -alpha_q 70 /tmp/login256.png -o front/public/logo-text.webp
```

Sidebar logo — `front/src/components/layout/AppSidebar.tsx`, `h-10 w-10` = 40
CSS px. Kept at 128px for the same reason as the website navbar, and it is the
same recipe; PNG only, since at 6 KB a WebP twin would save ~1 KB and cost a
second file:

```sh
magick website/img-src/logo-notext.png -filter Lanczos -resize 128x128 -strip PNG32:/tmp/nav128.png
magick /tmp/nav128.png -dither Riemersma -colors 255 \
  -define png:compression-level=9 PNG8:front/public/logo-notext.png
```

`front/public/favicon.png` is a separate 32x32 export (3.6 KB) and needs none of
this.
