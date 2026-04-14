package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/fclairamb/dbbat/internal/auth"
	"github.com/fclairamb/dbbat/internal/crypto"
	"github.com/fclairamb/dbbat/internal/store"
)

const (
	oauthStateBytes  = 32
	oauthStateTTL    = 10 * time.Minute
	randomPassLength = 32
)

// authProviderInfo describes an available auth method.
type authProviderInfo struct {
	Type         string `json:"type"`
	Enabled      bool   `json:"enabled"`
	AuthorizeURL string `json:"authorize_url,omitempty"`
}

// handleAuthProviders returns which authentication methods are available.
// GET /api/v1/auth/providers
func (s *Server) handleAuthProviders(c *gin.Context) {
	providers := make([]authProviderInfo, 0, 1+len(s.oauthProviders))
	providers = append(providers, authProviderInfo{Type: "password", Enabled: true})

	for name := range s.oauthProviders {
		providers = append(providers, authProviderInfo{
			Type:         name,
			Enabled:      true,
			AuthorizeURL: "/api/v1/auth/" + name,
		})
	}

	c.JSON(http.StatusOK, gin.H{"providers": providers})
}

// handleOAuthAuthorize returns a handler that initiates the OAuth flow.
// GET /api/v1/auth/:provider
func (s *Server) handleOAuthAuthorize(providerName string) gin.HandlerFunc {
	return func(c *gin.Context) {
		provider, ok := s.oauthProviders[providerName]
		if !ok {
			writeError(c, http.StatusNotFound, ErrCodeNotFound, "unknown OAuth provider")
			return
		}

		// Generate random state token
		stateToken, err := generateRandomState()
		if err != nil {
			writeInternalError(c, s.logger, err, "failed to generate OAuth state")
			return
		}

		// Persist state for CSRF validation
		oauthState := &store.OAuthState{
			State:     stateToken,
			Provider:  providerName,
			ExpiresAt: time.Now().Add(oauthStateTTL),
		}

		if _, err := s.store.CreateOAuthState(c.Request.Context(), oauthState); err != nil {
			writeInternalError(c, s.logger, err, "failed to persist OAuth state")
			return
		}

		callbackURL := s.buildCallbackURL(c.Request, providerName)
		redirectURL := provider.AuthorizeURL(stateToken, callbackURL)

		c.Redirect(http.StatusFound, redirectURL)
	}
}

// handleOAuthCallback returns a handler that completes the OAuth flow.
// GET /api/v1/auth/:provider/callback
func (s *Server) handleOAuthCallback(providerName string) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := c.Request.Context()

		// 1. Check for provider-side error
		if errParam := c.Query("error"); errParam != "" {
			s.logger.WarnContext(ctx, "OAuth provider returned error",
				slog.String("provider", providerName),
				slog.String("error", errParam),
				slog.String("description", c.Query("error_description")))
			s.redirectWithError(c, ErrCodeOAuthProviderError)
			return
		}

		// 2. Validate and consume state (CSRF protection)
		stateToken := c.Query("state")
		if stateToken == "" {
			s.redirectWithError(c, ErrCodeOAuthStateMismatch)
			return
		}

		oauthState, err := s.store.ConsumeOAuthState(ctx, stateToken)
		if err != nil {
			if errors.Is(err, store.ErrOAuthStateNotFound) {
				s.redirectWithError(c, ErrCodeOAuthStateMismatch)
				return
			}
			writeInternalError(c, s.logger, err, "failed to consume OAuth state")
			return
		}

		if oauthState.Provider != providerName {
			s.redirectWithError(c, ErrCodeOAuthStateMismatch)
			return
		}

		// 3. Exchange authorization code for user info
		provider := s.oauthProviders[providerName]
		callbackURL := s.buildCallbackURL(c.Request, providerName)
		code := c.Query("code")

		oauthUser, err := provider.ExchangeCode(ctx, code, callbackURL)
		if err != nil {
			s.logger.ErrorContext(ctx, "OAuth code exchange failed",
				slog.String("provider", providerName),
				slog.Any("error", err))
			s.redirectWithError(c, ErrCodeOAuthFailed)
			return
		}

		// 4. Find or create the local user
		user, err := s.findOrCreateOAuthUser(ctx, provider, oauthUser)
		if err != nil {
			s.logger.ErrorContext(ctx, "OAuth user resolution failed",
				slog.String("provider", providerName),
				slog.String("provider_id", oauthUser.ProviderID),
				slog.Any("error", err))

			if errors.Is(err, errOAuthUserNotLinked) {
				s.redirectWithError(c, ErrCodeOAuthUserNotLinked)
				return
			}
			s.redirectWithError(c, ErrCodeOAuthFailed)
			return
		}

		// 5. Create web session
		_, plainKey, err := s.store.CreateWebSession(ctx, user.UID)
		if err != nil {
			writeInternalError(c, s.logger, err, "failed to create web session for OAuth user")
			return
		}

		s.logger.InfoContext(ctx, "OAuth login successful",
			slog.String("provider", providerName),
			slog.String("username", user.Username),
			slog.Any("uid", user.UID))

		// 6. Redirect to frontend with token
		baseURL := "/app"
		if s.config != nil && s.config.BaseURL != "" {
			baseURL = s.config.BaseURL
		}
		c.Redirect(http.StatusFound, baseURL+"/login?token="+plainKey)
	}
}

// errOAuthUserNotLinked is returned when no local user could be found or created.
var errOAuthUserNotLinked = errors.New("no linked account and auto-create disabled")

// findOrCreateOAuthUser resolves an OAuthUser to a local user.
// Resolution order:
//  1. Existing identity link (provider + provider_id)
//  2. Match by email against existing usernames
//  3. Auto-create if enabled
func (s *Server) findOrCreateOAuthUser(ctx context.Context, provider auth.OAuthProvider, oauthUser *auth.OAuthUser) (*store.User, error) {
	providerName := provider.Name()

	// 1. Check if identity is already linked
	user, err := s.store.GetUserByIdentity(ctx, providerName, oauthUser.ProviderID)
	if err == nil {
		return user, nil
	}
	if !errors.Is(err, store.ErrIdentityNotFound) {
		return nil, fmt.Errorf("identity lookup: %w", err)
	}

	// 2. Try to match by email (use email as username lookup)
	if oauthUser.Email != "" {
		user, err = s.store.GetUserByUsername(ctx, oauthUser.Email)
		if err == nil {
			// Found a user with username matching the email, link identity
			if linkErr := s.linkIdentity(ctx, user.UID, providerName, oauthUser); linkErr != nil {
				return nil, fmt.Errorf("link identity: %w", linkErr)
			}
			return user, nil
		}
		if !errors.Is(err, store.ErrUserNotFound) {
			return nil, fmt.Errorf("email lookup: %w", err)
		}
	}

	// 3. Auto-create if enabled
	if s.config == nil || !s.config.SlackAuth.AutoCreateUsers {
		return nil, errOAuthUserNotLinked
	}

	role := s.config.SlackAuth.DefaultRole
	if role == "" {
		role = store.RoleConnector
	}

	// Generate a unique username from the display name or email
	username := s.generateUniqueUsername(ctx, oauthUser.DisplayName, oauthUser.Email)

	// Create user with a random password (they authenticate via OAuth)
	randomPass, err := generateRandomPassword()
	if err != nil {
		return nil, fmt.Errorf("generate password: %w", err)
	}

	passwordHash, err := crypto.HashPassword(randomPass)
	if err != nil {
		return nil, fmt.Errorf("hash password: %w", err)
	}

	user, err = s.store.CreateUser(ctx, username, passwordHash, []string{role})
	if err != nil {
		return nil, fmt.Errorf("create user: %w", err)
	}

	// Mark password as changed so the user can access the UI without being forced
	// to change a password they never knew.
	dummyHash := passwordHash // Same hash, just triggers password_changed_at update
	if err := s.store.UpdateUser(ctx, user.UID, store.UserUpdate{PasswordHash: &dummyHash}); err != nil {
		return nil, fmt.Errorf("mark password changed: %w", err)
	}

	// Link the OAuth identity
	if err := s.linkIdentity(ctx, user.UID, providerName, oauthUser); err != nil {
		return nil, fmt.Errorf("link identity: %w", err)
	}

	s.logger.InfoContext(ctx, "auto-created user from OAuth",
		slog.String("provider", providerName),
		slog.String("username", username),
		slog.String("email", oauthUser.Email),
		slog.Any("uid", user.UID))

	return user, nil
}

// linkIdentity creates a UserIdentity record linking a local user to an OAuth provider.
func (s *Server) linkIdentity(ctx context.Context, userID uuid.UUID, providerName string, oauthUser *auth.OAuthUser) error {
	identity := &store.UserIdentity{
		UserID:      userID,
		Provider:    providerName,
		ProviderID:  oauthUser.ProviderID,
		Email:       oauthUser.Email,
		DisplayName: oauthUser.DisplayName,
		Metadata:    oauthUser.RawData,
	}

	_, err := s.store.CreateUserIdentity(ctx, identity)
	return err
}

// generateRandomState produces a cryptographically random hex-encoded state string.
func generateRandomState() (string, error) {
	b := make([]byte, oauthStateBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("crypto/rand: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// generateRandomPassword produces a random password for auto-created users.
func generateRandomPassword() (string, error) {
	b := make([]byte, randomPassLength)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("crypto/rand: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// buildCallbackURL constructs the OAuth callback URL from the current request.
func (s *Server) buildCallbackURL(r *http.Request, providerName string) string {
	scheme := "https"
	if r.TLS == nil {
		// Check common proxy headers
		if fwdProto := r.Header.Get("X-Forwarded-Proto"); fwdProto != "" {
			scheme = fwdProto
		} else {
			scheme = "http"
		}
	}

	host := r.Host
	return fmt.Sprintf("%s://%s/api/v1/auth/%s/callback", scheme, host, providerName)
}

// usernameRegexp matches characters that are NOT safe for usernames.
var usernameRegexp = regexp.MustCompile(`[^a-z0-9._-]`)

// generateUniqueUsername creates a username from a display name or email.
// It sanitizes the input and appends a short suffix if the name is already taken.
func (s *Server) generateUniqueUsername(ctx context.Context, displayName, email string) string {
	// Start with display name, fall back to email local part
	base := displayName
	if base == "" && email != "" {
		parts := strings.SplitN(email, "@", 2)
		base = parts[0]
	}
	if base == "" {
		base = "user"
	}

	// Normalize: lowercase, replace spaces with dots, strip unsafe chars
	base = strings.ToLower(base)
	base = strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return '.'
		}
		return r
	}, base)
	base = usernameRegexp.ReplaceAllString(base, "")

	// Trim to reasonable length
	if len(base) > 30 {
		base = base[:30]
	}
	if base == "" {
		base = "user"
	}

	// Try the base name first
	candidate := base
	if _, err := s.store.GetUserByUsername(ctx, candidate); errors.Is(err, store.ErrUserNotFound) {
		return candidate
	}

	// Append random suffix
	suffix := make([]byte, 4)
	_, _ = rand.Read(suffix)
	return base + "." + hex.EncodeToString(suffix)
}

// redirectWithError redirects the user to the login page with an error code.
func (s *Server) redirectWithError(c *gin.Context, code ErrorCode) {
	baseURL := "/app"
	if s.config != nil && s.config.BaseURL != "" {
		baseURL = s.config.BaseURL
	}
	c.Redirect(http.StatusFound, baseURL+"/login?error="+string(code))
}
