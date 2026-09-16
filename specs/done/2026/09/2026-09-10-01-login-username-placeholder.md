---
model: sonnet
effort: low
---

# The login form's username placeholder reads "admin", which misleads first-time users

## Goal

Replace the `admin` placeholder on the login page's username field with a
neutral hint such as `<username>` (or `Enter your username`, matching the
password field's `Enter your password`), so the empty field no longer looks
like a pre-filled value or an instruction.

## Why

`front/src/routes/login.tsx:348` renders the username input with
`placeholder="admin"`. On a fresh install the default account *is* `admin`,
so the greyed-out text is read as either "already filled in" or "type
`admin` here". During first setup people have been:

- submitting with the field empty, assuming the placeholder counts as a value,
  and getting a validation error they don't understand;
- typing `admin` into an instance where the operator had already renamed or
  disabled that account, or where sign-in is via SSO with a different
  identity;
- assuming `admin` is *their* username once other accounts exist.

The password field next to it already uses an instruction-style placeholder
(`Enter your password`, line 363), so the username field is the odd one out.
The rest of the form (`autoComplete="username"`, `autoFocus`, the
`data-testid="login-username"` hook) is fine and stays.

## Implementation

One-line copy change in `front/src/routes/login.tsx:348`:

```tsx
placeholder="admin"
```

becomes something like

```tsx
placeholder="Enter your username"
```

`<username>` is the literal form asked for; `Enter your username` reads more
naturally next to the existing password hint. Either is acceptable — pick one
and keep the two fields consistent.

Nothing else references the placeholder: the Playwright suites
(`front/e2e/login.spec.ts`, `fixtures.ts`, `auth-rate-limit.spec.ts`,
`observability.spec.ts`, `approvals.spec.ts`) locate the field through
`data-testid="login-username"`, not `getByPlaceholder`, and the showcase
project doesn't reference it. No backend, API or docs change.

Out of scope: the `placeholder="admin"` on the server form's username field
(`front/src/routes/_authenticated/servers/index.tsx:1166`) is a database-user
example in an admin-only form and is not the source of confusion; leave it.
