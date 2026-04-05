# Slack Integration

## Status: Draft

## Summary

Add Slack integration to DBBat, enabling users to register their Slack identity with DBBat, manage their passwords, and request database access grants directly from Slack.

## Problem

Currently, all DBBat user management must be done through the web UI or REST API. For teams that primarily communicate via Slack, this creates friction:

1. Users need to switch contexts to access DBBat
2. Admins must manually coordinate user creation and grant assignments
3. No visibility into database access requests within team communication channels

## Solution

Implement Slack app integration with:

1. **App mention commands**: `@dbbat <command>` for user interactions
2. **Event handling**: Process Slack events for mentions and interactions
3. **Webhook verification**: HMAC-SHA256 signature verification for security

## Architecture

### Components

```
Slack Workspace
    │
    ├── @dbbat mention → Events API
    │
    └── Button clicks → Interactions API
            │
            ▼
┌─────────────────────────────────────────┐
│              DBBat Server               │
│                                         │
│  ┌─────────────────────────────────┐   │
│  │   /integrations/slack/events    │   │ ← Event verification + routing
│  └─────────────────────────────────┘   │
│                 │                       │
│                 ▼                       │
│  ┌─────────────────────────────────┐   │
│  │     Slack Command Parser        │   │ ← Parse @dbbat commands
│  └─────────────────────────────────┘   │
│                 │                       │
│                 ▼                       │
│  ┌─────────────────────────────────┐   │
│  │     Command Handlers            │   │
│  │  - register                     │   │
│  │  - changepass                   │   │
│  │  - grant                        │   │
│  └─────────────────────────────────┘   │
│                 │                       │
│                 ▼                       │
│  ┌─────────────────────────────────┐   │
│  │          Store                  │   │ ← Database operations
│  └─────────────────────────────────┘   │
└─────────────────────────────────────────┘
```

### API Endpoints

| Endpoint | Purpose |
|----------|---------|
| `POST /integrations/slack/events` | Receive Slack events (app_mention) |
| `POST /integrations/slack/interaction` | Receive interaction payloads (buttons, modals) |
| `GET /auth/slack/password-setup/:token` | Password setup page (from changepass command) |

### Configuration

Environment variables:

| Variable | Description | Required |
|----------|-------------|----------|
| `DBB_SLACK_SIGNING_SECRET` | HMAC signing secret from Slack app | Yes |
| `DBB_SLACK_BOT_TOKEN` | Bot OAuth token (xoxb-...) | Yes |
| `DBB_SLACK_APP_ID` | Slack App ID | No |

These are stored in `global_parameters` table after initial setup:

| group_key | key | encrypted |
|-----------|-----|-----------|
| `slack` | `signing_secret` | Yes |
| `slack` | `bot_token` | Yes |
| `slack` | `team_id` | No |
| `slack` | `bot_user_id` | No |

### Event Flow

#### 1. Event Reception

```
Slack → POST /integrations/slack/events
         ├── Verify HMAC signature
         ├── Handle URL verification challenge
         └── Route to event handler
```

#### 2. App Mention Event

```go
type AppMentionEvent struct {
    Type    string `json:"type"`    // "app_mention"
    User    string `json:"user"`    // Slack user ID
    Text    string `json:"text"`    // Full message text
    Channel string `json:"channel"` // Channel ID
    TS      string `json:"ts"`      // Message timestamp
    TeamID  string `json:"team"`    // Workspace ID
}
```

#### 3. Command Parsing

Strip bot mention and parse command:

```
"<@U123ABC> grant -user john -db prod -duration 2h"
     │
     ▼
ParsedCommand{
    Command:    "grant",
    Args:       [],
    Flags: {
        "user":     "john",
        "db":       "prod",
        "duration": "2h",
    },
}
```

### Security

#### Request Verification

All incoming Slack requests must be verified using HMAC-SHA256:

```go
func VerifySlackRequest(signingSecret string, timestamp string, body []byte, signature string) bool {
    baseString := fmt.Sprintf("v0:%s:%s", timestamp, string(body))
    mac := hmac.New(sha256.New, []byte(signingSecret))
    mac.Write([]byte(baseString))
    expected := "v0=" + hex.EncodeToString(mac.Sum(nil))
    return hmac.Equal([]byte(expected), []byte(signature))
}
```

#### Timestamp Validation

Reject requests older than 5 minutes to prevent replay attacks.

#### Identity Verification

For sensitive commands (grant, changepass), verify the Slack user has a linked DBBat identity via the `user_identities` table.

## Implementation

### Package Structure

```
internal/
└── integrations/
    └── slack/
        ├── service.go        # Main service, dependency injection
        ├── events.go         # Event handlers
        ├── interactions.go   # Interaction handlers
        ├── parser.go         # Command parser
        ├── parser_test.go    # Parser tests
        ├── messages.go       # Slack message builders
        ├── middleware.go     # HMAC verification middleware
        └── client.go         # Slack API client wrapper
```

### Service Interface

**File**: `internal/integrations/slack/service.go`

```go
type Service struct {
    store       *store.Store
    crypto      *crypto.Crypto
    config      SlackConfig
    client      *SlackClient
}

type SlackConfig struct {
    SigningSecret string
    BotToken      string
    TeamID        string
    BotUserID     string
}

func NewService(store *store.Store, crypto *crypto.Crypto) (*Service, error)

// Event handlers
func (s *Service) HandleEvents(c *gin.Context)
func (s *Service) HandleInteraction(c *gin.Context)

// Command handlers (internal)
func (s *Service) handleRegister(ctx context.Context, event AppMentionEvent) error
func (s *Service) handleChangePass(ctx context.Context, event AppMentionEvent) error
func (s *Service) handleGrant(ctx context.Context, event AppMentionEvent, cmd ParsedCommand) error
func (s *Service) handleHelp(ctx context.Context, event AppMentionEvent) error
```

### API Routes

**File**: `internal/api/routes.go`

```go
// Slack integration endpoints
slack := r.Group("/integrations/slack")
{
    slack.POST("/events", s.slack.VerifyMiddleware(), s.slack.HandleEvents)
    slack.POST("/interaction", s.slack.VerifyMiddleware(), s.slack.HandleInteraction)
}

// Password setup page (public, token-authenticated)
r.GET("/auth/slack/password-setup/:token", s.handleSlackPasswordSetup)
r.POST("/auth/slack/password-setup/:token", s.handleSlackPasswordSetupSubmit)
```

### Message Responses

Use Slack Block Kit for rich message formatting:

```go
// Success message
func BuildSuccessMessage(text string) Message {
    return Message{
        Blocks: []Block{
            {
                Type: "section",
                Text: &TextObject{
                    Type: "mrkdwn",
                    Text: ":white_check_mark: " + text,
                },
            },
        },
    }
}

// Error message (ephemeral - only visible to user)
func BuildErrorMessage(text string) Message {
    return Message{
        ResponseType: "ephemeral",
        Blocks: []Block{
            {
                Type: "section",
                Text: &TextObject{
                    Type: "mrkdwn",
                    Text: ":x: " + text,
                },
            },
        },
    }
}
```

### Error Handling

All errors are logged server-side but return user-friendly messages to Slack:

| Error | Slack Message |
|-------|---------------|
| User not registered | "You're not registered. Use `@dbbat register` first." |
| Invalid command | "Unknown command. Use `@dbbat help` for available commands." |
| Permission denied | "You don't have permission to perform this action." |
| Database not found | "Database 'xyz' not found." |
| User not found | "User 'abc' not found." |

## Testing

### Unit Tests

```go
func TestParser(t *testing.T) {
    t.Run("parse simple command", func(t *testing.T) {
        cmd := ParseMentionText("<@U123ABC> register")
        assert.Equal(t, "register", cmd.Command)
    })

    t.Run("parse command with flags", func(t *testing.T) {
        cmd := ParseMentionText("<@U123ABC> grant -user john -db prod -duration 2h")
        assert.Equal(t, "grant", cmd.Command)
        assert.Equal(t, "john", cmd.Flags["user"])
        assert.Equal(t, "prod", cmd.Flags["db"])
        assert.Equal(t, "2h", cmd.Flags["duration"])
    })

    t.Run("parse command with quoted args", func(t *testing.T) {
        cmd := ParseMentionText(`<@U123ABC> grant -user "john doe" -db prod`)
        assert.Equal(t, "john doe", cmd.Flags["user"])
    })
}

func TestVerifySlackRequest(t *testing.T) {
    t.Run("valid signature", func(t *testing.T) {
        // Test with known signature
    })

    t.Run("invalid signature", func(t *testing.T) {
        // Test rejection
    })

    t.Run("expired timestamp", func(t *testing.T) {
        // Test 5-minute window
    })
}
```

### Integration Tests

```go
func TestSlackIntegration(t *testing.T) {
    t.Run("register command creates user identity", func(t *testing.T) {
        // Send mock app_mention event
        // Verify user_identity created
    })

    t.Run("grant command creates access grant", func(t *testing.T) {
        // Setup: registered admin user
        // Send grant command
        // Verify grant created in database
    })
}
```

## Slack App Configuration

### Required Scopes

**Bot Token Scopes:**
- `app_mentions:read` - Receive @dbbat mentions
- `chat:write` - Send messages
- `users:read` - Read user info for display names

### Event Subscriptions

Subscribe to these events:
- `app_mention` - Bot mentioned in channel

### Request URL

```
https://your-dbbat-instance.com/integrations/slack/events
```

## Dependencies

- Depends on: [Global Parameters and User Identities](./2026-01-24-global-parameters-user-identities.md)

## Rollout Plan

1. Implement model changes (global_parameters, user_identities)
2. Implement Slack service with event handling
3. Implement command parser and handlers
4. Create Slack app in workspace
5. Configure environment variables
6. Test in development workspace
7. Deploy to production
