# Header breadcrumb renders nested `<li>`, triggering hydration warnings

## Goal

Fix the invalid-HTML/hydration-warning console errors thrown by the breadcrumb in
`front/src/components/layout/Header.tsx`:

```
<%s> cannot contain a nested %s.  li <li>
In HTML, %s cannot be a descendant of <%s>. This will cause a hydration error.  <li> li
```

## Why

`Header.tsx` renders `{index > 0 && <BreadcrumbSeparator />}` *inside* `<BreadcrumbItem>`
(`front/src/components/layout/Header.tsx:61-62`). Both `BreadcrumbItem` and `BreadcrumbSeparator`
(`front/src/components/ui/breadcrumb.tsx`) render a `<li>`, so this nests an `<li>` inside
another `<li>` — invalid HTML that React (and browsers) warn about on every render. Confirmed
present since the initial commit (predates the auto-approve/SSH-tunnel batch), so it's
pre-existing debt rather than a new regression — not fixed as part of that batch to keep the PR
scoped.

## Implementation

Follow the standard shadcn/ui breadcrumb pattern: render `BreadcrumbSeparator` as a **sibling**
of `BreadcrumbItem` inside `BreadcrumbList`, not nested inside it. In
`front/src/components/layout/Header.tsx`, change the `.map(...)` over crumbs so each iteration
emits the separator (when `index > 0`) and the item as two adjacent elements in a fragment,
e.g.:

```tsx
{crumbs.map((crumb, index) => (
  <Fragment key={crumb.href || index}>
    {index > 0 && <BreadcrumbSeparator />}
    <BreadcrumbItem>
      {/* ...existing link/page content... */}
    </BreadcrumbItem>
  </Fragment>
))}
```

Verify by loading any page with a multi-crumb breadcrumb (e.g. a query detail page) and
confirming the "nested `<li>`" console errors are gone.

## Implementation Plan

1. **Fix the markup** in `front/src/components/layout/Header.tsx`: wrap each crumb in a
   `React.Fragment` keyed by `crumb.href || index`, emitting `<BreadcrumbSeparator />` (when
   `index > 0`) as a *sibling* of `<BreadcrumbItem>` so both are direct children of the
   `<ol>` rendered by `BreadcrumbList`. No change needed in
   `front/src/components/ui/breadcrumb.tsx` (it already matches upstream shadcn/ui).
2. **E2E coverage** in `front/e2e/navigation.spec.ts`: add a test that navigates to a
   multi-crumb page, asserts the breadcrumb `<ol>` has no `li li` descendants, and asserts
   no React "cannot contain a nested" / "hydration" console error is emitted.
3. **QA**: `make build-front` plus `bun run lint` from `front/`; run only the touched
   Playwright spec if the e2e harness can run locally.
