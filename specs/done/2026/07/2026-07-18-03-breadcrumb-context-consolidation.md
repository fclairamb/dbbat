---
model: sonnet
effort: medium
---

# BreadcrumbContext has grown two parallel side channels for a single consumer page

## Problem

[front/src/contexts/BreadcrumbContext.tsx](../../front/src/contexts/BreadcrumbContext.tsx)
now maintains two independent pathname-keyed maps — `titles` (title override)
and `items` (extra parent crumbs) — each with its own setter, its own
value-equality guard (`itemsEqual`), and its own publishing hook. The second
channel also needed a `JSON.stringify(items)` dependency with an
`eslint-disable` for `react-hooks/exhaustive-deps`
([BreadcrumbContext.tsx:148](../../front/src/contexts/BreadcrumbContext.tsx)).

All of this serves exactly one consumer (`queries/$uid.tsx`, which calls both
hooks for the same pathname). The next breadcrumb need (icons, href override,
a second detail page) would likely add a third channel. Meanwhile
[front/src/components/layout/Header.tsx:14](../../front/src/components/layout/Header.tsx)
declares a local `interface BreadcrumbItem` that shares its name with the
`BreadcrumbItem` UI component imported four lines above — confusing shadowing.

## Proposal

Pure refactor, no behavior change:

1. Merge the two maps into one pathname-keyed entry:

   ```ts
   interface BreadcrumbEntry {
     title?: string;              // leaf-crumb title override
     parents?: BreadcrumbExtraItem[]; // crumbs inserted before the leaf
   }
   ```

   One setter (`setBreadcrumb(pathname, entry | undefined)`), one
   value-equality compare, one publishing hook
   (`useBreadcrumb(pathname, entry)`). Keep thin
   `useBreadcrumbTitle` / `useBreadcrumbItems` wrappers if churn in callers
   should be avoided, or update the single caller in `queries/$uid.tsx`.

2. Replace the `JSON.stringify` effect dependency with a proper value
   comparison — e.g. keep the last-published entry in a `useRef` and compare
   inside the effect before calling the setter — so the eslint-disable goes
   away without reintroducing publish loops. Note the setter already
   deduplicates by value, so the ref comparison mainly avoids redundant
   effect work; verify no update loop with React StrictMode double-invoking
   effects.

3. Rename Header's local `BreadcrumbItem` interface to `Crumb` (it is private
   to the file).

4. Existing behavior is covered by the breadcrumb E2E assertions added with
   the connection-crumb work — they must pass unchanged.

Deliberately out of scope: moving crumbs into TanStack Router loader data /
route context. That is the idiomatic long-term shape but only pays off if more
detail pages appear, and it would force data fetching into route loaders;
revisit if [[2026-07-18-02-connection-detail-page]] multiplies detail routes.

## Implementation Plan

Consumers today: `connections/$uid.tsx` uses `useBreadcrumbTitle` only;
`queries/$uid.tsx` uses both `useBreadcrumbTitle` and `useBreadcrumbItems` for
the same pathname; `Header.tsx` reads `titles`/`items` from the context and
has its own locally-shadowing `BreadcrumbItem` interface.

1. **`front/src/contexts/BreadcrumbContext.tsx`**
   - Replace the two `Record<string, ...>` state maps with a single
     `Record<string, BreadcrumbEntry>` (`BreadcrumbEntry = { title?: string;
     parents?: BreadcrumbExtraItem[] }`).
   - One `setBreadcrumb(pathname, entry | undefined)` setter with a single
     value-equality guard (extend `itemsEqual`-style comparison to cover both
     `title` and `parents`); clearing means removing the pathname key
     entirely (not storing an empty entry).
   - One `useBreadcrumb(pathname, entry)` publishing hook, using a `useRef`
     holding the last-published entry (by value) to decide whether to call
     `setBreadcrumb` again, instead of `JSON.stringify` in the deps array.
     Deps become `[pathname, entry.title, entry.parents, setBreadcrumb]`
     conceptually, but since `parents` is a fresh array each render, do the
     value comparison inside the effect body via the ref rather than in the
     deps list — deps stay `[pathname, setBreadcrumb]` plus primitive title,
     with the array passed through a ref-stable check. Verify under
     StrictMode (double-invoked mount effects) this doesn't loop: the ref
     comparison must not itself be a state update.
   - Keep `useBreadcrumbTitle(pathname, title)` and
     `useBreadcrumbItems(pathname, items)` as thin wrappers around
     `useBreadcrumb` to avoid churn in `connections/$uid.tsx` and
     `queries/$uid.tsx` (per the spec's own suggestion) — each wraps its
     argument into `{ title }` / `{ parents: items }` and calls
     `useBreadcrumb`.
   - Context value exposes `entries: Record<string, BreadcrumbEntry>` and
     `setBreadcrumb` (replacing `titles`/`setBreadcrumbTitle`/`items`/
     `setBreadcrumbItems`).

2. **`front/src/components/layout/Header.tsx`**
   - Rename local `interface BreadcrumbItem` to `Crumb`, update all local
     usages (`breadcrumbs: Crumb[]`, function signatures if any).
   - Read `entries` from `useBreadcrumbContext()` instead of `titles`/`items`;
     replace `items[href]` with `entries[href]?.parents` and `titles[href]`
     with `entries[href]?.title`.

3. **`front/src/routes/_authenticated/queries/$uid.tsx`** and
   **`front/src/routes/_authenticated/connections/$uid.tsx`**: no changes
   needed if wrapper hooks are kept (option chosen in step 1). Verify imports
   still resolve.

4. QA: `bun run lint`, `bun run build` (typecheck), any unit tests touching
   Breadcrumb/Header, and the breadcrumb-related e2e assertions in
   `front/e2e/observability.spec.ts` if the stack is reachable.
