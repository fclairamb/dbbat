package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"

	"github.com/fclairamb/dbbat/internal/auth"
	"github.com/fclairamb/dbbat/internal/crypto"
	"github.com/fclairamb/dbbat/internal/store"
)

const (
	oauthStateBytes  = 32
	oauthStateTTL    = 10 * time.Minute
	randomPassLength = 32

	// loginRedirectMaxLength bounds the post-login redirect target carried
	// through the OAuth round-trip; anything longer is dropped, not truncated.
	loginRedirectMaxLength = 512
)

// authProviderInfo describes an available auth method.
type authProviderInfo struct {
	Type         string `json:"type"`
	Enabled      bool   `json:"enabled"`
	AuthorizeURL string `json:"authorize_url,omitempty"`
	// DisplayName is the login-button label for providers whose branding is
	// operator-configured (the generic OIDC one). Empty for providers the
	// frontend already knows how to label, like Slack.
	DisplayName string `json:"display_name,omitempty"`
}

// oauthStateMetadata is the JSON payload stashed on the `oauth_states` row
// for the duration of a single login round-trip. It currently carries only
// the PKCE code verifier, which must be minted before the redirect and
// presented at the callback — and must survive a restart or a hop to another
// replica, so it cannot live in process memory.
type oauthStateMetadata struct {
	PKCEVerifier string `json:"pkce_verifier,omitempty"`
}

// handleAuthProviders returns which authentication methods are available.
// GET /api/v1/auth/providers
func (s *Server) handleAuthProviders(c *gin.Context) {
	providers := make([]authProviderInfo, 0, 1+len(s.oauthProviders))
	providers = append(providers, authProviderInfo{Type: "password", Enabled: true})

	for name, provider := range s.oauthProviders {
		info := authProviderInfo{
			Type:         name,
			Enabled:      true,
			AuthorizeURL: "/api/v1/auth/" + name,
		}

		if namer, ok := provider.(auth.DisplayNamer); ok {
			info.DisplayName = namer.DisplayName()
		}

		providers = append(providers, info)
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

		callbackURL := s.buildCallbackURL(c.Request, providerName)

		// PKCE-capable providers mint a code verifier here; it has to be
		// persisted with the state row *before* the redirect, because the
		// callback may well land on a different replica.
		redirectURL, verifier, err := authorizeURLFor(c.Request.Context(), provider, stateToken, callbackURL)
		if err != nil {
			writeInternalError(c, s.logger, err, "failed to build OAuth authorization URL")
			return
		}

		var metadata json.RawMessage
		if verifier != "" {
			metadata, err = json.Marshal(oauthStateMetadata{PKCEVerifier: verifier})
			if err != nil {
				writeInternalError(c, s.logger, err, "failed to encode OAuth state metadata")
				return
			}
		}

		// Persist state for CSRF validation. The state row also carries the
		// post-login redirect target (e.g. the device consent page that
		// bounced the user here), so the callback can send the user back to
		// where they started instead of the dashboard.
		oauthState := &store.OAuthState{
			State:       stateToken,
			Provider:    providerName,
			RedirectURL: sanitizeLoginRedirect(c.Query("redirect")),
			Metadata:    metadata,
			ExpiresAt:   time.Now().Add(oauthStateTTL),
		}

		if _, err := s.store.CreateOAuthState(c.Request.Context(), oauthState); err != nil {
			writeInternalError(c, s.logger, err, "failed to persist OAuth state")
			return
		}

		c.Redirect(http.StatusFound, redirectURL)
	}
}

// authorizeURLFor builds the provider's authorization URL, using PKCE when
// the provider supports it. The second return value is the code verifier to
// persist ("" for providers without PKCE).
func authorizeURLFor(
	ctx context.Context,
	provider auth.OAuthProvider,
	state, callbackURL string,
) (string, string, error) {
	if pkce, ok := provider.(auth.PKCEProvider); ok {
		return pkce.AuthorizeURLWithPKCE(ctx, state, callbackURL)
	}

	return provider.AuthorizeURL(state, callbackURL), "", nil
}

// exchangeCodeFor completes the code exchange, presenting the PKCE verifier
// recovered from the state row when the provider supports PKCE.
func exchangeCodeFor(
	ctx context.Context,
	provider auth.OAuthProvider,
	code, callbackURL, verifier string,
) (*auth.OAuthUser, error) {
	if pkce, ok := provider.(auth.PKCEProvider); ok {
		return pkce.ExchangeCodeWithVerifier(ctx, code, callbackURL, verifier)
	}

	return provider.ExchangeCode(ctx, code, callbackURL)
}

// pkceVerifier recovers the code verifier stashed on the state row. A row
// without usable metadata yields "", which the provider treats as "no PKCE" —
// the issuer then rejects the exchange if it expected a challenge, which is
// the correct failure mode.
func pkceVerifier(state *store.OAuthState) string {
	if state == nil || len(state.Metadata) == 0 {
		return ""
	}

	var metadata oauthStateMetadata
	if err := json.Unmarshal(state.Metadata, &metadata); err != nil {
		return ""
	}

	return metadata.PKCEVerifier
}

// handleOAuthCallback returns a handler that completes the OAuth flow.
// GET /api/v1/auth/:provider/callback
func (s *Server) handleOAuthCallback(providerName string) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := c.Request.Context()

		// 1. Check for provider-side error. The provider hands the state back
		// even when it denies the request, so burn it to recover the redirect
		// target — the user's original destination survives the failure and a
		// retry from the login page still lands there.
		if errParam := c.Query("error"); errParam != "" {
			s.logger.WarnContext(ctx, "OAuth provider returned error",
				slog.String("provider", providerName),
				slog.String("error", errParam),
				slog.String("description", c.Query("error_description")))
			s.redirectWithError(c, ErrCodeOAuthProviderError,
				s.consumeRedirectTarget(ctx, c.Query("state"), providerName))
			return
		}

		// 2. Validate and consume state (CSRF protection). Nothing is known
		// about the flow before this point, so these two branches have no
		// redirect target to forward.
		stateToken := c.Query("state")
		if stateToken == "" {
			s.redirectWithError(c, ErrCodeOAuthStateMismatch, "")
			return
		}

		oauthState, err := s.store.ConsumeOAuthState(ctx, stateToken)
		if err != nil {
			if errors.Is(err, store.ErrOAuthStateNotFound) {
				s.redirectWithError(c, ErrCodeOAuthStateMismatch, "")
				return
			}
			writeInternalError(c, s.logger, err, "failed to consume OAuth state")
			return
		}

		// The state row carries the post-login redirect target captured when
		// the flow started (e.g. the device consent page). Every failure below
		// forwards it so the login page can retry into the same destination.
		redirectTarget := oauthState.RedirectURL

		if oauthState.Provider != providerName {
			s.redirectWithError(c, ErrCodeOAuthStateMismatch, redirectTarget)
			return
		}

		// 3. Exchange authorization code for user info
		provider := s.oauthProviders[providerName]
		callbackURL := s.buildCallbackURL(c.Request, providerName)
		code := c.Query("code")

		oauthUser, err := exchangeCodeFor(ctx, provider, code, callbackURL, pkceVerifier(oauthState))
		if err != nil {
			s.logger.ErrorContext(ctx, "OAuth code exchange failed",
				slog.String("provider", providerName),
				slog.Any("error", err))
			s.redirectWithError(c, ErrCodeOAuthFailed, redirectTarget)
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
				s.redirectWithError(c, ErrCodeOAuthUserNotLinked, redirectTarget)
				return
			}
			s.redirectWithError(c, ErrCodeOAuthFailed, redirectTarget)
			return
		}

		// 5. Create web session
		_, plainKey, err := s.store.CreateWebSession(ctx, user.UID)
		if err != nil {
			writeInternalError(c, s.logger, err, "failed to create web session for OAuth user")
			return
		}

		// 6. Park the session behind a one-time exchange code. The session
		// token itself must never travel in a URL: a redirect Location lands
		// in access logs, proxy logs, browser history and Referer headers,
		// and it stays replayable for the session's whole lifetime. The code
		// is short-lived, single-use and worthless once redeemed.
		exchangeCode, err := s.createLoginExchange(ctx, user.UID, plainKey)
		if err != nil {
			writeInternalError(c, s.logger, err, "failed to create OAuth login exchange")
			return
		}

		s.logger.InfoContext(ctx, "OAuth login successful",
			slog.String("provider", providerName),
			slog.String("username", user.Username),
			slog.Any("uid", user.UID))

		// 7. Redirect to frontend with the exchange code, forwarding the
		// redirect target captured when the flow started. The login page picks
		// `redirect` up the same way it does after a password login.
		c.Redirect(http.StatusFound, s.buildLoginRedirect("code="+url.QueryEscape(exchangeCode), redirectTarget))
	}
}

// createLoginExchange mints the one-time code the browser trades for the web
// session token created by the OAuth callback. The token is AES-GCM encrypted
// and AAD-bound to the exchange row before it is stored, so a database dump on
// its own never yields a usable session.
func (s *Server) createLoginExchange(ctx context.Context, userID uuid.UUID, plainKey string) (string, error) {
	code, err := generateRandomState()
	if err != nil {
		return "", err
	}

	exchangeUID := uuid.New()

	encrypted, err := crypto.Encrypt([]byte(plainKey), s.encryptionKey, crypto.LoginExchangeAAD(exchangeUID.String()))
	if err != nil {
		return "", fmt.Errorf("encrypt login exchange token: %w", err)
	}

	if err := s.store.CreateLoginExchange(ctx, exchangeUID, code, userID, encrypted); err != nil {
		return "", err
	}

	return code, nil
}

// LoginExchangeRequest is the request body for POST /auth/oauth/exchange.
type LoginExchangeRequest struct {
	Code string `json:"code" binding:"required"`
}

// LoginExchangeResponse hands the web session token back to the SPA. Shaped
// like the device-grant token response so both login paths look the same to a
// client.
type LoginExchangeResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
}

// handleOAuthExchange redeems the one-time code the OAuth callback put in the
// login page's URL for the actual web session token. Unauthenticated by
// design — the code *is* the credential — but single-use, short-lived and IP
// rate limited like the other pre-auth endpoints.
// POST /api/v1/auth/oauth/exchange
func (s *Server) handleOAuthExchange(c *gin.Context) {
	var req LoginExchangeRequest
	if err := c.ShouldBindJSON(&req); err != nil || req.Code == "" {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, "code is required")
		return
	}

	ctx := c.Request.Context()

	exchangeUID, userID, encrypted, err := s.store.ConsumeLoginExchange(ctx, req.Code)
	if err != nil {
		if errors.Is(err, store.ErrLoginExchangeNotFound) {
			writeError(c, http.StatusUnauthorized, ErrCodeOAuthExchangeInvalid,
				"login code is invalid, already used or expired")
			return
		}
		writeInternalError(c, s.logger, err, "failed to consume OAuth login exchange")
		return
	}

	plainKey, err := crypto.Decrypt(encrypted, s.encryptionKey, crypto.LoginExchangeAAD(exchangeUID.String()))
	if err != nil {
		writeInternalError(c, s.logger, err, "failed to decrypt OAuth login exchange token")
		return
	}

	s.logger.InfoContext(ctx, "OAuth login exchange redeemed", slog.Any("uid", userID))

	c.JSON(http.StatusOK, LoginExchangeResponse{AccessToken: string(plainKey), TokenType: "Bearer"})
}

// errOAuthUserNotLinked is returned when no local user could be found or created.
var errOAuthUserNotLinked = errors.New("no linked account and auto-create disabled")

// findOrCreateOAuthUser resolves an OAuthUser to a local user.
// Resolution order:
//  1. Existing identity link (provider + provider_id)
//  2. Match by email against existing usernames
//  3. Auto-create if enabled
//
// Every path that returns an existing user runs the directory role mapping
// first (see syncOAuthRoles): roles follow group membership on **every**
// login, not only at creation, because the login that must demote someone is
// never their first one.
func (s *Server) findOrCreateOAuthUser(ctx context.Context, provider auth.OAuthProvider, oauthUser *auth.OAuthUser) (*store.User, error) {
	providerName := provider.Name()

	// 1. Check if identity is already linked
	user, err := s.store.GetUserByIdentity(ctx, providerName, oauthUser.ProviderID)
	switch {
	case err == nil:
		return s.syncOAuthRoles(ctx, user, providerName, oauthUser)
	case errors.Is(err, store.ErrIdentityNotFound):
		// fall through to email / auto-create
	case errors.Is(err, store.ErrUserNotFound):
		// Orphan identity: the linked user was soft-deleted without cascading.
		// Clean up the stale identity so the provider_id can be relinked below.
		if cleanupErr := s.cleanupOrphanIdentity(ctx, providerName, oauthUser.ProviderID); cleanupErr != nil {
			s.logger.WarnContext(ctx, "failed to clean up orphan identity",
				slog.String("provider", providerName),
				slog.String("provider_id", oauthUser.ProviderID),
				slog.Any("err", cleanupErr))
		}
	default:
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
			return s.syncOAuthRoles(ctx, user, providerName, oauthUser)
		}
		if !errors.Is(err, store.ErrUserNotFound) {
			return nil, fmt.Errorf("email lookup: %w", err)
		}
	}

	// 3. Auto-create if enabled
	if !s.oauthAutoCreateUsers() {
		return nil, errOAuthUserNotLinked
	}

	// A first login already resolves through the mapping, so a new engineer in
	// db-admins lands on admin immediately rather than on the default role
	// until they sign in a second time.
	roles := s.oauthRolesForNewUser(ctx, providerName, oauthUser)

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

	user, err = s.store.CreateUser(ctx, username, passwordHash, roles)
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
		slog.Any("roles", roles),
		slog.Any("uid", user.UID))

	s.auditOAuthUserCreated(ctx, user, providerName, oauthUser)

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

// cleanupOrphanIdentity soft-deletes a user_identity row that points to a
// soft-deleted (or otherwise missing) user, so its provider_id slot is freed.
func (s *Server) cleanupOrphanIdentity(ctx context.Context, provider, providerID string) error {
	identity, err := s.store.GetIdentityByProviderID(ctx, provider, providerID)
	if err != nil {
		return err
	}
	return s.store.DeleteUserIdentity(ctx, identity.UID)
}

// sanitizeLoginRedirect keeps only same-app absolute paths ("/grants", not
// "//evil.com" or "https://evil.com") so a redirect target riding through the
// OAuth flow can never leave the SPA — the server-side twin of the login
// page's getSafeRedirectTarget. Backslashes are rejected too, because
// browsers normalize "/\evil.com" to "//evil.com". Returns "" for anything
// unusable.
func sanitizeLoginRedirect(target string) string {
	if target == "" || len(target) > loginRedirectMaxLength {
		return ""
	}
	if !strings.HasPrefix(target, "/") || strings.HasPrefix(target, "//") {
		return ""
	}
	if strings.Contains(target, "\\") {
		return ""
	}
	return target
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

// foldAccents decomposes accented characters to base + combining mark, drops
// the marks, then recomposes — so `é` becomes `e` rather than being stripped
// by usernameRegexp. Letters that don't decompose (`ø`, `ß`, CJK, etc.) fall
// through to the regex strip and the email/`"user"` fallback path.
//
// transform.Chain returns a stateful transformer, so we build a fresh chain
// per call rather than sharing one — username generation is not a hot path.
func foldAccents(s string) string {
	t := transform.Chain(
		norm.NFD,
		runes.Remove(runes.In(unicode.Mn)),
		norm.NFC,
	)
	out, _, _ := transform.String(t, s)
	return out
}

// canonicalizeUsername produces the deterministic username base from a display
// name and an email fallback. No uniqueness check, no store access — pure
// string canonicalization, so it's safe to call from tests.
func canonicalizeUsername(displayName, email string) string {
	base := displayName
	if base == "" && email != "" {
		parts := strings.SplitN(email, "@", 2)
		base = parts[0]
	}
	if base == "" {
		base = "user"
	}

	base = strings.ToLower(base)
	base = foldAccents(base)
	base = strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return '.'
		}
		return r
	}, base)
	base = usernameRegexp.ReplaceAllString(base, "")

	if len(base) > 30 {
		base = base[:30]
	}
	if !hasUsernameContent(base) {
		base = "user"
	}
	return base
}

// hasUsernameContent reports whether s has at least one alphanumeric byte.
// Catches all-punctuation results like "..." that come from dot-mapping a
// whitespace-only display name — they would otherwise sneak past the empty
// check and land in the user list as gibberish.
func hasUsernameContent(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			return true
		}
	}
	return false
}

// generateUniqueUsername creates a username from a display name or email.
// It sanitizes the input and appends a short suffix if the name is already taken.
func (s *Server) generateUniqueUsername(ctx context.Context, displayName, email string) string {
	base := canonicalizeUsername(displayName, email)

	if _, err := s.store.GetUserByUsername(ctx, base); errors.Is(err, store.ErrUserNotFound) {
		return base
	}

	suffix := make([]byte, 4)
	_, _ = rand.Read(suffix)
	return base + "." + hex.EncodeToString(suffix)
}

// redirectWithError redirects the user to the login page with an error code,
// keeping the post-login redirect target alive so a retry from the login page
// still lands where the user started. Pass "" when the failure happened before
// the state row was read and no target is known.
func (s *Server) redirectWithError(c *gin.Context, code ErrorCode, redirectTarget string) {
	c.Redirect(http.StatusFound, s.buildLoginRedirect("error="+string(code), redirectTarget))
}

// buildLoginRedirect assembles a `/login?<query>[&redirect=...]` URL on the
// configured base path. It is the single place the redirect target is written
// out, and it re-sanitizes on the way — no caller, and no value read back from
// the `oauth_states` column, can turn this into an open redirect.
func (s *Server) buildLoginRedirect(query, redirectTarget string) string {
	baseURL := "/app"
	if s.config != nil && s.config.BaseURL != "" {
		baseURL = s.config.BaseURL
	}

	target := baseURL + "/login?" + query
	if safe := sanitizeLoginRedirect(redirectTarget); safe != "" {
		target += "&redirect=" + url.QueryEscape(safe)
	}
	return target
}

// consumeRedirectTarget best-effort recovers the post-login redirect target
// from a pending state row. Used by the provider-error branch, which runs
// before the regular state validation: any problem (absent, expired, unknown
// or foreign state) just means no target to forward, never a failed login
// redirect.
func (s *Server) consumeRedirectTarget(ctx context.Context, stateToken, providerName string) string {
	if stateToken == "" {
		return ""
	}

	oauthState, err := s.store.ConsumeOAuthState(ctx, stateToken)
	if err != nil || oauthState.Provider != providerName {
		return ""
	}
	return oauthState.RedirectURL
}
