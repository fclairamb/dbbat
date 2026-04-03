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
- Multiple OAuth providers (Google, GitHub) — future; architecture supports it

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

## Slack OIDC Client

**File**: `internal/auth/slack/client.go`

A minimal HTTP client for Slack's OpenID Connect endpoints. No Slack SDK dependency needed.

```go
type Client struct {
    clientID     string
    clientSecret string
    httpClient   *http.Client
}

type TokenResponse struct {
    AccessToken string `json:"access_token"`
    IDToken     string `json:"id_token"`
    TokenType   string `json:"token_type"`
}

type UserInfo struct {
    Sub       string `json:"sub"`          // Slack user ID (e.g., "U013ZGBT0SJ")
    Name      string `json:"name"`         // Display name
    Email     string `json:"email"`
    Picture   string `json:"picture"`      // Avatar URL
    TeamID    string `json:"https://slack.com/team_id"`
    TeamName  string `json:"https://slack.com/team_name"`
}

// ExchangeCode exchanges an authorization code for tokens.
// POST https://slack.com/api/openid.connect.token
func (c *Client) ExchangeCode(ctx context.Context, code, redirectURI string) (*TokenResponse, error)

// GetUserInfo fetches the authenticated user's profile.
// GET https://slack.com/api/openid.connect.userInfo
func (c *Client) GetUserInfo(ctx context.Context, accessToken string) (*UserInfo, error)

// AuthorizeURL builds the Slack authorization URL.
func (c *Client) AuthorizeURL(state, redirectURI, teamID string) string
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
| `internal/auth/slack/client.go` | New | Slack OIDC client (token exchange, user info) |
| `internal/auth/slack/client_test.go` | New | Client tests with httptest mocks |
| `internal/api/slack_auth.go` | New | OAuth handlers (authorize, callback, providers) |
| `internal/api/slack_auth_test.go` | New | Integration tests for OAuth flow |
| `internal/api/server.go` | Modified | Register Slack auth routes |
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

~300 lines Slack client + ~250 lines handlers + ~100 lines config + ~300 lines tests = **~950 lines total**
