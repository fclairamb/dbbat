# Handle 401 Errors in Frontend

## Summary

The frontend UI does not correctly handle HTTP 401 (Unauthorized) responses. When a user's session expires or their token becomes invalid, API calls fail silently or display generic error messages instead of redirecting to the login page.

## Issue Reference

GitHub Issue #23: "UI doesn't handle correctly 401 errors"

## Current Behavior

When a 401 response is received from the API:

1. **No redirect**: The user stays on the current page
2. **Generic errors**: Error messages like "Failed to load users" appear instead of authentication-specific feedback
3. **Broken state**: The app remains in an authenticated state despite the token being invalid
4. **Silent failures**: Some components may fail to render data without clear indication of why

## Root Cause Analysis

The frontend API client (`front/src/api/client.ts`) has middleware that adds Bearer tokens to requests, but it lacks a response interceptor to catch 401 responses:

```typescript
// Current implementation (problematic)
authMiddleware = apiClient.use({
  onRequest({ request }) {
    request.headers.set("Authorization", `Bearer ${token}`);
    return request;
  },
  // Missing: onResponse handler for 401s
});
```

The web session authentication spec (`specs/2026-01-09-web-session-authentication.md`) already defines the expected behavior, but it was not fully implemented:

```typescript
// Expected implementation (from spec)
authMiddleware = apiClient.use({
  onRequest({ request }) {
    request.headers.set("Authorization", `Bearer ${token}`);
    return request;
  },
  onResponse({ response }) {
    // Auto-logout on 401 (token expired/revoked)
    if (response.status === 401) {
      clearToken();
      window.location.href = "/app/login";
    }
    return response;
  },
});
```

## Expected Behavior

When a 401 response is received:

1. **Clear the stored token**: Remove the invalid token from localStorage
2. **Redirect to login**: Navigate the user to `/app/login`
3. **Clean auth state**: Reset the authentication context to unauthenticated state

## Implementation Plan

### 1. Add Response Interceptor to API Client

**File**: `front/src/api/client.ts`

Add an `onResponse` handler to the existing middleware:

```typescript
const TOKEN_KEY = "dbbat_session_token";

const clearToken = (): void => {
  localStorage.removeItem(TOKEN_KEY);
};

export const setupBearerAuth = (token: string): void => {
  if (authMiddleware) {
    apiClient.eject(authMiddleware);
  }

  authMiddleware = apiClient.use({
    onRequest({ request }) {
      request.headers.set("Authorization", `Bearer ${token}`);
      return request;
    },
    onResponse({ response }) {
      // Auto-logout on 401 (token expired/revoked)
      if (response.status === 401) {
        clearToken();
        window.location.href = "/app/login";
      }
      return response;
    },
  });
};
```

### 2. Handle Login Page Exception

The login page itself makes API calls (for authentication). The 401 interceptor should not redirect when already on the login page to avoid infinite loops:

```typescript
onResponse({ response }) {
  if (response.status === 401) {
    // Don't redirect if already on login page
    if (!window.location.pathname.endsWith("/login")) {
      clearToken();
      window.location.href = "/app/login";
    }
  }
  return response;
}
```

### 3. Optional: Show Toast Notification

Before redirecting, optionally show a brief toast notification to inform the user:

```typescript
onResponse({ response }) {
  if (response.status === 401) {
    if (!window.location.pathname.endsWith("/login")) {
      clearToken();
      // Toast notification would require access to the toast context
      // This is optional - the redirect itself is sufficient feedback
      window.location.href = "/app/login";
    }
  }
  return response;
}
```

Note: Showing a toast before redirect is complex because the middleware runs outside React context. A simpler approach is to pass a query parameter to the login page:

```typescript
window.location.href = "/app/login?session_expired=true";
```

Then the login page can show an appropriate message based on this parameter.

## Alternative Approaches Considered

### Global TanStack Query Error Handler

TanStack Query supports global error handlers:

```typescript
const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      onError: (error) => {
        // Handle 401 here
      },
    },
  },
});
```

**Why not chosen**: The openapi-fetch error object doesn't preserve HTTP status codes, making it difficult to distinguish 401s from other errors. The response interceptor approach is more reliable because it has direct access to the response object.

### React Error Boundary

An error boundary could catch authentication errors and redirect.

**Why not chosen**: Error boundaries are designed for rendering errors, not for API response handling. The middleware approach handles the error before it propagates to React.

## Testing

### Manual Testing

1. Log in to the application
2. Manually invalidate the session (delete from database or wait for expiration)
3. Navigate to any page that makes API calls
4. Verify the user is redirected to the login page

### E2E Testing

Add a Playwright test:

```typescript
test("redirects to login on 401", async ({ page }) => {
  // Log in first
  await page.goto("/app/login");
  await page.fill('input[name="username"]', "admin");
  await page.fill('input[name="password"]', "admintest");
  await page.click('button[type="submit"]');
  await page.waitForURL("/app");

  // Clear the token to simulate expiration
  await page.evaluate(() => localStorage.removeItem("dbbat_session_token"));

  // Navigate to a page that makes API calls
  await page.goto("/app/users");

  // Should redirect to login
  await expect(page).toHaveURL(/\/login/);
});
```

## Files to Modify

| File | Changes |
|------|---------|
| `front/src/api/client.ts` | Add `onResponse` handler to middleware, add `clearToken` function |
| `front/src/routes/login.tsx` | Optionally display "Session expired" message if `session_expired` query param is present |

## References

- GitHub Issue #23
- Spec: `specs/2026-01-09-web-session-authentication.md` (lines 297-305 define the expected implementation)
