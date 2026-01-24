# Slack Commands

## Status: Draft

## Summary

Define the Slack bot commands for DBBat: `register`, `changepass`, and `grant`. These commands enable users to self-service their DBBat accounts and request database access directly from Slack.

## Commands Overview

| Command | Description | Who can use |
|---------|-------------|-------------|
| `@dbbat register` | Link Slack identity to DBBat user | Anyone |
| `@dbbat changepass` | Generate password setup link | Registered users |
| `@dbbat grant` | Create database access grant | Admins only |
| `@dbbat help` | Show available commands | Anyone |

## Command: register

### Purpose

Link a Slack user's identity to an existing or new DBBat user account.

### Syntax

```
@dbbat register
```

### Behavior

1. Check if Slack user already has a linked DBBat identity
2. If linked: Reply with "You're already registered as `<username>`"
3. If not linked:
   a. Look for existing DBBat user with matching email (from Slack profile)
   b. If found: Link the identity
   c. If not found: Create new user with Slack display name as username
4. Reply with success message

### Flow Diagram

```
@dbbat register
       │
       ▼
┌──────────────────┐
│ Get Slack user   │
│ info (users.info)│
└────────┬─────────┘
         │
         ▼
┌──────────────────┐     Yes    ┌─────────────────┐
│ Identity exists? ├───────────►│ "Already        │
│ (user_identities)│            │  registered"    │
└────────┬─────────┘            └─────────────────┘
         │ No
         ▼
┌──────────────────┐     Yes    ┌─────────────────┐
│ User with email  ├───────────►│ Link identity   │
│ exists?          │            │ to existing user│
└────────┬─────────┘            └─────────────────┘
         │ No
         ▼
┌──────────────────┐
│ Create new user  │
│ + link identity  │
└────────┬─────────┘
         │
         ▼
┌──────────────────┐
│ Reply: Registered│
│ as <username>    │
└──────────────────┘
```

### Implementation

```go
func (s *Service) handleRegister(ctx context.Context, event AppMentionEvent) error {
    // Check existing identity
    existing, err := s.store.GetUserIdentity(ctx, IdentityTypeSlack, event.User)
    if err == nil && existing != nil {
        user, _ := s.store.GetUserByUID(ctx, existing.UserUID)
        return s.replyEphemeral(event, fmt.Sprintf("You're already registered as `%s`.", user.Username))
    }

    // Get Slack user info
    slackUser, err := s.client.GetUserInfo(ctx, event.User)
    if err != nil {
        return s.replyEphemeral(event, "Failed to get your Slack profile. Please try again.")
    }

    // Try to find existing user by email
    var dbUser *store.User
    if slackUser.Email != "" {
        dbUser, _ = s.store.GetUserByEmail(ctx, slackUser.Email)
    }

    // Create new user if not found
    if dbUser == nil {
        username := s.generateUsername(slackUser.DisplayName)
        dbUser = &store.User{
            Username: username,
            Roles:    []string{store.RoleConnector},
            // Password left empty - must use changepass
        }
        if err := s.store.CreateUser(ctx, dbUser); err != nil {
            return s.replyEphemeral(event, "Failed to create your account. Please contact an admin.")
        }
    }

    // Create identity link
    identity := &store.UserIdentity{
        UserUID:       dbUser.UID,
        IdentityType:  IdentityTypeSlack,
        IdentityValue: event.User,
        Metadata: json.RawMessage(fmt.Sprintf(
            `{"team_id": "%s", "display_name": "%s"}`,
            event.TeamID, slackUser.DisplayName,
        )),
    }
    if err := s.store.CreateUserIdentity(ctx, identity); err != nil {
        return s.replyEphemeral(event, "Failed to link your Slack account. Please contact an admin.")
    }

    return s.replyEphemeral(event, fmt.Sprintf(
        ":white_check_mark: Registered as `%s`. Use `@dbbat changepass` to set your password.",
        dbUser.Username,
    ))
}
```

### Response Messages

**Success (new user):**
```
:white_check_mark: Registered as `john.doe`. Use `@dbbat changepass` to set your password.
```

**Success (linked to existing):**
```
:white_check_mark: Linked your Slack account to existing user `john.doe`.
```

**Already registered:**
```
:information_source: You're already registered as `john.doe`.
```

---

## Command: changepass

### Purpose

Generate a secure, time-limited link for the user to set or change their DBBat password.

### Syntax

```
@dbbat changepass
```

### Behavior

1. Verify user is registered (has linked identity)
2. Generate a secure token (UUIDv7) with 15-minute expiry
3. Store token in `password_reset_tokens` or reuse existing mechanism
4. Reply with a private link to the password setup page

### Token Storage

Use `global_parameters` with a TTL pattern:

| group_key | key | value |
|-----------|-----|-------|
| `password_token` | `<token>` | `{"user_uid": "...", "expires_at": "..."}` |

Or add a new table:

```sql
CREATE TABLE password_setup_tokens (
    token UUID PRIMARY KEY,
    user_uid UUID NOT NULL REFERENCES users(uid) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at TIMESTAMPTZ NOT NULL,
    used_at TIMESTAMPTZ
);
```

### Implementation

```go
func (s *Service) handleChangePass(ctx context.Context, event AppMentionEvent) error {
    // Get linked user
    identity, err := s.store.GetUserIdentity(ctx, IdentityTypeSlack, event.User)
    if err != nil {
        return s.replyEphemeral(event, "You're not registered. Use `@dbbat register` first.")
    }

    // Generate token
    token := uid.NewV7()
    expiresAt := time.Now().Add(15 * time.Minute)

    // Store token
    err = s.store.CreatePasswordSetupToken(ctx, token, identity.UserUID, expiresAt)
    if err != nil {
        return s.replyEphemeral(event, "Failed to generate password link. Please try again.")
    }

    // Build URL
    setupURL := fmt.Sprintf("%s/auth/slack/password-setup/%s", s.config.BaseURL, token)

    // Reply with ephemeral message (only visible to user)
    return s.replyEphemeral(event, fmt.Sprintf(
        ":key: Click here to set your password (expires in 15 minutes):\n%s",
        setupURL,
    ))
}
```

### Password Setup Page

**Route**: `GET /auth/slack/password-setup/:token`

Simple HTML form:
- New password field
- Confirm password field
- Submit button

On submit:
1. Validate token exists and not expired
2. Validate password meets requirements (8+ chars)
3. Hash password with Argon2id
4. Update user
5. Mark token as used
6. Show success message

### Response Messages

**Success:**
```
:key: Click here to set your password (expires in 15 minutes):
https://dbbat.example.com/auth/slack/password-setup/01234567-89ab-cdef-...
```

**Not registered:**
```
:x: You're not registered. Use `@dbbat register` first.
```

---

## Command: grant

### Purpose

Allow admins to create database access grants for users directly from Slack.

### Syntax

```
@dbbat grant -user <username> -db <database> -duration <duration> [-max-queries <n>] [-max-data <size>] [-controls <list>]
```

### Parameters

| Parameter | Required | Description | Example |
|-----------|----------|-------------|---------|
| `-user` | Yes | Target username | `john.doe` or `@john` (Slack mention) |
| `-db` | Yes | Database name | `prod-analytics` |
| `-duration` | Yes | Grant duration | `2h`, `1d`, `7d` |
| `-max-queries` | No | Query limit | `100`, `1000` |
| `-max-data` | No | Data transfer limit | `10MB`, `1GB` |
| `-controls` | No | Comma-separated controls | `read_only,block_copy,block_ddl` |

### Duration Format

Supported formats:
- `30m` - 30 minutes
- `2h` - 2 hours
- `1d` - 1 day
- `7d` - 7 days (maximum)

### Data Size Format

Supported formats:
- `100KB` - 100 kilobytes
- `10MB` - 10 megabytes
- `1GB` - 1 gigabyte

### Controls

| Control | Description |
|---------|-------------|
| `read_only` | Block INSERT, UPDATE, DELETE |
| `block_copy` | Block COPY commands |
| `block_ddl` | Block CREATE, DROP, ALTER, TRUNCATE |

### Behavior

1. Verify requesting user is registered and has admin role
2. Parse and validate all parameters
3. Resolve target user (by username or Slack mention)
4. Verify database exists
5. Create access grant
6. Reply with confirmation (visible to channel)
7. Send DM to target user with grant details

### Implementation

```go
func (s *Service) handleGrant(ctx context.Context, event AppMentionEvent, cmd ParsedCommand) error {
    // Verify admin
    identity, err := s.store.GetUserIdentity(ctx, IdentityTypeSlack, event.User)
    if err != nil {
        return s.replyEphemeral(event, "You're not registered. Use `@dbbat register` first.")
    }

    admin, err := s.store.GetUserByUID(ctx, identity.UserUID)
    if err != nil || !admin.IsAdmin() {
        return s.replyEphemeral(event, ":x: You don't have permission to create grants.")
    }

    // Parse required flags
    targetUsername := cmd.Flags["user"]
    dbName := cmd.Flags["db"]
    durationStr := cmd.Flags["duration"]

    if targetUsername == "" || dbName == "" || durationStr == "" {
        return s.replyEphemeral(event,
            "Usage: `@dbbat grant -user <username> -db <database> -duration <duration>`\n"+
            "Example: `@dbbat grant -user john -db prod -duration 2h`")
    }

    // Resolve user (handle @mention format)
    targetUser, err := s.resolveUser(ctx, targetUsername)
    if err != nil {
        return s.replyEphemeral(event, fmt.Sprintf(":x: User '%s' not found.", targetUsername))
    }

    // Get database
    db, err := s.store.GetDatabaseByName(ctx, dbName)
    if err != nil {
        return s.replyEphemeral(event, fmt.Sprintf(":x: Database '%s' not found.", dbName))
    }

    // Parse duration
    duration, err := parseDuration(durationStr)
    if err != nil {
        return s.replyEphemeral(event, fmt.Sprintf(":x: Invalid duration: %s", durationStr))
    }

    // Parse optional flags
    var maxQueries *int64
    if mqStr := cmd.Flags["max-queries"]; mqStr != "" {
        mq, err := strconv.ParseInt(mqStr, 10, 64)
        if err != nil {
            return s.replyEphemeral(event, fmt.Sprintf(":x: Invalid max-queries: %s", mqStr))
        }
        maxQueries = &mq
    }

    var maxBytes *int64
    if mdStr := cmd.Flags["max-data"]; mdStr != "" {
        mb, err := parseDataSize(mdStr)
        if err != nil {
            return s.replyEphemeral(event, fmt.Sprintf(":x: Invalid max-data: %s", mdStr))
        }
        maxBytes = &mb
    }

    var controls []string
    if ctrlStr := cmd.Flags["controls"]; ctrlStr != "" {
        controls = parseControls(ctrlStr)
        if err := validateControls(controls); err != nil {
            return s.replyEphemeral(event, fmt.Sprintf(":x: Invalid controls: %s", err))
        }
    }

    // Create grant
    now := time.Now()
    grant := &store.AccessGrant{
        UserID:              targetUser.UID,
        DatabaseID:          db.UID,
        Controls:            controls,
        GrantedBy:           admin.UID,
        StartsAt:            now,
        ExpiresAt:           now.Add(duration),
        MaxQueryCounts:      maxQueries,
        MaxBytesTransferred: maxBytes,
    }

    if err := s.store.CreateGrant(ctx, grant); err != nil {
        slog.Error("failed to create grant", "error", err)
        return s.replyEphemeral(event, ":x: Failed to create grant. Please try again.")
    }

    // Reply to channel
    msg := s.buildGrantMessage(grant, targetUser, db, admin)
    if err := s.replyToChannel(event, msg); err != nil {
        slog.Error("failed to send channel message", "error", err)
    }

    // DM target user
    if err := s.notifyGrantRecipient(ctx, targetUser, grant, db, admin); err != nil {
        slog.Warn("failed to DM grant recipient", "error", err)
    }

    return nil
}

func (s *Service) resolveUser(ctx context.Context, ref string) (*store.User, error) {
    // Handle Slack mention format: <@U123ABC|display>
    if strings.HasPrefix(ref, "<@") {
        slackID := strings.TrimPrefix(ref, "<@")
        slackID = strings.Split(slackID, "|")[0]
        slackID = strings.TrimSuffix(slackID, ">")

        identity, err := s.store.GetUserIdentity(ctx, IdentityTypeSlack, slackID)
        if err != nil {
            return nil, err
        }
        return s.store.GetUserByUID(ctx, identity.UserUID)
    }

    // Direct username lookup
    return s.store.GetUserByUsername(ctx, ref)
}
```

### Response Messages

**Success (channel message):**
```
:white_check_mark: *Grant Created*

*User:* john.doe
*Database:* prod-analytics
*Duration:* 2 hours (expires <t:1706123456:R>)
*Controls:* read_only
*Created by:* admin.user
```

**DM to recipient:**
```
:wave: You've been granted access to *prod-analytics*

*Expires:* <t:1706123456:F>
*Controls:* read_only

Connect using:
```
psql "host=dbbat.example.com port=5434 user=john.doe dbname=prod-analytics"
```
```

**Error - Not admin:**
```
:x: You don't have permission to create grants.
```

**Error - Missing parameters:**
```
Usage: `@dbbat grant -user <username> -db <database> -duration <duration>`

Optional flags:
  `-max-queries <n>` - Limit number of queries
  `-max-data <size>` - Limit data transfer (e.g., 10MB, 1GB)
  `-controls <list>` - Comma-separated: read_only, block_copy, block_ddl

Example: `@dbbat grant -user john -db prod -duration 2h -controls read_only`
```

---

## Command: help

### Purpose

Display available commands and their usage.

### Syntax

```
@dbbat help
```

### Response

```
*DBBat Commands*

`@dbbat register`
Link your Slack account to DBBat

`@dbbat changepass`
Get a link to set or change your password

`@dbbat grant -user <user> -db <db> -duration <time> [options]`
_(Admin only)_ Create a database access grant
  Options:
  `-max-queries <n>` - Query limit
  `-max-data <size>` - Data limit (e.g., 10MB)
  `-controls <list>` - read_only, block_copy, block_ddl

`@dbbat help`
Show this help message
```

---

## Error Handling

All commands should handle errors gracefully:

| Scenario | Response |
|----------|----------|
| User not registered | "You're not registered. Use `@dbbat register` first." |
| Not authorized | "You don't have permission to perform this action." |
| Invalid syntax | Show usage help for that command |
| User/DB not found | "User 'x' not found." / "Database 'x' not found." |
| Internal error | "Something went wrong. Please try again or contact support." |

Errors are sent as ephemeral messages (only visible to the user who ran the command).

## Audit Logging

All grant commands should create audit log entries:

```go
s.store.CreateAuditLog(ctx, &store.AuditLog{
    EventType:   "grant_created_slack",
    UserID:      &targetUser.UID,
    PerformedBy: &admin.UID,
    Details: json.RawMessage(fmt.Sprintf(`{
        "database": "%s",
        "duration": "%s",
        "controls": %s,
        "slack_channel": "%s"
    }`, db.Name, duration, controls, event.Channel)),
})
```

## Dependencies

- [Global Parameters and User Identities](./2026-01-24-global-parameters-user-identities.md)
- [Slack Integration](./2026-01-24-slack-integration.md)

## Testing Requirements

### Unit Tests

```go
func TestGrantCommand(t *testing.T) {
    t.Run("parse full command", func(t *testing.T) {
        cmd := ParseMentionText("<@BOT> grant -user john -db prod -duration 2h -max-queries 100 -controls read_only,block_copy")
        assert.Equal(t, "grant", cmd.Command)
        assert.Equal(t, "john", cmd.Flags["user"])
        assert.Equal(t, "prod", cmd.Flags["db"])
        assert.Equal(t, "2h", cmd.Flags["duration"])
        assert.Equal(t, "100", cmd.Flags["max-queries"])
        assert.Equal(t, "read_only,block_copy", cmd.Flags["controls"])
    })

    t.Run("resolve slack mention", func(t *testing.T) {
        // Test <@U123ABC|john> format
    })

    t.Run("parse duration", func(t *testing.T) {
        tests := []struct {
            input    string
            expected time.Duration
        }{
            {"30m", 30 * time.Minute},
            {"2h", 2 * time.Hour},
            {"1d", 24 * time.Hour},
            {"7d", 7 * 24 * time.Hour},
        }
        for _, tt := range tests {
            d, err := parseDuration(tt.input)
            assert.NoError(t, err)
            assert.Equal(t, tt.expected, d)
        }
    })

    t.Run("parse data size", func(t *testing.T) {
        tests := []struct {
            input    string
            expected int64
        }{
            {"100KB", 100 * 1024},
            {"10MB", 10 * 1024 * 1024},
            {"1GB", 1024 * 1024 * 1024},
        }
        for _, tt := range tests {
            size, err := parseDataSize(tt.input)
            assert.NoError(t, err)
            assert.Equal(t, tt.expected, size)
        }
    })
}
```

### Integration Tests

```go
func TestSlackCommands(t *testing.T) {
    t.Run("register creates user and identity", func(t *testing.T) {
        // Mock Slack event
        // Call handleRegister
        // Verify user created
        // Verify identity linked
    })

    t.Run("grant creates access grant", func(t *testing.T) {
        // Setup admin with Slack identity
        // Setup target user
        // Setup database
        // Mock grant command event
        // Verify grant created with correct parameters
    })

    t.Run("non-admin cannot grant", func(t *testing.T) {
        // Setup non-admin user with Slack identity
        // Mock grant command
        // Verify rejection
    })
}
```
