# Slack Auth Phase 2: OAuth "Sign in with Slack" Backend

> Part of: Slack Authentication series

## Goal

Implement the backend OAuth 2.0 flow for "Sign in with Slack". Users click a button on the login page, authorize via Slack, and are redirected back to DBBat with an active web session. New users are auto-provisioned from their Slack profile.

## Prerequisites

- Phase 1: User Identities & OAuth State (tables and store methods)

## Outcome

- `GET /api/v1/auth/slack` — Redirects to Slack authorization
- `GET /api/v1/auth/slack/callback` — Handles Slack callback, creates session
- `GET /api/v1/auth/providers` — Lists enabled auth providers (for frontend discovery)
- Config: `DBB_SLACK_CLIENT_ID`, `DBB_SLACK_CLIENT_SECRET`, `DBB_SLACK_TEAM_ID`
- Auto-provisioning: new users created from Slack profile on first login
- Existing password-based auth continues to work unchanged

## Non-Goals

- Slack bot commands (`@dbbat grant`, etc.) — separate feature
- Admin UI for managing identities — future enhancement

---

## OAuth Provider Abstraction

While Slack is the first provider, the implementation uses an **OAuth provider interface** so that adding Google, GitHub, Microsoft, etc. later requires only implementing the interface — no changes to the callback handler, user resolution, or session creation logic. This pattern is borrowed from solidping's multi-provider auth system.

### Provider Interface

**File**: `internal/auth/oauth.go`

```go
package auth

// OAuthProvider defines the contract for OAuth identity providers.
type OAuthProvider interface {
    // Name returns the provider identifier (e.g., "slack", "google").
    Name() string

    // AuthorizeURL builds the URL to redirect the user to for authorization.
    AuthorizeURL(state, redirectURI string) string

    // ExchangeCode exchanges an authorization code for user information.
    // This handles both the token exchange and user profile fetch.
    ExchangeCode(ctx context.Context, code, redirectURI string) (*OAuthUser, error)
}

// OAuthUser represents the normalized user info returned by any OAuth provider.
type OAuthUser struct {
    ProviderID  string // Provider-specific user ID (e.g., Slack "U013ZGBT0SJ")
    Email       string
    DisplayName string
    TeamID      string // Provider-specific team/org (optional)
    TeamName    string // Team display name (optional)
    AvatarURL   string // Profile picture URL (optional)
    RawData     json.RawMessage // Full provider response for metadata
}
```

### Provider Registration

**File**: `internal/api/server.go`

```go
// In Server struct:
type Server struct {
    // ... existing fields ...
    oauthProviders map[string]auth.OAuthProvider // "slack" → SlackProvider
}

// During setup:
if s.config.SlackAuth.Enabled() {
    slackProvider := slack.NewProvider(s.config.SlackAuth)
    s.oauthProviders["slack"] = slackProvider
}

// Routes — one pair per registered provider:
for name, provider := range s.oauthProviders {
    p := provider // capture
    auth.GET("/"+name, s.handleOAuthAuthorize(p))
    auth.GET("/"+name+"/callback", s.handleOAuthCallback(p))
}
```

### Generic Handlers

The authorize and callback handlers are **provider-agnostic** — they work with any `OAuthProvider`:

```go
func (s *Server) handleOAuthAuthorize(provider auth.OAuthProvider) gin.HandlerFunc {
    return func(c *gin.Context) {
        state := generateRandomState()
        redirectURI := s.buildCallbackURL(provider.Name())
        _ = s.store.CreateOAuthState(ctx, &store.OAuthState{
            State: state, Provider: provider.Name(), ...
        })
        c.Redirect(http.StatusFound, provider.AuthorizeURL(state, redirectURI))
    }
}

func (s *Server) handleOAuthCallback(provider auth.OAuthProvider) gin.HandlerFunc {
    return func(c *gin.Context) {
        // 1. Validate state
        // 2. provider.ExchangeCode(ctx, code, redirectURI)
        // 3. findOrCreateUser(ctx, provider.Name(), oauthUser)
        // 4. Create web session
        // 5. Redirect to app
    }
}
```

This means adding a second provider (e.g., Google) later requires:
1. Implement `auth.OAuthProvider` (~50 lines)
2. Add config fields
3. Register in server setup

No new handlers, no new routes, no changes to user resolution.

---

## Architecture

```
Browser                   DBBat                         Slack
  │                         │                             │
  │  1. Click "Sign in      │                             │
  │     with Slack"         │                             │
  │────────────────────────>│                             │
  │                         │  2. Generate state          │
  │                         │     Store in oauth_states   │
  │  3. 302 Redirect        │                             │
  │<────────────────────────│                             │
  │                         │                             │
  │  4. Authorize at Slack  │                             │
  │─────────────────────────────────────────────────────>│
  │                         │                             │
  │  5. Redirect back with  │                             │
  │     code + state        │                             │
  │────────────────────────>│                             │
  │                         │  6. Consume state           │
  │                         │     (CSRF check)            │
  │                         │                             │
  │                         │  7. Exchange code            │
  │                         │     for access token        │
  │                         │─────────────────────────────>│
  │                         │  8. Token response          │
  │                         │<─────────────────────────────│
  │                         │                             │
  │                         │  9. Fetch user info         │
  │                         │─────────────────────────────>│
  │                         │  10. User profile           │
  │                         │<─────────────────────────────│
  │                         │                             │
  │                         │  11. Find or create user    │
  │                         │      Link identity          │
  │                         │      Create web session     │
  │                         │                             │
  │  12. Redirect to app    │                             │
  │      with session token │                             │
  │<────────────────────────│                             │
```

## Configuration

| Variable | Description | Required | Default |
|----------|-------------|----------|---------|
| `DBB_SLACK_CLIENT_ID` | Slack app OAuth client ID | No* | |
| `DBB_SLACK_CLIENT_SECRET` | Slack app OAuth client secret | No* | |
| `DBB_SLACK_TEAM_ID` | Restrict to one Slack workspace | No | (any workspace) |
| `DBB_SLACK_AUTO_CREATE_USERS` | Create users on first Slack login | No | `true` |
| `DBB_SLACK_DEFAULT_ROLE` | Role for auto-created users | No | `connector` |

\* Slack auth is **disabled** if `DBB_SLACK_CLIENT_ID` is empty. This is the opt-in mechanism.

**Config struct addition** (`internal/config/config.go`):

```go
type SlackAuthConfig struct {
    ClientID        string `koanf:"slack_client_id"`
    ClientSecret    string `koanf:"slack_client_secret"`
    TeamID          string `koanf:"slack_team_id"`
    AutoCreateUsers bool   `koanf:"slack_auto_create_users"`
    DefaultRole     string `koanf:"slack_default_role"`
}

// Enabled returns true if Slack auth is configured.
func (c *SlackAuthConfig) Enabled() bool {
    return c.ClientID != "" && c.ClientSecret != ""
}
```

## API Endpoints

### GET /api/v1/auth/providers

Returns a list of enabled authentication providers. The frontend uses this to decide whether to show the "Sign in with Slack" button.

**Response:**
```json
{
  "providers": [
    {
      "type": "password",
      "enabled": true
    },
    {
      "type": "slack",
      "enabled": true,
      "authorize_url": "/api/v1/auth/slack"
    }
  ]
}
```

This endpoint is **unauthenticated** (public).

### GET /api/v1/auth/slack

Initiates the OAuth flow. Generates a random state, stores it, and redirects to Slack.

**Query parameters:**
| Param | Description | Default |
|-------|-------------|---------|
| `redirect` | Where to send the user after login | `/app/` |

**Behavior:**
1. Generate cryptographically random `state` (32 bytes, hex-encoded)
2. Store `OAuthState{state, provider: "slack", redirect_uri, expires_at: now+10min}`
3. Build Slack authorize URL:
   ```
   https://slack.com/openid/connect/authorize?
     response_type=code
     &client_id=<DBB_SLACK_CLIENT_ID>
     &scope=openid%20email%20profile
     &redirect_uri=<base_url>/api/v1/auth/slack/callback
     &state=<state>
     &team=<DBB_SLACK_TEAM_ID>   (if configured)
     &nonce=<random>
   ```
4. Return `302 Redirect` to that URL

**Note:** We use Slack's **OpenID Connect** flow (`openid email profile` scopes), not the legacy OAuth v2 flow. This gives us standardized user info via the OIDC userinfo endpoint.

### GET /api/v1/auth/slack/callback

Handles the redirect from Slack after user authorization.

**Query parameters (from Slack):**
| Param | Description |
|-------|-------------|
| `code` | Authorization code |
| `state` | The state we sent |
| `error` | Error code (if user denied) |

**Behavior:**

```go
func (s *Server) handleSlackCallback(c *gin.Context) {
    // 1. Check for error from Slack
    if errCode := c.Query("error"); errCode != "" {
        // Redirect to login with error
        redirectToLogin(c, "slack_denied")
        return
    }

    code := c.Query("code")
    stateParam := c.Query("state")

    // 2. Consume OAuth state (CSRF protection)
    oauthState, err := s.store.ConsumeOAuthState(ctx, stateParam)
    if err != nil {
        redirectToLogin(c, "invalid_state")
        return
    }

    // 3. Exchange code for tokens
    tokens, err := s.slackAuth.ExchangeCode(ctx, code)
    if err != nil {
        redirectToLogin(c, "token_exchange_failed")
        return
    }

    // 4. Fetch user info from Slack OIDC userinfo endpoint
    slackUser, err := s.slackAuth.GetUserInfo(ctx, tokens.AccessToken)
    if err != nil {
        redirectToLogin(c, "user_info_failed")
        return
    }

    // 5. Enforce team restriction if configured
    if s.config.SlackAuth.TeamID != "" && slackUser.TeamID != s.config.SlackAuth.TeamID {
        redirectToLogin(c, "wrong_workspace")
        return
    }

    // 6. Find or create DBBat user
    user, err := s.findOrCreateSlackUser(ctx, slackUser)
    if err != nil {
        redirectToLogin(c, "user_creation_failed")
        return
    }

    // 7. Create web session
    _, plainKey, err := s.store.CreateWebSession(ctx, user.UID)
    if err != nil {
        redirectToLogin(c, "session_failed")
        return
    }

    // 8. Redirect to app with token
    redirectURI := oauthState.RedirectURI
    if redirectURI == "" {
        redirectURI = s.config.BaseURL + "/"
    }
    c.Redirect(http.StatusFound, redirectURI + "?token=" + plainKey)
}
```

### User Resolution Logic

When a Slack user authenticates, we need to find or create the corresponding DBBat user:

```go
func (s *Server) findOrCreateSlackUser(ctx context.Context, slackUser *SlackUserInfo) (*store.User, error) {
    // 1. Check if this Slack identity is already linked
    existingUser, err := s.store.GetUserByIdentity(ctx, store.IdentityTypeSlack, slackUser.Sub)
    if err == nil {
        return existingUser, nil // Already linked, return existing user
    }

    // 2. Try to match by email to an existing unlinked user
    if slackUser.Email != "" {
        existingUser, err = s.store.GetUserByEmail(ctx, slackUser.Email)
        if err == nil {
            // Found existing user with same email — link identity
            _, err = s.store.CreateUserIdentity(ctx, &store.UserIdentity{
                UserUID:       existingUser.UID,
                IdentityType:  store.IdentityTypeSlack,
                IdentityValue: slackUser.Sub,
                Email:         slackUser.Email,
                DisplayName:   slackUser.Name,
                Metadata:      buildSlackMetadata(slackUser),
            })
            return existingUser, err
        }
    }

    // 3. Auto-create new user (if enabled)
    if !s.config.SlackAuth.AutoCreateUsers {
        return nil, ErrSlackUserNotLinked
    }

    username := s.generateUniqueUsername(ctx, slackUser)
    passwordHash, _ := crypto.HashPassword(crypto.GenerateRandomPassword(32))

    role := s.config.SlackAuth.DefaultRole
    if role == "" {
        role = store.RoleConnector
    }

    newUser, err := s.store.CreateUser(ctx, username, passwordHash, []string{role})
    if err != nil {
        return nil, fmt.Errorf("failed to create user from Slack: %w", err)
    }

    // Mark password as "changed" so they can use the UI (they don't need a password)
    _ = s.store.UpdateUser(ctx, newUser.UID, store.UserUpdate{
        PasswordHash: &passwordHash,
    })

    // Link identity
    _, err = s.store.CreateUserIdentity(ctx, &store.UserIdentity{
        UserUID:       newUser.UID,
        IdentityType:  store.IdentityTypeSlack,
        IdentityValue: slackUser.Sub,
        Email:         slackUser.Email,
        DisplayName:   slackUser.Name,
        Metadata:      buildSlackMetadata(slackUser),
    })

    return newUser, err
}
```

### Username Generation

Generate a clean username from the Slack profile:

```go
func (s *Server) generateUniqueUsername(ctx context.Context, slackUser *SlackUserInfo) string {
    // Try: "john.doe" from display name
    // Fallback: "john.doe.2", "john.doe.3", etc.
    // Last resort: "slack-<first 8 chars of Sub>"
    base := sanitizeUsername(slackUser.Name)
    if base == "" {
        base = "slack-" + slackUser.Sub[:8]
    }

    candidate := base
    for i := 2; ; i++ {
        _, err := s.store.GetUserByUsername(ctx, candidate)
        if err != nil {
            return candidate // username is available
        }
        candidate = fmt.Sprintf("%s.%d", base, i)
    }
}
```

## Slack Provider Implementation

**File**: `internal/auth/slack/provider.go`

Implements `auth.OAuthProvider` using Slack's OpenID Connect endpoints. No Slack SDK dependency — just HTTP calls.

```go
// Provider implements auth.OAuthProvider for Slack OIDC.
type Provider struct {
    clientID     string
    clientSecret string
    teamID       string // Optional workspace restriction
    httpClient   *http.Client
}

func NewProvider(cfg config.SlackAuthConfig) *Provider

// Name returns "slack".
func (p *Provider) Name() string { return "slack" }

// AuthorizeURL builds the Slack OIDC authorization URL.
func (p *Provider) AuthorizeURL(state, redirectURI string) string {
    // https://slack.com/openid/connect/authorize?
    //   response_type=code&client_id=...&scope=openid email profile
    //   &redirect_uri=...&state=...&team=... (if configured)
}

// ExchangeCode exchanges the authorization code for user info.
// Calls token endpoint, then userinfo endpoint, returns normalized OAuthUser.
func (p *Provider) ExchangeCode(ctx context.Context, code, redirectURI string) (*auth.OAuthUser, error) {
    // 1. POST https://slack.com/api/openid.connect.token
    // 2. GET https://slack.com/api/openid.connect.userInfo
    // 3. Check team restriction
    // 4. Return normalized OAuthUser
}
```

**Endpoints used:**
| Endpoint | Purpose |
|----------|---------|
| `https://slack.com/openid/connect/authorize` | Authorization (user redirected here) |
| `https://slack.com/api/openid.connect.token` | Token exchange (server-to-server) |
| `https://slack.com/api/openid.connect.userInfo` | User profile (server-to-server) |

## Store Additions

**File**: `internal/store/users.go` — Add method:

```go
// GetUserByEmail finds a user by matching email against user_identities.email
// or against a username that looks like an email.
func (s *Store) GetUserByEmail(ctx context.Context, email string) (*User, error)
```

## Route Registration

**File**: `internal/api/server.go`

Add to the unauthenticated auth group:

```go
auth := v1.Group("/auth")
auth.POST("/login", s.handleLogin)
auth.PUT("/password", s.handlePreLoginPasswordChange)
auth.GET("/providers", s.handleAuthProviders)

// Slack OAuth (only registered if configured)
if s.config.SlackAuth.Enabled() {
    auth.GET("/slack", s.handleSlackAuthorize)
    auth.GET("/slack/callback", s.handleSlackCallback)
}
```

## Error Handling

All OAuth errors redirect to the login page with an error query parameter:

```
/app/login?error=slack_denied
/app/login?error=invalid_state
/app/login?error=wrong_workspace
/app/login?error=user_creation_failed
```

The frontend reads the `error` param and displays an appropriate message.

| Error Code | User Message |
|------------|-------------|
| `slack_denied` | "Slack authorization was cancelled." |
| `invalid_state` | "Login session expired. Please try again." |
| `wrong_workspace` | "Your Slack workspace is not authorized for this instance." |
| `token_exchange_failed` | "Failed to complete Slack login. Please try again." |
| `user_creation_failed` | "Failed to create your account. Contact an administrator." |
| `slack_not_linked` | "No DBBat account is linked to your Slack identity. Contact an administrator." |

## Security Considerations

1. **CSRF Protection**: OAuth `state` parameter is random, stored server-side, single-use, expires in 10 minutes
2. **Team Restriction**: Optional `DBB_SLACK_TEAM_ID` restricts login to one Slack workspace
3. **No client-side secrets**: `client_secret` never sent to browser; token exchange is server-to-server
4. **Session token in URL**: The callback redirect includes `?token=...` which is visible in browser history. The frontend should consume it immediately and replace the URL. This is standard OAuth practice.
5. **Random password**: Auto-created users get a random 32-char password they'll never need (they authenticate via Slack)
6. **Rate limiting**: The callback endpoint should be rate-limited to prevent abuse

## Tests

**File**: `internal/auth/slack/client_test.go`

```go
func TestClient_AuthorizeURL(t *testing.T) {
    c := NewClient("client-id", "client-secret")
    url := c.AuthorizeURL("state123", "http://localhost/callback", "T0123")
    assert.Contains(t, url, "client_id=client-id")
    assert.Contains(t, url, "state=state123")
    assert.Contains(t, url, "team=T0123")
}

func TestClient_ExchangeCode(t *testing.T) {
    // Use httptest.Server to mock Slack's token endpoint
    server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        assert.Equal(t, "POST", r.Method)
        json.NewEncoder(w).Encode(TokenResponse{
            AccessToken: "xoxp-test-token",
            TokenType:   "bearer",
        })
    }))
    defer server.Close()

    c := NewClient("id", "secret")
    c.tokenURL = server.URL
    tokens, err := c.ExchangeCode(context.Background(), "test-code", "http://localhost/callback")
    require.NoError(t, err)
    assert.Equal(t, "xoxp-test-token", tokens.AccessToken)
}
```

**File**: `internal/api/slack_auth_test.go`

```go
func TestSlackAuthFlow(t *testing.T) {
    t.Run("authorize redirects to Slack", func(t *testing.T) {
        // GET /api/v1/auth/slack
        // Assert 302 redirect to slack.com
        // Assert state stored in DB
    })

    t.Run("callback creates session for existing linked user", func(t *testing.T) {
        // Setup: user with Slack identity
        // Mock Slack token + userinfo endpoints
        // GET /api/v1/auth/slack/callback?code=xxx&state=yyy
        // Assert redirect to /app/?token=...
    })

    t.Run("callback auto-creates user on first login", func(t *testing.T) {
        // Mock Slack returning new user info
        // GET callback
        // Assert new user created in DB
        // Assert identity linked
        // Assert web session created
    })

    t.Run("callback links existing user by email", func(t *testing.T) {
        // Setup: existing user "john" with no Slack identity
        // Mock Slack returning same email
        // GET callback
        // Assert identity linked to existing user (not new user)
    })

    t.Run("callback rejects wrong workspace", func(t *testing.T) {
        // Config: TeamID = "T0123"
        // Mock Slack returning TeamID = "T9999"
        // Assert redirect to /app/login?error=wrong_workspace
    })

    t.Run("callback rejects invalid state", func(t *testing.T) {
        // GET callback with bogus state
        // Assert redirect to /app/login?error=invalid_state
    })

    t.Run("callback rejects when auto-create disabled", func(t *testing.T) {
        // Config: AutoCreateUsers = false
        // Mock new Slack user (no existing link)
        // Assert redirect to /app/login?error=slack_not_linked
    })

    t.Run("providers endpoint shows slack when configured", func(t *testing.T) {
        // GET /api/v1/auth/providers
        // Assert slack provider in list with enabled=true
    })

    t.Run("providers endpoint hides slack when not configured", func(t *testing.T) {
        // No Slack config
        // GET /api/v1/auth/providers
        // Assert only password provider
    })
}
```

## Files Summary

| File | Type | Description |
|------|------|-------------|
| `internal/auth/oauth.go` | New | `OAuthProvider` interface + `OAuthUser` struct |
| `internal/auth/slack/provider.go` | New | Slack OIDC provider implementing `OAuthProvider` |
| `internal/auth/slack/provider_test.go` | New | Provider tests with httptest mocks |
| `internal/api/oauth.go` | New | Generic OAuth handlers (authorize, callback, providers) |
| `internal/api/oauth_test.go` | New | Integration tests for OAuth flow |
| `internal/api/server.go` | Modified | Register OAuth providers + routes |
| `internal/config/config.go` | Modified | Add `SlackAuthConfig` |
| `internal/store/users.go` | Modified | Add `GetUserByEmail` |

## Slack App Setup

To use "Sign in with Slack", create a Slack app at https://api.slack.com/apps:

1. Create new app → "From scratch"
2. Go to **OAuth & Permissions**
3. Add redirect URL: `https://<your-dbbat>/api/v1/auth/slack/callback`
4. Under **Scopes** → **User Token Scopes**, add: `openid`, `email`, `profile`
5. Go to **Basic Information** → copy Client ID and Client Secret
6. Set `DBB_SLACK_CLIENT_ID` and `DBB_SLACK_CLIENT_SECRET`

## Acceptance Criteria

1. `GET /api/v1/auth/slack` redirects to Slack with correct OAuth parameters
2. After Slack authorization, user is redirected back with a valid session token
3. First-time Slack user is auto-created with `connector` role
4. Returning Slack user gets a session without creating a new account
5. Existing user with matching email is auto-linked on first Slack login
6. Wrong workspace is rejected when `DBB_SLACK_TEAM_ID` is set
7. Invalid/expired state parameter is rejected
8. `GET /api/v1/auth/providers` lists enabled auth methods
9. Slack auth is completely disabled when `DBB_SLACK_CLIENT_ID` is not set
10. Existing password-based login continues to work

## Estimated Size

~80 lines OAuth interface + ~200 lines Slack provider + ~200 lines generic handlers + ~100 lines config + ~300 lines tests = **~880 lines total**
