# Slack OAuth Authentication

## Status: Draft

## Summary

Add Slack as an OAuth authentication provider, allowing users to log in via their Slack workspace identity instead of username/password. This is inspired by the implementation in [SolidPing](https://github.com/fclairamb/solidping), adapted to DBBat's existing authentication model (web session keys, roles, grants).

## Problem

Currently, DBBat only supports username/password authentication. For teams already using Slack, this means:
- Yet another set of credentials to manage
- No SSO experience
- Manual user provisioning by admins
- No identity federation with existing workspace tools

## Solution

Implement Slack OpenID Connect (OIDC) OAuth flow as an alternative login method. Slack-authenticated users receive the same web session tokens (`web_` prefix) as password-authenticated users.

### Key Design Decisions

1. **Slack identity links to existing DBBat users** - Unlike SolidPing (which auto-creates users and organizations), DBBat has a fixed user/role model. Slack login maps to existing users by email, or optionally auto-provisions users with a default role.
2. **No Slack SDK dependency** - Direct HTTP calls to Slack API (same approach as SolidPing).
3. **Reuse existing web session infrastructure** - Slack login produces the same `web_` session tokens, so all existing middleware, expiration, and audit logging works unchanged.

## OAuth Flow

### Slack App Configuration

**Required Slack App settings:**
- OAuth redirect URL: `https://<dbbat-host>/api/v1/auth/slack/callback`
- User scopes: `openid`, `profile`, `email`
- No bot scopes needed (DBBat doesn't interact with Slack channels)

### Flow Diagram

```
┌──────────┐     ┌──────────┐     ┌──────────┐     ┌──────────┐
│ Frontend │     │  DBBat   │     │  Slack   │     │  DBBat   │
│          │     │  API     │     │  OAuth   │     │  Store   │
└────┬─────┘     └────┬─────┘     └────┬─────┘     └────┬─────┘
     │                │                │                │
     │ GET /auth/slack/login           │                │
     │ ?redirect_uri=/app              │                │
     ├───────────────>│                │                │
     │                │                │                │
     │                │ Generate state │                │
     │                │ Store in DB    │                │
     │                │ (10min TTL)    ├───────────────>│
     │                │                │                │
     │  302 → Slack OAuth URL          │                │
     │  (client_id, state, scopes)     │                │
     │<───────────────┤                │                │
     │                │                │                │
     │ User authorizes in Slack        │                │
     ├────────────────────────────────>│                │
     │                │                │                │
     │ 302 → /auth/slack/callback      │                │
     │ ?code=xxx&state=yyy             │                │
     ├───────────────>│                │                │
     │                │                │                │
     │                │ Validate state │                │
     │                │<───────────────┼───────────────>│
     │                │                │                │
     │                │ Exchange code  │                │
     │                │ for token      │                │
     │                ├───────────────>│                │
     │                │<───────────────┤                │
     │                │                │                │
     │                │ GET userInfo   │                │
     │                │ (email, name)  │                │
     │                ├───────────────>│                │
     │                │<───────────────┤                │
     │                │                │                │
     │                │ Find/create    │                │
     │                │ user by email  │                │
     │                │ Create web     │                │
     │                │ session token  ├───────────────>│
     │                │                │                │
     │  302 → redirect_uri             │                │
     │  ?token=web_xxx                 │                │
     │<───────────────┤                │                │
     │                │                │                │
     │ Store token    │                │                │
     │ in localStorage│                │                │
     └────────────────┘                │                │
```

## Configuration

All settings can be specified via **environment variables** or via the **`general_settings` table** (with associated REST APIs). Database settings take precedence over environment variables, allowing initial deployment via env vars and runtime reconfiguration via the admin UI without restarts.

### Settings Table

#### Database Schema

```sql
-- Migration: YYYYMMDDHHMMSS_general_settings.up.sql
CREATE TABLE general_settings (
    key         VARCHAR(100) PRIMARY KEY,
    value       TEXT NOT NULL,
    private     BOOLEAN NOT NULL DEFAULT FALSE,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
```

The `private` flag controls API read behavior:
- `private = false`: Value is returned normally via the API
- `private = true`: Value is **never returned** via the API (write-only). GET requests return `{"key": "slack.client_secret", "value": null, "private": true, "is_set": true}` — confirming the setting exists without exposing it.

#### REST API

```
GET /api/v1/admin/settings
Authorization: Bearer web_xxx (admin only)

Response (200 OK):
{
    "settings": [
        { "key": "public_url", "value": "https://dbbat.example.com", "private": false, "is_set": true },
        { "key": "slack.client_id", "value": "1234567890.abcdef", "private": false, "is_set": true },
        { "key": "slack.client_secret", "value": null, "private": true, "is_set": true },
        { "key": "slack.auto_provision", "value": "true", "private": false, "is_set": true },
        { "key": "slack.email_allow", "value": null, "private": false, "is_set": false }
    ]
}
```

```
GET /api/v1/admin/settings/:key
Authorization: Bearer web_xxx (admin only)

Response (200 OK):
{ "key": "slack.client_id", "value": "1234567890.abcdef", "private": false, "is_set": true }

Response (200 OK) for private key:
{ "key": "slack.client_secret", "value": null, "private": true, "is_set": true }
```

```
PUT /api/v1/admin/settings/:key
Authorization: Bearer web_xxx (admin only)
Content-Type: application/json

Request:
{ "value": "new-client-secret-value" }

Response (200 OK):
{ "key": "slack.client_secret", "value": null, "private": true, "is_set": true }
```

```
DELETE /api/v1/admin/settings/:key
Authorization: Bearer web_xxx (admin only)

Response (204 No Content)
```

Deleting a setting removes the database override; the env var value (if any) takes effect again.

#### Resolution Order

For each setting, the effective value is resolved as:

```
Database value (if exists) → Environment variable (if set) → Default value
```

```go
func (s *SettingsService) Get(ctx context.Context, key string) string {
    // 1. Check database
    if val, err := s.store.GetSetting(ctx, key); err == nil {
        return val
    }
    // 2. Check env var
    if val := s.envMapping[key]; val != "" {
        return val
    }
    // 3. Return default
    return s.defaults[key]
}
```

### Settings Reference

| Key | Env Var | Private | Default | Description |
|-----|---------|---------|---------|-------------|
| `public_url` | `DBB_PUBLIC_URL` | No | *(from Host header)* | Public URL for OAuth redirect URLs and manifest |
| `slack.client_id` | `DBB_SLACK_CLIENT_ID` | No | *(empty)* | Slack OAuth Client ID |
| `slack.client_secret` | `DBB_SLACK_CLIENT_SECRET` | **Yes** | *(empty)* | Slack OAuth Client Secret |
| `slack.auto_provision` | `DBB_SLACK_AUTO_PROVISION` | No | `false` | Auto-create users on first Slack login |
| `slack.default_roles` | `DBB_SLACK_DEFAULT_ROLES` | No | `viewer` | Comma-separated roles for auto-provisioned users |
| `slack.allowed_teams` | `DBB_SLACK_ALLOWED_TEAMS` | No | *(empty)* | Comma-separated Slack Team IDs (empty = allow all) |
| `slack.email_allow` | `DBB_SLACK_EMAIL_ALLOW` | No | *(empty)* | Comma-separated regex patterns for allowed emails |
| `slack.email_deny` | `DBB_SLACK_EMAIL_DENY` | No | *(empty)* | Comma-separated regex patterns for denied emails |

Slack auth is **disabled** when the effective `slack.client_id` is empty. The login page should only show the "Sign in with Slack" button when the backend reports Slack is configured (via `GET /api/v1/auth/providers`).

### Email Filtering Rules

Email filtering uses an **allow-then-deny** strategy (similar to firewall rules):

1. If `email_allow` is set, the email **must match at least one** allow pattern
2. If `email_deny` is set, the email **must not match any** deny pattern
3. Deny rules are evaluated **after** allow rules (deny takes precedence)
4. If neither is set, all emails are allowed

Patterns are Go `regexp` patterns matched against the full email address (case-insensitive).

**Examples (env var or settings API):**

```bash
# Allow only @google.com emails
DBB_SLACK_EMAIL_ALLOW=".*@google\.com"

# Allow @google.com but block external contractors
DBB_SLACK_EMAIL_ALLOW=".*@google\.com"
DBB_SLACK_EMAIL_DENY=".*\.ext@google\.com"

# Allow multiple domains
DBB_SLACK_EMAIL_ALLOW=".*@google\.com,.*@alphabet\.com"

# Block specific users across all domains
DBB_SLACK_EMAIL_DENY="test.*@.*,noreply@.*"
```

**Evaluation logic:**

```go
func (c *SlackConfig) IsEmailAllowed(email string) bool {
    email = strings.ToLower(email)

    // Step 1: Check allow list (if configured, email must match)
    if len(c.EmailAllow) > 0 {
        allowed := false
        for _, pattern := range c.EmailAllow {
            if matched, _ := regexp.MatchString("(?i)^"+pattern+"$", email); matched {
                allowed = true
                break
            }
        }
        if !allowed {
            return false
        }
    }

    // Step 2: Check deny list (if matched, email is rejected)
    for _, pattern := range c.EmailDeny {
        if matched, _ := regexp.MatchString("(?i)^"+pattern+"$", email); matched {
            return false
        }
    }

    return true
}
```

Invalid regex patterns are rejected by the settings API with a `400 Bad Request` error. When loaded from env vars, invalid patterns cause a startup error.

## API Endpoints

### Get Available Auth Providers

```
GET /api/v1/auth/providers

Response (200 OK):
{
    "providers": ["password", "slack"]
}
```

Returns only `["password"]` when Slack is not configured. The frontend uses this to decide which login options to show.

### Initiate Slack Login

```
GET /api/v1/auth/slack/login?redirect_uri=/app

Response: 302 redirect to Slack OAuth URL
```

**Backend logic:**
1. Generate random `state` string
2. Store `state → redirect_uri` mapping in `oauth_states` table (10-minute TTL)
3. Redirect to `https://slack.com/oauth/v2/authorize` with:
   - `client_id`
   - `user_scope=openid,profile,email`
   - `redirect_uri=https://<host>/api/v1/auth/slack/callback`
   - `state`

### OAuth Callback

```
GET /api/v1/auth/slack/callback?code=xxx&state=yyy

Success: 302 redirect to original redirect_uri with token
Error: 302 redirect to /app/login?error=<error_code>&error_description=<message>
```

**Backend logic:**
1. Validate `state` (lookup in `oauth_states`, check not expired, delete after use)
2. Exchange `code` for tokens via `POST https://slack.com/api/oauth.v2.access`
3. Call `GET https://slack.com/api/openid.connect.userInfo` to get user profile
4. Validate `team_id` against `DBB_SLACK_ALLOWED_TEAMS` (if configured)
5. **Validate email against allow/deny rules** (see [Email Filtering Rules](#email-filtering-rules))
6. Find existing user by email:
   - **Found**: Link Slack identity if not already linked, create web session
   - **Not found + auto-provision enabled**: Create user with default roles, link Slack identity, create web session
   - **Not found + auto-provision disabled**: Redirect with `error=user_not_found`
7. Skip `password_change_required` check (Slack users don't have passwords to change)
8. Create web session token (same as password login)
9. Redirect to `redirect_uri?token=web_xxx`

**Error codes:**
- `invalid_state` - State expired or invalid (CSRF protection)
- `slack_error` - Slack API returned an error
- `email_not_found` - Slack profile has no verified email
- `email_not_allowed` - Email rejected by allow/deny rules
- `user_not_found` - No DBBat user with this email and auto-provision is off
- `team_not_allowed` - Slack team not in allowed list

## Database Changes

### New Table: `general_settings`

See [Settings Table](#settings-table) above for schema and API. The migration for this table is shared infrastructure — not Slack-specific — and should be created first.

### New Table: `oauth_states`

Stores temporary OAuth state for CSRF protection (same pattern as SolidPing's `state_entries`).

```sql
-- Migration: YYYYMMDDHHMMSS_oauth_states.up.sql
CREATE TABLE oauth_states (
    state       VARCHAR(64) PRIMARY KEY,
    redirect_uri TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at  TIMESTAMPTZ NOT NULL
);

CREATE INDEX idx_oauth_states_expires_at ON oauth_states(expires_at);
```

```sql
-- Migration: YYYYMMDDHHMMSS_oauth_states.down.sql
DROP TABLE oauth_states;
```

### New Table: `user_providers`

Links external identity providers to DBBat users.

```sql
-- Migration: YYYYMMDDHHMMSS_user_providers.up.sql
CREATE TABLE user_providers (
    uid          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_uid     UUID NOT NULL REFERENCES users(uid) ON DELETE CASCADE,
    provider     VARCHAR(20) NOT NULL,  -- 'slack'
    provider_id  VARCHAR(100) NOT NULL, -- Slack user ID (e.g., U0123456789)
    metadata     JSONB,                 -- { "team_id": "T...", "team_name": "..." }
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- One Slack identity can only link to one DBBat user
CREATE UNIQUE INDEX idx_user_providers_provider ON user_providers(provider, provider_id);

-- Fast lookup by user
CREATE INDEX idx_user_providers_user ON user_providers(user_uid);
```

```sql
-- Migration: YYYYMMDDHHMMSS_user_providers.down.sql
DROP TABLE user_providers;
```

### Users Table Changes

Add `email` column to users table (currently users only have `username`):

```sql
-- Migration: YYYYMMDDHHMMSS_users_email.up.sql
ALTER TABLE users ADD COLUMN email VARCHAR(255);
CREATE UNIQUE INDEX idx_users_email ON users(email) WHERE email IS NOT NULL;
```

```sql
-- Migration: YYYYMMDDHHMMSS_users_email.down.sql
DROP INDEX idx_users_email;
ALTER TABLE users DROP COLUMN email;
```

## User Identity Mapping

### Linking Strategy

| Scenario | Behavior |
|----------|----------|
| Slack email matches existing user | Auto-link on first Slack login |
| Slack email not found, auto-provision on | Create user with default roles |
| Slack email not found, auto-provision off | Error: `user_not_found` |
| User already linked to different Slack ID | Error (one Slack ID per user) |
| Slack ID already linked to different user | Error (one user per Slack ID) |

### Auto-Provisioned Users

When `DBB_SLACK_AUTO_PROVISION=true`:
- Username derived from Slack email (local part, e.g., `john.doe` from `john.doe@company.com`)
- Roles set from `DBB_SLACK_DEFAULT_ROLES` (default: `viewer`)
- `password_changed = true` (no password to change)
- No password hash set (user can only log in via Slack)

## Frontend Changes

### Login Page

When `GET /api/v1/auth/providers` returns `slack`:

```
┌─────────────────────────────────┐
│         Sign in to DBBat        │
│                                 │
│  ┌───────────────────────────┐  │
│  │  Username                 │  │
│  └───────────────────────────┘  │
│  ┌───────────────────────────┐  │
│  │  Password                 │  │
│  └───────────────────────────┘  │
│  ┌───────────────────────────┐  │
│  │        Sign In            │  │
│  └───────────────────────────┘  │
│                                 │
│  ─────────── or ───────────── │
│                                 │
│  ┌───────────────────────────┐  │
│  │  🔗 Sign in with Slack    │  │
│  └───────────────────────────┘  │
└─────────────────────────────────┘
```

### Token Handling from Callback

The frontend needs to handle the redirect from the OAuth callback:

```typescript
// In the app router, handle /app/login?token=web_xxx
useEffect(() => {
    const params = new URLSearchParams(window.location.search);
    const token = params.get('token');
    if (token) {
        storeToken(token);
        // Clean up URL and redirect to dashboard
        navigate('/app', { replace: true });
    }
    const error = params.get('error');
    if (error) {
        setLoginError(params.get('error_description') || error);
    }
}, []);
```

### Admin: Slack App Setup Page

A new admin page at `/settings/slack` helps administrators configure the Slack integration. It displays the Slack app manifest pre-filled with the correct redirect URL for the current DBBat instance, so the admin can copy-paste it into Slack's app creation flow.

**Route:** `src/routes/_authenticated/settings/slack.tsx`

**Access:** Admin only

#### Page Layout

```
┌─────────────────────────────────────────────────────────────────┐
│  Slack Integration                                              │
│  Configure Slack as an authentication provider                  │
├─────────────────────────────────────────────────────────────────┤
│                                                                 │
│  Status: ● Connected  /  ○ Not configured                       │
│  Client ID: ****abcd                                            │
│  Email rules: allow .*@acme\.com, deny .*\.ext@acme\.com        │
│                                                                 │
│  ── Step 1: Create a Slack App ──────────────────────────────── │
│                                                                 │
│  1. Go to https://api.slack.com/apps                            │
│  2. Click "Create New App" → "From a manifest"                  │
│  3. Select your workspace                                       │
│  4. Paste the manifest below                                    │
│                                                                 │
│  ┌───────────────────────────────────────────────────────────┐  │
│  │ {                                                         │  │
│  │   "display_information": {                                │  │
│  │     "name": "DBBat",                                      │  │
│  │     ...                                                   │  │
│  │   },                                                      │  │
│  │   "oauth_config": {                                       │  │
│  │     "redirect_urls": [                                    │  │
│  │       "https://dbbat.example.com/api/v1/auth/slack/..."   │  │
│  │     ],                                                    │  │
│  │     ...                                                   │  │
│  │   }                                                       │  │
│  │ }                                                         │  │
│  └───────────────────────────────────────────────────────────┘  │
│                                                    [ Copy ]     │
│                                                                 │
│  ── Step 2: Configure DBBat ─────────────────────────────────── │
│                                                                 │
│  Client ID          ┌──────────────────────────────────┐        │
│                      │ 1234567890.abcdef                │        │
│                      └──────────────────────────────────┘        │
│  Client Secret      ┌──────────────────────────────────┐        │
│                      │ ●●●●●●●●●●●●  (set)             │        │
│                      └──────────────────────────────────┘        │
│                                                                 │
│  ── Step 3: Access Rules (optional) ─────────────────────────── │
│                                                                 │
│  Allowed Teams      ┌──────────────────────────────────┐        │
│                      │ T0123456789                      │        │
│                      └──────────────────────────────────┘        │
│  Email Allow        ┌──────────────────────────────────┐        │
│                      │ .*@acme\.com                     │        │
│                      └──────────────────────────────────┘        │
│  Email Deny         ┌──────────────────────────────────┐        │
│                      │ .*\.ext@acme\.com                │        │
│                      └──────────────────────────────────┘        │
│                                                                 │
│  ── Step 4: User Provisioning (optional) ────────────────────── │
│                                                                 │
│  Auto-provision     [x] Create users on first Slack login       │
│  Default Roles      ┌──────────────────────────────────┐        │
│                      │ viewer                           │        │
│                      └──────────────────────────────────┘        │
│                                                                 │
│                                              [ Save Settings ]  │
│                                                                 │
└─────────────────────────────────────────────────────────────────┘
```

#### Manifest Generation

The backend provides a pre-filled manifest via a new endpoint:

```
GET /api/v1/admin/slack/manifest

Response (200 OK):
{
    "manifest": { ... },
    "manifest_json": "{ ... }"
}
```

The manifest is generated server-side because it needs the correct `redirect_urls` based on the instance's public URL. The backend derives this from the `public_url` setting (or falls back to the `Host` header).

#### Sample Manifest

```json
{
    "display_information": {
        "name": "DBBat",
        "description": "PostgreSQL observability proxy authentication",
        "background_color": "#1e293b"
    },
    "features": {
        "bot_user": {
            "display_name": "DBBat",
            "always_online": false
        }
    },
    "oauth_config": {
        "redirect_urls": [
            "https://dbbat.example.com/api/v1/auth/slack/callback"
        ],
        "scopes": {
            "user": [
                "openid",
                "profile",
                "email"
            ],
            "bot": []
        }
    },
    "settings": {
        "org_deploy_enabled": false,
        "socket_mode_enabled": false,
        "token_rotation_enabled": false
    }
}
```

Note: Unlike SolidPing's manifest which includes bot scopes, slash commands, and event subscriptions, DBBat's manifest is minimal — it only needs user-scoped OAuth for authentication. No bot functionality is required.

#### API Endpoint

```
GET /api/v1/admin/slack/manifest
Authorization: Bearer web_xxx (admin only)

Response (200 OK):
{
    "manifest": {
        "display_information": { ... },
        "features": { ... },
        "oauth_config": {
            "redirect_urls": ["https://dbbat.example.com/api/v1/auth/slack/callback"],
            "scopes": { "user": ["openid", "profile", "email"], "bot": [] }
        },
        "settings": { ... }
    },
    "manifest_json": "{\n  \"display_information\": ...\n}"
}
```

The admin page fetches Slack configuration status via `GET /api/v1/admin/settings` (filtered to `slack.*` keys). Private settings like `slack.client_secret` return `"value": null, "is_set": true` — the frontend shows "●●●● (set)" or "Not configured" accordingly.

#### Sidebar Navigation

Add a "Slack" item under the existing "Settings" group in the sidebar (next to "API Keys"):

```
Settings
├── API Keys
└── Slack          ← new (admin only)
```

## Audit Logging

Slack-based authentication is logged with:

```json
{
    "user_id": "550e8400-...",
    "key_type": "web",
    "action": "user.login",
    "details": {
        "method": "slack",
        "slack_team_id": "T0123456789",
        "slack_user_id": "U0123456789"
    }
}
```

Auto-provisioned users generate an additional audit entry:

```json
{
    "action": "user.auto_provisioned",
    "details": {
        "method": "slack",
        "email": "john.doe@company.com",
        "roles": ["viewer"]
    }
}
```

## Security Considerations

### CSRF Protection
- OAuth `state` parameter stored server-side with 10-minute TTL
- State is single-use (deleted after validation)
- Prevents authorization code injection attacks

### Team Restriction
- `DBB_SLACK_ALLOWED_TEAMS` restricts which Slack workspaces can authenticate
- Without this, any Slack user whose email matches a DBBat user could log in
- Recommended for production deployments

### Email Allow/Deny Rules
- Provides fine-grained control over which email addresses can authenticate via Slack
- **Allow-then-deny** evaluation: allow rules are checked first, then deny rules override
- Common use case: allow a company domain but block contractor/external patterns (e.g., allow `.*@company.com`, deny `.*\.ext@company.com`)
- Patterns are full regex, anchored with `^...$` — partial matches are not possible
- Invalid regex patterns are rejected at startup to prevent misconfiguration
- Rejected emails produce an `email_not_allowed` error with no details about which rule matched (to avoid leaking rule structure)

### Token Passing
- The callback redirects with `?token=web_xxx` in the URL
- The token appears briefly in browser history/URL bar
- Frontend immediately stores it and cleans the URL
- Future improvement: use a short-lived authorization code instead

### No Password for Slack-Only Users
- Auto-provisioned users have no password hash
- They cannot use password login or password change endpoints
- If Slack auth is later disabled, these users are locked out (admin must set a password)

## Implementation Plan

### Phase 1: General Settings Infrastructure
1. Create `general_settings` table migration
2. Implement `SettingsService` with env var → DB resolution logic
3. Implement settings REST API (`GET/PUT/DELETE /api/v1/admin/settings`)
4. Handle `private` flag (write-only behavior for secrets)
5. Unit tests for resolution order and private flag

### Phase 2: Backend OAuth Flow
1. Migrate Slack config to use `SettingsService` (with env var fallback)
2. Create `oauth_states` and `user_providers` migrations
3. Add `email` column to users
4. Implement `GET /api/v1/auth/providers` endpoint
5. Implement `GET /api/v1/auth/slack/login` endpoint
6. Implement `GET /api/v1/auth/slack/callback` endpoint
7. Add Slack user info fetching (direct HTTP, no SDK)
8. Implement user lookup by email + provider linking
9. Add audit logging for Slack auth events

### Phase 3: Frontend - Login
1. Fetch available providers on login page load
2. Add "Sign in with Slack" button (conditionally shown)
3. Handle `?token=` and `?error=` query params on login page
4. Update AuthContext to support OAuth-initiated sessions

### Phase 4: Frontend - Admin Setup Page
1. Add `GET /api/v1/admin/slack/manifest` backend endpoint
2. Add `/settings/slack` route (admin only)
3. Display manifest with copy-to-clipboard button
4. Settings form: client ID, client secret, email rules, teams, provisioning
5. Save settings via `PUT /api/v1/admin/settings/:key`
6. Show private fields as "●●●● (set)" / "Not configured"
7. Add "Slack" item to sidebar under Settings group

### Phase 5: Auto-Provisioning (Optional)
1. Implement user creation from Slack profile
2. Username derivation from email
3. Default role assignment
4. Add admin UI to view/manage linked providers per user

### Phase 4: Testing
1. Unit tests for OAuth state management
2. Unit tests for user-provider linking logic
3. Unit tests for team restriction
4. E2E tests for full Slack login flow (mocked Slack API)
5. E2E tests for error cases (unknown email, expired state, etc.)

## Future Considerations

1. **Additional providers**: The `user_providers` table is generic - Google, GitHub, etc. can be added later
2. **SCIM provisioning**: Auto-sync users from Slack workspace
3. **Team-to-role mapping**: Map Slack team/channel membership to DBBat roles
4. **Forced SSO**: Disable password login entirely when Slack is configured
5. **Refresh tokens**: Use Slack refresh tokens to keep identity in sync
