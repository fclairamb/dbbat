# Slack Auth Phase 3: Frontend "Sign in with Slack"

> Part of: Slack Authentication series

## Goal

Update the login page to show a "Sign in with Slack" button when the backend has Slack auth enabled. Handle the token from the OAuth callback redirect and establish the frontend session.

## Prerequisites

- Phase 2: OAuth "Sign in with Slack" Backend (providers endpoint, OAuth flow, callback with `?token=`)

## Outcome

- Login page queries `GET /api/v1/auth/providers` to discover available auth methods
- "Sign in with Slack" button shown when Slack provider is enabled
- OAuth callback token is consumed from URL and stored as session
- OAuth error codes from callback are displayed as user-friendly messages
- Existing username/password login unchanged

## Non-Goals

- Slack identity management UI (linking/unlinking) — future enhancement
- Profile page showing linked identities — future enhancement

---

## Changes

### 1. Auth Providers Query

**File**: `front/src/api/queries.ts`

Add a query for the providers endpoint:

```tsx
export function useAuthProviders() {
  return useQuery({
    queryKey: ["auth-providers"],
    queryFn: async () => {
      const { data, error } = await apiClient.GET("/api/v1/auth/providers");
      if (error) throw error;
      return data?.providers ?? [];
    },
    staleTime: 5 * 60 * 1000, // Cache for 5 minutes
  });
}
```

### 2. Login Page — Token Consumption

**File**: `front/src/routes/login.tsx`

When the OAuth callback redirects to `/app/login?token=...`, the login page must consume the token immediately and establish the session.

Add to `LoginPage` component, before the existing `useEffect`:

```tsx
// Handle OAuth callback token
useEffect(() => {
  const params = new URLSearchParams(window.location.search);
  const token = params.get("token");
  if (token) {
    // Remove token from URL immediately (security: don't leave in browser history)
    window.history.replaceState({}, "", window.location.pathname);

    // Store token and redirect to app
    storeToken(token);
    refreshUser().then(() => {
      navigate({ to: "/" });
    });
  }
}, []);
```

### 3. Login Page — Error Display

Handle OAuth error codes from the callback redirect (`/app/login?error=...`):

```tsx
// Handle OAuth error from callback
useEffect(() => {
  const params = new URLSearchParams(window.location.search);
  const oauthError = params.get("error");
  if (oauthError) {
    window.history.replaceState({}, "", window.location.pathname);
    setError(oauthErrorMessage(oauthError));
  }
}, []);

function oauthErrorMessage(code: string): string {
  switch (code) {
    case "slack_denied":
      return "Slack authorization was cancelled.";
    case "invalid_state":
      return "Login session expired. Please try again.";
    case "wrong_workspace":
      return "Your Slack workspace is not authorized for this instance.";
    case "token_exchange_failed":
    case "user_info_failed":
      return "Failed to complete Slack login. Please try again.";
    case "user_creation_failed":
      return "Failed to create your account. Contact an administrator.";
    case "slack_not_linked":
      return "No account is linked to your Slack identity. Contact an administrator.";
    default:
      return "An error occurred during login. Please try again.";
  }
}
```

### 4. Login Page — "Sign in with Slack" Button

Add the Slack button below the existing login form, separated by a divider:

```tsx
function LoginPage() {
  const { data: providers } = useAuthProviders();
  const slackProvider = providers?.find((p) => p.type === "slack" && p.enabled);

  // ... existing state and handlers ...

  return (
    <Card>
      <CardHeader>
        <CardTitle>Login</CardTitle>
      </CardHeader>
      <CardContent>
        {/* Existing username/password form */}
        <form onSubmit={handleLoginSubmit}>
          {/* ... existing fields ... */}
        </form>

        {/* Slack OAuth button */}
        {slackProvider && (
          <>
            <div className="relative my-4">
              <div className="absolute inset-0 flex items-center">
                <span className="w-full border-t" />
              </div>
              <div className="relative flex justify-center text-xs uppercase">
                <span className="bg-background px-2 text-muted-foreground">
                  Or continue with
                </span>
              </div>
            </div>

            <Button
              variant="outline"
              className="w-full"
              onClick={() => {
                window.location.href = slackProvider.authorize_url;
              }}
              data-testid="slack-login-button"
            >
              <SlackIcon className="mr-2 h-4 w-4" />
              Sign in with Slack
            </Button>
          </>
        )}
      </CardContent>
    </Card>
  );
}
```

### 5. Slack Icon Component

**File**: `front/src/components/ui/slack-icon.tsx`

A simple Slack logo SVG:

```tsx
export function SlackIcon({ className }: { className?: string }) {
  return (
    <svg
      className={className}
      viewBox="0 0 24 24"
      fill="currentColor"
      xmlns="http://www.w3.org/2000/svg"
    >
      <path d="M5.042 15.165a2.528 2.528 0 0 1-2.52 2.523A2.528 2.528 0 0 1 0 15.165a2.527 2.527 0 0 1 2.522-2.52h2.52v2.52zm1.271 0a2.527 2.527 0 0 1 2.521-2.52 2.527 2.527 0 0 1 2.521 2.52v6.313A2.528 2.528 0 0 1 8.834 24a2.528 2.528 0 0 1-2.521-2.522v-6.313zM8.834 5.042a2.528 2.528 0 0 1-2.521-2.52A2.528 2.528 0 0 1 8.834 0a2.528 2.528 0 0 1 2.521 2.522v2.52H8.834zm0 1.271a2.528 2.528 0 0 1 2.521 2.521 2.528 2.528 0 0 1-2.521 2.521H2.522A2.528 2.528 0 0 1 0 8.834a2.528 2.528 0 0 1 2.522-2.521h6.312zM18.956 8.834a2.528 2.528 0 0 1 2.522-2.521A2.528 2.528 0 0 1 24 8.834a2.528 2.528 0 0 1-2.522 2.521h-2.522V8.834zm-1.27 0a2.528 2.528 0 0 1-2.523 2.521 2.527 2.527 0 0 1-2.52-2.521V2.522A2.527 2.527 0 0 1 15.163 0a2.528 2.528 0 0 1 2.523 2.522v6.312zM15.163 18.956a2.528 2.528 0 0 1 2.523 2.522A2.528 2.528 0 0 1 15.163 24a2.527 2.527 0 0 1-2.52-2.522v-2.522h2.52zm0-1.27a2.527 2.527 0 0 1-2.52-2.523 2.526 2.526 0 0 1 2.52-2.52h6.315A2.528 2.528 0 0 1 24 15.163a2.528 2.528 0 0 1-2.522 2.523h-6.315z" />
    </svg>
  );
}
```

### 6. OpenAPI Spec Update

**File**: `internal/api/openapi.yml`

Add the providers endpoint and Slack auth endpoints:

```yaml
/auth/providers:
  get:
    summary: List authentication providers
    description: Returns enabled authentication methods. Used by the frontend to show login options.
    tags: [Auth]
    responses:
      '200':
        description: List of auth providers
        content:
          application/json:
            schema:
              type: object
              properties:
                providers:
                  type: array
                  items:
                    type: object
                    properties:
                      type:
                        type: string
                        enum: [password, slack]
                      enabled:
                        type: boolean
                      authorize_url:
                        type: string
                        description: URL to initiate OAuth flow (only for OAuth providers)

/auth/slack:
  get:
    summary: Initiate Slack OAuth login
    description: Redirects to Slack's authorization page. After approval, Slack redirects back to the callback endpoint.
    tags: [Auth]
    parameters:
      - in: query
        name: redirect
        schema:
          type: string
        description: URL to redirect to after successful login
    responses:
      '302':
        description: Redirect to Slack authorization

/auth/slack/callback:
  get:
    summary: Slack OAuth callback
    description: Handles the redirect from Slack after authorization. Creates or links user, creates session, redirects to app.
    tags: [Auth]
    parameters:
      - in: query
        name: code
        schema:
          type: string
      - in: query
        name: state
        schema:
          type: string
    responses:
      '302':
        description: Redirect to app with session token
```

### 7. TypeScript Types Regeneration

After OpenAPI spec update:

```bash
cd front && bun run generate-client
```

This updates `front/src/api/schema.ts` with the new provider types.

---

## Visual Design

### Login Page with Slack (password + Slack enabled):

```
┌──────────────────────────────────┐
│         DBBat                    │
│                                  │
│  Username: [____________]        │
│  Password: [____________]        │
│                                  │
│           [  Login  ]            │
│                                  │
│  ────── Or continue with ──────  │
│                                  │
│     [🔲 Sign in with Slack]      │
│                                  │
│              v0.4.0              │
└──────────────────────────────────┘
```

### Login Page without Slack (password only — unchanged):

```
┌──────────────────────────────────┐
│         DBBat                    │
│                                  │
│  Username: [____________]        │
│  Password: [____________]        │
│                                  │
│           [  Login  ]            │
│                                  │
│              v0.4.0              │
└──────────────────────────────────┘
```

---

## E2E Tests

**File**: `front/e2e/slack-auth.spec.ts`

```typescript
import { test, expect } from "./fixtures";

test.describe("Slack Authentication", () => {
  test("should not show Slack button when not configured", async ({ page }) => {
    await page.goto("/app/login");
    await page.waitForLoadState("networkidle");

    // By default in test mode, Slack is not configured
    await expect(page.getByTestId("slack-login-button")).not.toBeVisible();
  });

  test("should display OAuth error messages", async ({ page }) => {
    await page.goto("/app/login?error=slack_denied");
    await page.waitForLoadState("networkidle");

    await expect(page.getByText("Slack authorization was cancelled")).toBeVisible();
  });

  test("should display wrong workspace error", async ({ page }) => {
    await page.goto("/app/login?error=wrong_workspace");
    await page.waitForLoadState("networkidle");

    await expect(
      page.getByText("Your Slack workspace is not authorized")
    ).toBeVisible();
  });

  test("should consume token from OAuth callback", async ({ page }) => {
    // This would require mocking the auth flow
    // For now, test that the token param is removed from URL
    // Full integration test in backend
  });
});
```

---

## Files Summary

| File | Type | Description |
|------|------|-------------|
| `front/src/routes/login.tsx` | Modified | Add Slack button, OAuth token/error handling |
| `front/src/api/queries.ts` | Modified | Add `useAuthProviders` query |
| `front/src/components/ui/slack-icon.tsx` | New | Slack logo SVG component |
| `front/src/api/schema.ts` | Regenerated | New provider types from OpenAPI |
| `internal/api/openapi.yml` | Modified | Add providers + Slack auth endpoints |
| `front/e2e/slack-auth.spec.ts` | New | E2E tests for Slack auth UI |

## Acceptance Criteria

1. "Sign in with Slack" button appears on login page when Slack is configured
2. Button does NOT appear when `DBB_SLACK_CLIENT_ID` is not set
3. Clicking the button navigates to `GET /api/v1/auth/slack`
4. After OAuth callback with `?token=...`, user is logged in and redirected to app
5. Token is immediately removed from URL (not left in browser history)
6. OAuth errors display user-friendly messages on login page
7. Error query param is removed from URL after displaying
8. Existing username/password login is completely unchanged
9. `bun run build` compiles without TypeScript errors
10. E2E tests pass for error message display

## Estimated Size

~80 lines login page changes + ~30 lines queries + ~20 lines Slack icon + ~50 lines OpenAPI + ~40 lines E2E tests = **~220 lines total**
