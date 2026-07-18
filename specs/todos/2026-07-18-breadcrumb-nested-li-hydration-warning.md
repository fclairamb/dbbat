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
