---
model: sonnet
effort: low
---

# In demo mode, the admin user's password must not be changeable

## Problem

`handleUpdateUser` in [internal/api/users.go:119](internal/api/users.go#L119) lets any
authenticated caller (admin themselves, or another admin editing the user) set a new
password via `PUT` on a user, with no demo-mode restriction. On the public demo instance
(`DBB_RUN_MODE=demo`), anyone who logs in with `admin`/`admintest` could change the admin
password and lock out the shared demo credentials for everyone else, breaking the demo.

There's already a precedent for this exact class of guard: `handleDeleteUser` blocks
deleting the admin user in demo mode ([internal/api/users.go:249-253](internal/api/users.go#L249-L253)):

```go
// In demo mode, prevent deleting the admin user
if s.config != nil && s.config.IsDemoMode() && userToDelete.Username == "admin" {
    writeError(c, http.StatusForbidden, ErrCodeForbidden, "cannot delete admin user in demo mode")
    return
}
```

No equivalent guard exists for password updates.

## Proposal

In `handleUpdateUser`, before hashing/applying `req.Password`, load (or reuse) the target
user and reject the request when `s.config.IsDemoMode()` is true, the target username is
`admin`, and `req.Password != nil`:

```go
if req.Password != nil && s.config != nil && s.config.IsDemoMode() {
    targetUser, err := s.store.GetUserByUID(c.Request.Context(), uid)
    if err == nil && targetUser.Username == "admin" {
        writeError(c, http.StatusForbidden, ErrCodeForbidden, "cannot change admin password in demo mode")
        return
    }
}
```

Return `403 Forbidden` with `ErrCodeForbidden`, matching the delete-guard's status code and
error style. Place the check early (before the roles/last-admin logic is fine, but must be
before `s.store.UpdateUser` and the Mongo verifier refresh) so nothing is mutated.

Add a test alongside the existing demo-mode delete test (see `internal/api/users_test.go`)
that asserts a demo-mode `PUT` with a new password for the `admin` user is rejected with
403, while password changes for non-admin users still succeed in demo mode.

## Implementation Plan

1. **Guard in `handleUpdateUser`** (`internal/api/users.go`): add the demo-mode check right
   after the existing "API keys cannot change passwords" check (around line 136), before the
   `currentUser`/roles logic and before any mutation. Load the target user via
   `s.store.GetUserByUID(c.Request.Context(), uid)` and reject with `403
   ErrCodeForbidden` ("cannot change admin password in demo mode") when
   `req.Password != nil`, `s.config != nil && s.config.IsDemoMode()`, and
   `targetUser.Username == "admin"`. Note: `internal/api/users_test.go` currently has no
   existing demo-mode test to extend (the delete-admin demo guard has no dedicated test
   either) — the new test is written from scratch following the `setupTestServer` /
   `createTestUser` / `loginUser` helpers in `password_reset_test.go`, using a `*config.Config`
   with `RunMode: config.RunModeDemo`.
2. **Tests** (`internal/api/users_test.go`): add
   `TestUpdateUser_DemoModeAdminPasswordChangeRejected` (demo mode, PUT new password for
   `admin` → 403 `FORBIDDEN`, password hash unchanged in the store) and
   `TestUpdateUser_DemoModeNonAdminPasswordChangeAllowed` (demo mode, PUT new password for a
   non-admin user → 200 success). Need a `doUpdateUserPassword` request helper and a way to
   spin up a server with demo-mode config (either a small local `setupTestServer`-like
   helper or reuse `setupTestServer` then mutate `server.config.RunMode`).
3. Run `gofmt`, `go build ./...`, `go vet ./internal/api/...` (or golangci-lint if
   available), and `go test ./internal/api/...` (requires Docker/testcontainers).
