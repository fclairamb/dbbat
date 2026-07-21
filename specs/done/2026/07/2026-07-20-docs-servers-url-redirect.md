# Rename the docs "databases" page to "servers" with a redirect

## Goal

Move `website/docs/configuration/databases.md` to `configuration/servers.md` so the published URL becomes `/docs/configuration/servers`, and add a client-side redirect from the old `/docs/configuration/databases` path so existing inbound links keep working.

## Why

v0.17.0 renamed the `databases` resource to `servers` throughout the API, store and UI (`/api/v1/databases` → `/api/v1/servers`, no alias). The docs page covering it was updated in content and retitled via frontmatter (`title: Server Configuration`), but the **filename and slug were deliberately left alone** to avoid breaking:

- the published URL `/docs/configuration/databases`, which is linked externally and from the README, and
- the inbound link at `website/docs/installation/kubernetes.md`.

So the page is now titled "Server Configuration" while living at a `/databases` URL — an inconsistency that will get more confusing as "databases" fades from the vocabulary. Fixing it properly needs a redirect, which needs a plugin the site does not currently install.

## Implementation

1. Add the redirect plugin:

   ```bash
   cd website && bun add @docusaurus/plugin-client-redirects
   ```

2. Register it in `website/docusaurus.config.ts` under `plugins`:

   ```ts
   [
     "@docusaurus/plugin-client-redirects",
     {
       redirects: [
         { from: "/docs/configuration/databases", to: "/docs/configuration/servers" },
       ],
     },
   ],
   ```

3. `git mv website/docs/configuration/databases.md website/docs/configuration/servers.md` and drop the now-redundant `title:` frontmatter override (the filename carries it).

4. Update inbound links:
   - `website/docs/installation/kubernetes.md` — "Configure servers" link target
   - `website/docs/features/ssh-tunnels.md` — "See Also" link
   - `website/docs/features/supported-databases.md` — check for references
   - `README.md` — check the documentation link list

5. `cd website && bun run build` must pass. `onBrokenLinks` is `"throw"`, so any missed link fails the build — that is the check.

Note that the plugin emits **client-side** redirects (a meta-refresh HTML page per old path). That is fine for docs but does not produce a 301, so it is invisible to search engines' link-equity transfer. If SEO on that URL matters, do it at the CDN/host layer instead.

## Related

- Also worth folding in while touching the config: `siteConfig.onBrokenMarkdownLinks` is deprecated and should move to `siteConfig.markdown.hooks.onBrokenMarkdownLinks` before Docusaurus v4. The build currently warns about it on every run.

No GitHub issue exists for this yet — one should be filed if it is not picked up soon.

## Implementation Plan

1. Add `@docusaurus/plugin-client-redirects` (version-matched to `@docusaurus/core` 3.10.2) to `website/package.json` and refresh `website/bun.lock` via `bun install`.
2. Register the plugin in `website/docusaurus.config.ts` with a `redirects` entry mapping `/docs/configuration/databases` → `/docs/configuration/servers`. Also migrate the deprecated `onBrokenMarkdownLinks` to `markdown.hooks.onBrokenMarkdownLinks` while in the file (spec "Related").
3. `git mv website/docs/configuration/databases.md website/docs/configuration/servers.md` and drop the now-redundant `title: Server Configuration` frontmatter (the H1/filename carries it).
4. Update inbound absolute links to `/docs/configuration/servers`:
   - `website/docs/features/ssh-tunnels.md:121`
   - `website/docs/installation/kubernetes.md:34,405,562`
   - `website/docs/installation/docker-compose.md:119`
   - `README.md` — verified: no link to the docs URL, nothing to change.
5. QA: `cd website && bun run build` must pass with `onBrokenLinks: "throw"`; verify `website/build/docs/configuration/servers/index.html` exists and a redirect artifact for `docs/configuration/databases` is emitted.
