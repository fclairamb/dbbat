# Web Session Authentication

## Status: Draft

## Summary

Replace localStorage credential storage in the frontend with short-lived API keys (web sessions). This improves security by eliminating plaintext credential storage while leveraging the existing API key infrastructure.

## Problem

The current frontend authentication flow stores username and password in localStorage:

1. User enters credentials on login page
2. Credentials stored in localStorage (if "remember me" is checked)
3. Every API request uses Basic Auth with stored credentials
4. Logout clears localStorage

**Security issues:**
- Credentials stored in plaintext in localStorage
- Accessible to any JavaScript running on the page (XSS vulnerability)
- Persists across browser sessions indefinitely
- No automatic expiration

## Solution

Use the existing API key infrastructure to create **web session keys** - short-lived, auto-generated API keys that authenticate the browser session.

### Web Session Key Characteristics

| Property | Web Session Key | Regular API Key |
|----------|-----------------|-----------------|
| Prefix | `web_` | `dbb_` |
| Max expiration | 1 hour | Unlimited (optional) |
| User-named | No (auto-generated) | Yes |
| Visible in API keys list | No | Yes |
| Can be manually created | No | Yes |
| Can be manually revoked | Yes (logout) | Yes |
| Subject to API key restrictions | Yes | Yes |

### Key Format

```
web_<random_32_chars>
```

Example: `web_k7x9m2p4q8r1s5t3u6v0w2y4z7a9b1c3`

### Database Schema

Extend the existing `api_keys` table with a `key_type` column:

```sql
-- Add key type to distinguish web sessions from regular API keys
ALTER TABLE api_keys ADD COLUMN key_type VARCHAR(10) NOT NULL DEFAULT 'api';

-- Add index for filtering
CREATE INDEX idx_api_keys_key_type ON api_keys(key_type);
```

Key types:
- `api` - Regular API keys (prefix `dbb_`)
- `web` - Web session keys (prefix `web_`)

### API Endpoints

#### Login - Create Web Session

```
POST /api/auth/login
Content-Type: application/json

Request:
{
    "username": "admin",
    "password": "secretpassword"
}

Response (200 OK):
{
    "token": "web_k7x9m2p4q8r1s5t3u6v0w2y4z7a9b1c3",
    "expires_at": "2026-01-09T13:00:00Z",
    "user": {
        "id": "550e8400-e29b-41d4-a716-446655440000",
        "username": "admin",
        "roles": ["admin", "connector"],
        "password_change_required": false
    }
}

Response (401 Unauthorized):
{
    "error": "invalid_credentials",
    "message": "Invalid username or password"
}
```

**Backend logic:**
1. Validate username/password against users table
2. Create new API key with:
   - `key_type = 'web'`
   - `name = 'Web Session'` (or include browser/timestamp info)
   - `expires_at = NOW() + 1 hour`
3. Return the plaintext token (only time it's visible)
4. Include user info to avoid a separate `/me` call

#### Logout - Revoke Web Session

```
POST /api/auth/logout
Authorization: Bearer web_k7x9m2p4q8r1s5t3u6v0w2y4z7a9b1c3

Response (204 No Content)
```

**Backend logic:**
1. Identify the API key from the Bearer token
2. Set `revoked_at = NOW()` on the key
3. Key is immediately invalidated

**Note:** Unlike regular API key revocation, web session logout does NOT require password authentication - the Bearer token is sufficient since the user is already authenticated.

#### Get Current User

```
GET /api/auth/me
Authorization: Bearer web_k7x9m2p4q8r1s5t3u6v0w2y4z7a9b1c3

Response (200 OK):
{
    "id": "550e8400-e29b-41d4-a716-446655440000",
    "username": "admin",
    "roles": ["admin", "connector"],
    "password_change_required": false,
    "session": {
        "expires_at": "2026-01-09T13:00:00Z",
        "created_at": "2026-01-09T12:00:00Z"
    }
}

Response (401 Unauthorized):
{
    "error": "invalid_token",
    "message": "Token is invalid, expired, or revoked"
}
```

**Use cases:**
- Validate session on app load (before rendering protected routes)
- Check if password change is required
- Refresh user roles after changes
- Display session expiration to user

#### Change Password (Re-authentication Required)

Since web session keys inherit API key restrictions (cannot change passwords), password changes require re-authentication:

```
PUT /api/users/:id/password
Content-Type: application/json

Request:
{
    "current_password": "oldpassword",
    "new_password": "newpassword"
}

Response (200 OK):
{
    "message": "Password changed successfully"
}

Response (401 Unauthorized):
{
    "error": "invalid_password",
    "message": "Current password is incorrect"
}
```

**Note:** This endpoint requires the current password in the request body, not Basic Auth. The Bearer token identifies the user, but the password proves current possession of credentials.

## Frontend Implementation

### Authentication Flow

```
┌─────────────────────────────────────────────────────────────────┐
│                         App Load                                 │
└─────────────────────────────────────────────────────────────────┘
                              │
                              ▼
               ┌──────────────────────────┐
               │ Check localStorage for   │
               │ session token            │
               └──────────────────────────┘
                              │
              ┌───────────────┴───────────────┐
              │                               │
        Token exists                    No token
              │                               │
              ▼                               ▼
     ┌─────────────────┐            ┌─────────────────┐
     │ GET /api/auth/me│            │ Redirect to     │
     │                 │            │ /login          │
     └─────────────────┘            └─────────────────┘
              │
     ┌────────┴────────┐
     │                 │
  200 OK            401/Error
     │                 │
     ▼                 ▼
┌─────────────┐  ┌─────────────────┐
│ Set user    │  │ Clear token,    │
│ context,    │  │ redirect to     │
│ show app    │  │ /login          │
└─────────────┘  └─────────────────┘
```

### Login Flow

```
┌─────────────────────────────────────────────────────────────────┐
│                      Login Form Submit                           │
└─────────────────────────────────────────────────────────────────┘
                              │
                              ▼
               ┌──────────────────────────┐
               │ POST /api/auth/login     │
               │ { username, password }   │
               └──────────────────────────┘
                              │
              ┌───────────────┴───────────────┐
              │                               │
           200 OK                          401
              │                               │
              ▼                               ▼
     ┌─────────────────┐            ┌─────────────────┐
     │ Store token in  │            │ Show error      │
     │ localStorage    │            │ message         │
     └─────────────────┘            └─────────────────┘
              │
              ▼
     ┌─────────────────┐
     │ Set Bearer auth │
     │ middleware      │
     └─────────────────┘
              │
              ▼
     ┌─────────────────┐
     │ Redirect to     │
     │ dashboard       │
     └─────────────────┘
```

### Updated API Client

```typescript
// src/api/client.ts
import createClient from 'openapi-fetch';
import type { paths } from './schema';

export const apiClient = createClient<paths>({
  baseUrl: import.meta.env.VITE_API_BASE_URL || ''
});

const TOKEN_KEY = 'dbbat_session_token';

// Get stored token
export const getStoredToken = (): string | null => {
  return localStorage.getItem(TOKEN_KEY);
};

// Store token after login
export const storeToken = (token: string): void => {
  localStorage.setItem(TOKEN_KEY, token);
  setupBearerAuth(token);
};

// Clear token on logout or expiration
export const clearToken = (): void => {
  localStorage.removeItem(TOKEN_KEY);
  clearAuth();
};

// Set up Bearer auth middleware
let authMiddleware: ReturnType<typeof apiClient.use> | null = null;

const setupBearerAuth = (token: string): void => {
  if (authMiddleware) {
    apiClient.eject(authMiddleware);
  }

  authMiddleware = apiClient.use({
    onRequest({ request }) {
      request.headers.set('Authorization', `Bearer ${token}`);
      return request;
    },
    onResponse({ response }) {
      // Auto-logout on 401 (token expired/revoked)
      if (response.status === 401) {
        clearToken();
        window.location.href = '/app/login';
      }
      return response;
    }
  });
};

const clearAuth = (): void => {
  if (authMiddleware) {
    apiClient.eject(authMiddleware);
    authMiddleware = null;
  }
};

// Initialize auth from stored token on module load
const storedToken = getStoredToken();
if (storedToken) {
  setupBearerAuth(storedToken);
}

// For password change - temporary Basic Auth
export const withBasicAuth = async <T>(
  username: string,
  password: string,
  fn: () => Promise<T>
): Promise<T> => {
  const credentials = btoa(`${username}:${password}`);
  const tempMiddleware = apiClient.use({
    onRequest({ request }) {
      request.headers.set('Authorization', `Basic ${credentials}`);
      return request;
    }
  });

  try {
    return await fn();
  } finally {
    apiClient.eject(tempMiddleware);
  }
};
```

### Updated Auth Context

```typescript
// src/contexts/AuthContext.tsx
import { createContext, useContext, useState, useEffect, ReactNode } from 'react';
import { apiClient, storeToken, clearToken, getStoredToken } from '@/api/client';

interface User {
  id: string;
  username: string;
  roles: string[];
  password_change_required: boolean;
}

interface Session {
  expires_at: string;
  created_at: string;
}

interface AuthContextType {
  user: User | null;
  session: Session | null;
  isLoading: boolean;
  login: (username: string, password: string) => Promise<void>;
  logout: () => Promise<void>;
  refreshUser: () => Promise<void>;
}

const AuthContext = createContext<AuthContextType | null>(null);

export function AuthProvider({ children }: { children: ReactNode }) {
  const [user, setUser] = useState<User | null>(null);
  const [session, setSession] = useState<Session | null>(null);
  const [isLoading, setIsLoading] = useState(true);

  // Check session on mount
  useEffect(() => {
    const token = getStoredToken();
    if (token) {
      validateSession();
    } else {
      setIsLoading(false);
    }
  }, []);

  const validateSession = async () => {
    try {
      const { data, error } = await apiClient.GET('/api/auth/me');
      if (error || !data) {
        clearToken();
        setUser(null);
        setSession(null);
      } else {
        setUser({
          id: data.id,
          username: data.username,
          roles: data.roles,
          password_change_required: data.password_change_required
        });
        setSession(data.session);
      }
    } catch {
      clearToken();
      setUser(null);
      setSession(null);
    } finally {
      setIsLoading(false);
    }
  };

  const login = async (username: string, password: string) => {
    const { data, error } = await apiClient.POST('/api/auth/login', {
      body: { username, password }
    });

    if (error || !data) {
      throw new Error('Invalid credentials');
    }

    storeToken(data.token);
    setUser(data.user);
    setSession({
      expires_at: data.expires_at,
      created_at: new Date().toISOString()
    });
  };

  const logout = async () => {
    try {
      await apiClient.POST('/api/auth/logout');
    } finally {
      clearToken();
      setUser(null);
      setSession(null);
    }
  };

  const refreshUser = async () => {
    await validateSession();
  };

  return (
    <AuthContext.Provider value={{ user, session, isLoading, login, logout, refreshUser }}>
      {children}
    </AuthContext.Provider>
  );
}

export const useAuth = () => {
  const context = useContext(AuthContext);
  if (!context) {
    throw new Error('useAuth must be used within AuthProvider');
  }
  return context;
};
```

### Password Change Component

Since password change requires re-entering credentials:

```typescript
// src/components/PasswordChangeDialog.tsx
import { useState } from 'react';
import { Dialog, DialogContent, DialogHeader, DialogTitle } from '@/components/ui/dialog';
import { Input } from '@/components/ui/input';
import { Button } from '@/components/ui/button';
import { apiClient, withBasicAuth } from '@/api/client';
import { useAuth } from '@/contexts/AuthContext';

export function PasswordChangeDialog({ open, onOpenChange }: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
}) {
  const { user } = useAuth();
  const [currentPassword, setCurrentPassword] = useState('');
  const [newPassword, setNewPassword] = useState('');
  const [confirmPassword, setConfirmPassword] = useState('');
  const [error, setError] = useState('');
  const [isLoading, setIsLoading] = useState(false);

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    setError('');

    if (newPassword !== confirmPassword) {
      setError('New passwords do not match');
      return;
    }

    setIsLoading(true);
    try {
      const { error: apiError } = await apiClient.PUT('/api/users/{id}/password', {
        params: { path: { id: user!.id } },
        body: {
          current_password: currentPassword,
          new_password: newPassword
        }
      });

      if (apiError) {
        setError('Current password is incorrect');
        return;
      }

      onOpenChange(false);
      // Optionally: force re-login after password change
    } catch {
      setError('Failed to change password');
    } finally {
      setIsLoading(false);
    }
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Change Password</DialogTitle>
        </DialogHeader>
        <form onSubmit={handleSubmit} className="space-y-4">
          <Input
            type="password"
            placeholder="Current password"
            value={currentPassword}
            onChange={(e) => setCurrentPassword(e.target.value)}
            required
          />
          <Input
            type="password"
            placeholder="New password"
            value={newPassword}
            onChange={(e) => setNewPassword(e.target.value)}
            required
          />
          <Input
            type="password"
            placeholder="Confirm new password"
            value={confirmPassword}
            onChange={(e) => setConfirmPassword(e.target.value)}
            required
          />
          {error && <p className="text-sm text-destructive">{error}</p>}
          <Button type="submit" disabled={isLoading}>
            {isLoading ? 'Changing...' : 'Change Password'}
          </Button>
        </form>
      </DialogContent>
    </Dialog>
  );
}
```

## Session Management

### Expiration Handling

The frontend should handle session expiration gracefully:

1. **Proactive warning**: Display a warning toast 5 minutes before expiration
2. **Auto-redirect**: On 401 response, clear token and redirect to login
3. **Session extension**: Not supported in v1 (user must re-login)

### Multiple Tabs/Windows

Each browser tab shares the same localStorage token. Logout in one tab affects all tabs:

1. Tab A calls `POST /api/auth/logout`
2. Token is revoked server-side
3. Tab B's next API call returns 401
4. Tab B auto-redirects to login

### Session Display

Show session info in the user menu:

```
┌─────────────────────────────┐
│ admin                       │
│ Session expires in 45 min   │
├─────────────────────────────┤
│ Change Password             │
│ Logout                      │
└─────────────────────────────┘
```

## Security Considerations

### Improvements Over Current Approach

| Aspect | Before (Basic Auth) | After (Web Sessions) |
|--------|---------------------|----------------------|
| Stored credential | Password (plaintext) | Session token |
| Credential lifetime | Indefinite | 1 hour max |
| Revocation | Change password | Revoke token |
| XSS impact | Password stolen | Token stolen (limited lifetime) |
| Rotation | Manual | Automatic on re-login |

### Token Storage

Tokens are still stored in localStorage, which is accessible to JavaScript. This is acceptable because:

1. **Short lifetime**: Tokens expire in 1 hour maximum
2. **Revocable**: Tokens can be revoked immediately on logout
3. **No cascading damage**: Stolen token doesn't reveal password
4. **HttpOnly cookies alternative**: Could be implemented in v2 for enhanced security

### Automatic Cleanup

Add a background job to clean up expired web sessions:

```sql
-- Run periodically (e.g., every hour)
DELETE FROM api_keys
WHERE key_type = 'web'
  AND (
    expires_at < NOW()
    OR revoked_at IS NOT NULL
  )
  AND created_at < NOW() - INTERVAL '24 hours';
```

## Migration

### Database Migration

```sql
-- 20260109100000_web_sessions.up.sql

-- Add key_type column to distinguish web sessions from API keys
ALTER TABLE api_keys ADD COLUMN key_type VARCHAR(10) NOT NULL DEFAULT 'api';

-- Add check constraint for valid key types
ALTER TABLE api_keys ADD CONSTRAINT chk_key_type CHECK (key_type IN ('api', 'web'));

-- Index for filtering by key type
CREATE INDEX idx_api_keys_key_type ON api_keys(key_type);
```

```sql
-- 20260109100000_web_sessions.down.sql

DROP INDEX idx_api_keys_key_type;
ALTER TABLE api_keys DROP CONSTRAINT chk_key_type;
ALTER TABLE api_keys DROP COLUMN key_type;
```

### API Keys List Filtering

Update the `GET /api/keys` endpoint to exclude web sessions by default:

```go
func (h *Handler) ListAPIKeys(c *gin.Context) {
    // Only return regular API keys, not web sessions
    keys, err := h.store.ListAPIKeys(userID, "api")
    // ...
}
```

## Implementation Plan

### Phase 1: Backend Auth Endpoints

1. Add `key_type` column to `api_keys` table
2. Create `POST /api/auth/login` endpoint
3. Create `POST /api/auth/logout` endpoint
4. Create `GET /api/auth/me` endpoint
5. Update `PUT /api/users/:id/password` to accept password in body
6. Update `GET /api/keys` to filter out web sessions
7. Add web session cleanup job

### Phase 2: Frontend Migration

1. Update `src/api/client.ts` with new auth flow
2. Update `AuthContext` to use session tokens
3. Update login page to use `POST /api/auth/login`
4. Add logout functionality with `POST /api/auth/logout`
5. Add session validation on app load
6. Update password change flow
7. Add session expiration warning
8. Remove "Remember me" checkbox (sessions always stored)

### Phase 3: Testing

1. Unit tests for login/logout/me endpoints
2. Integration tests for full auth flow
3. E2E tests with Playwright:
   - Login success/failure
   - Session persistence across page reload
   - Session expiration handling
   - Logout
   - Password change
   - Multi-tab behavior

## OpenAPI Specification Updates

```yaml
paths:
  /api/auth/login:
    post:
      summary: Create web session
      operationId: login
      tags: [Auth]
      requestBody:
        required: true
        content:
          application/json:
            schema:
              type: object
              required: [username, password]
              properties:
                username:
                  type: string
                password:
                  type: string
                  format: password
      responses:
        '200':
          description: Login successful
          content:
            application/json:
              schema:
                type: object
                properties:
                  token:
                    type: string
                  expires_at:
                    type: string
                    format: date-time
                  user:
                    $ref: '#/components/schemas/User'
        '401':
          description: Invalid credentials

  /api/auth/logout:
    post:
      summary: Revoke web session
      operationId: logout
      tags: [Auth]
      security:
        - bearerAuth: []
      responses:
        '204':
          description: Logout successful

  /api/auth/me:
    get:
      summary: Get current user and session info
      operationId: getCurrentUser
      tags: [Auth]
      security:
        - bearerAuth: []
      responses:
        '200':
          description: Current user info
          content:
            application/json:
              schema:
                type: object
                properties:
                  id:
                    type: string
                    format: uuid
                  username:
                    type: string
                  roles:
                    type: array
                    items:
                      type: string
                  password_change_required:
                    type: boolean
                  session:
                    type: object
                    properties:
                      expires_at:
                        type: string
                        format: date-time
                      created_at:
                        type: string
                        format: date-time
        '401':
          description: Invalid or expired token
```

## Future Considerations

1. **Session extension**: Allow extending session without re-login (sliding window)
2. **HttpOnly cookies**: Move token to HttpOnly cookie for XSS protection
3. **Refresh tokens**: Implement refresh token pattern for longer sessions
4. **Device management**: Show active sessions, allow revoking from other devices
5. **Remember device**: Longer sessions for trusted devices
6. **Rate limiting**: Limit login attempts per IP/username
