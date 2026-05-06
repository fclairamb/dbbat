# Slack OAuth Username Accent Stripping

## Goal

Fix `generateUniqueUsername` so that accented Latin characters in a Slack user's display name are folded to their ASCII equivalent instead of being deleted. Today, `mélanie.samedi` becomes `mlanie.samedi` — the `é` disappears entirely. Expected: `melanie.samedi`.

## Why this matters

Slack OAuth is a primary first-login path. The auto-generated username is user-visible in:

- the web UI (user list, profile, grant assignment dropdowns)
- audit logs (every query is attributed by username)
- API responses (`/api/v1/users`, `/api/v1/connections`)
- the grants table (operators tag grants by username)

A username with a hole in the middle (`mlanie.samedi`) looks like a bug, is awkward to type, and undermines trust in the audit trail. Anyone whose display name contains `é è ê à â ç ñ ü ö …` — i.e. most non-English Slack workspaces — hits this.

## Root cause

`internal/api/oauth.go:313-337`:

```go
var usernameRegexp = regexp.MustCompile(`[^a-z0-9._-]`)

func (s *Server) generateUniqueUsername(ctx context.Context, displayName, email string) string {
    base := displayName
    if base == "" && email != "" {
        parts := strings.SplitN(email, "@", 2)
        base = parts[0]
    }
    if base == "" {
        base = "user"
    }

    base = strings.ToLower(base)
    base = strings.Map(func(r rune) rune {
        if unicode.IsSpace(r) {
            return '.'
        }
        return r
    }, base)
    base = usernameRegexp.ReplaceAllString(base, "") // ← strips `é`, doesn't fold to `e`
    ...
}
```

The regex character class `[^a-z0-9._-]` is ASCII-only. `é` (U+00E9) is not in it, so `ReplaceAllString(..., "")` deletes it. There is no Unicode normalization step before the strip, so `mélanie.samedi` → `mlanie.samedi`.

Trace:

| Step | Value |
|------|-------|
| Input display name | `mélanie.samedi` |
| After `ToLower` | `mélanie.samedi` |
| After space→dot map | `mélanie.samedi` |
| After regex strip | `mlanie.samedi` ← **bug** |

## Fix approach

Insert a Unicode-folding step before the regex strip, using `golang.org/x/text` (already an indirect dep at v0.36.0):

```go
import (
    "unicode"
    "golang.org/x/text/runes"
    "golang.org/x/text/transform"
    "golang.org/x/text/unicode/norm"
)

// Decompose to NFD, drop combining marks (accents), recompose.
var accentFold = transform.Chain(
    norm.NFD,
    runes.Remove(runes.In(unicode.Mn)),
    norm.NFC,
)
```

In `generateUniqueUsername`, between the space-to-dot map and the regex strip:

```go
base, _, _ = transform.String(accentFold, base)
```

This handles the bulk of European Latin accents (`é→e`, `è→e`, `ê→e`, `à→a`, `ñ→n`, `ü→u`, `ç→c`, `ö→o`, …). Letters that don't decompose under NFD (`ø`, `ß`, `æ`, `ł`) and non-Latin scripts (CJK, Cyrillic, Arabic, …) still fall through to the regex strip — acceptable, because the email-local-part fallback and the `"user"` final fallback already cover the "username became empty" case.

Promote `golang.org/x/text` to a direct dependency (run `go mod tidy` after the import).

## Acceptance criteria

- [ ] `mélanie.samedi` → `melanie.samedi`
- [ ] `José.García` → `jose.garcia`
- [ ] `François Müller` → `francois.muller`
- [ ] Pure-ASCII input unchanged: `alice.smith` → `alice.smith`
- [ ] Empty / whitespace-only display name still falls back to `user` or to the email local part
- [ ] Email fallback path still works when display name is empty (with accents in the email local part folded as well, e.g. `josé@x.com` → `jose`)
- [ ] Non-Latin scripts (CJK, Cyrillic) still fall back gracefully — either to the email local part or to `user` — with no panic and no empty username slipping through
- [ ] Uniqueness logic (random suffix on collision) unchanged
- [ ] Unit tests added in `internal/api/oauth_test.go` (create the file if it doesn't exist) covering all of the above
- [ ] `golang.org/x/text` promoted to a direct dependency in `go.mod`
- [ ] `make test` and `make lint` pass

## Implementation notes

- Single-file change: `internal/api/oauth.go`. Keep the existing regex; just fold accents first.
- Define `accentFold` as a package-level `var` next to `usernameRegexp` so it's compiled once.
- Apply the fold **after** `strings.ToLower` and **before** the regex strip — order doesn't matter for correctness in this case but reads cleanest there.
- The transform reuse: `transform.String` allocates a new string each call. That's fine — username generation runs once per first-login, not in a hot path.

## Out of scope

- **Backfilling already-corrupted usernames** (e.g. `mlanie.samedi` already in the DB). Existing users are looked up by Slack subject / email at auth time, so the corrupted username is cosmetic but stable. Remediation for any specific user who complains is a manual admin rename, tracked separately.
- **Full unidecode-style transliteration** (`ß→ss`, `ø→o`, `日本→ri ben`). Would need a heavier dependency (e.g. `mozillazg/go-unidecode`). Revisit only if real users with such names hit empty-username fallback.

## Verification

1. `make test` — new unit tests pass.
2. `make lint` — no new warnings.
3. Manual: in dev mode (`make dev`), point the Slack OAuth provider at a test workspace with a user whose display name contains an accent. Complete the OAuth flow and check `GET /api/v1/users` — the new user's `username` field should contain the folded form (e.g. `melanie.samedi`).

## Implementation Plan

1. **Add `accentFold` transform** in `internal/api/oauth.go` next to `usernameRegexp`. Use the chain `norm.NFD → runes.Remove(runes.In(unicode.Mn)) → norm.NFC` from `golang.org/x/text/{runes,transform,unicode/norm}`.
2. **Apply the fold** in `generateUniqueUsername`, right after `strings.ToLower` and before the regex strip: `base, _, _ = transform.String(accentFold, base)`.
3. **Promote `golang.org/x/text`** to a direct dependency via `go mod tidy` after the import lands.
4. **Create `internal/api/oauth_test.go`** with table-driven tests for `generateUniqueUsername`, covering all acceptance criteria. Use a mock or in-memory store to avoid the uniqueness-collision path in unit tests; focus on the canonicalization output.
5. **QA**: `make test`, `make lint`, `make build-binary`. Confirm the existing oauth flow still compiles and no regressions.
