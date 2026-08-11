package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/auth"
	"github.com/fclairamb/dbbat/internal/auth/oidc"
	"github.com/fclairamb/dbbat/internal/config"
	"github.com/fclairamb/dbbat/internal/store"
)

// mockProvider is a minimal auth.OAuthProvider for tests.
type mockProvider struct{ name string }

func (m *mockProvider) Name() string                    { return m.name }
func (m *mockProvider) AuthorizeURL(_, _ string) string { return "" }
func (m *mockProvider) ExchangeCode(_ context.Context, _, _ string) (*auth.OAuthUser, error) {
	return &auth.OAuthUser{}, nil
}

// recordingProvider captures the state passed to AuthorizeURL so a test can
// replay it against the callback, closing the loop without a real provider.
type recordingProvider struct {
	name      string
	lastState string
	user      *auth.OAuthUser
	// exchangeErr, when set, makes ExchangeCode fail — the "code exchange
	// blew up" branch of the callback.
	exchangeErr error
}

func (p *recordingProvider) Name() string { return p.name }
func (p *recordingProvider) AuthorizeURL(state, _ string) string {
	p.lastState = state
	return "https://provider.example/authorize?state=" + state
}

func (p *recordingProvider) ExchangeCode(_ context.Context, _, _ string) (*auth.OAuthUser, error) {
	if p.exchangeErr != nil {
		return nil, p.exchangeErr
	}
	return p.user, nil
}

// pkceRecordingProvider is a second registered provider that exercises the
// auth.PKCEProvider path: it mints a verifier at authorize time and records
// whatever verifier the callback hands back, so a test can prove the value
// survived the round trip through the oauth_states row. It also implements
// auth.DisplayNamer, like the real generic OIDC provider.
type pkceRecordingProvider struct {
	name        string
	displayName string
	user        *auth.OAuthUser

	mu               sync.Mutex
	lastState        string
	mintedVerifier   string
	receivedVerifier string
}

func (p *pkceRecordingProvider) Name() string        { return p.name }
func (p *pkceRecordingProvider) DisplayName() string { return p.displayName }

func (p *pkceRecordingProvider) AuthorizeURL(state, _ string) string {
	return "https://provider.example/authorize?state=" + state
}

func (p *pkceRecordingProvider) ExchangeCode(_ context.Context, _, _ string) (*auth.OAuthUser, error) {
	return p.user, nil
}

func (p *pkceRecordingProvider) AuthorizeURLWithPKCE(
	_ context.Context,
	state, _ string,
) (string, string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.lastState = state
	p.mintedVerifier = "verifier-for-" + state

	return "https://provider.example/authorize?state=" + state + "&code_challenge_method=S256", p.mintedVerifier, nil
}

func (p *pkceRecordingProvider) ExchangeCodeWithVerifier(
	_ context.Context,
	_, _, verifier string,
) (*auth.OAuthUser, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.receivedVerifier = verifier

	return p.user, nil
}

// TestOAuthPKCEVerifierSurvivesTheRoundTrip pins the storage half of PKCE: the
// verifier is minted before the redirect, parked on the oauth_states row, and
// handed back at the callback. It must never travel through the browser, and
// it must survive a callback that lands on a different process — which is why
// it lives in the database and not in memory.
func TestOAuthPKCEVerifierSurvivesTheRoundTrip(t *testing.T) {
	t.Parallel()

	server, _ := setupTestServer(t)
	server.encryptionKey = dbTestEncryptionKey
	suffix := uuid.NewString()[:8]

	server.config = &config.Config{
		Auth: config.OAuthUsersConfig{
			AutoCreateUsers: true,
			DefaultRole:     store.RoleConnector,
		},
	}

	providerName := "oidc-" + suffix
	provider := &pkceRecordingProvider{
		name:        providerName,
		displayName: "Acme SSO",
		user: &auth.OAuthUser{
			ProviderID:  "OIDC-" + suffix,
			Email:       "oidc-" + suffix + "@example.com",
			DisplayName: "OIDC User " + suffix,
		},
	}
	server.oauthProviders[providerName] = provider

	router := gin.New()
	router.GET("/api/v1/auth/"+providerName, server.handleOAuthAuthorize(providerName))
	router.GET("/api/v1/auth/"+providerName+"/callback", server.handleOAuthCallback(providerName))

	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/auth/"+providerName, nil))
	require.Equal(t, http.StatusFound, w.Code)

	provider.mu.Lock()
	state, minted := provider.lastState, provider.mintedVerifier
	provider.mu.Unlock()

	require.NotEmpty(t, state)
	require.NotEmpty(t, minted)
	assert.NotContains(t, w.Header().Get("Location"), minted,
		"the code verifier must never leave the server")

	w = httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet,
		"/api/v1/auth/"+providerName+"/callback?code=x&state="+state, nil))
	require.Equal(t, http.StatusFound, w.Code)

	provider.mu.Lock()
	received := provider.receivedVerifier
	provider.mu.Unlock()

	assert.Equal(t, minted, received,
		"the callback must present the verifier stored with the state row")

	loc, err := url.Parse(w.Header().Get("Location"))
	require.NoError(t, err)
	assert.Equal(t, "/app/login", loc.Path)
	assert.NotEmpty(t, loc.Query().Get("code"), "a PKCE login must still end in an exchange code")
	assert.Empty(t, loc.Query().Get("error"))
}

// TestAuthProvidersExposesDisplayName covers the login page's discovery
// endpoint with two OAuth providers registered: the generic one carries the
// operator-configured button label, the branded one does not.
func TestAuthProvidersExposesDisplayName(t *testing.T) {
	t.Parallel()

	server, _ := setupTestServer(t)
	suffix := uuid.NewString()[:8]

	genericName := "oidc-list-" + suffix
	brandedName := "slack-list-" + suffix

	server.oauthProviders[genericName] = &pkceRecordingProvider{name: genericName, displayName: "Acme SSO"}
	server.oauthProviders[brandedName] = &mockProvider{name: brandedName}

	router := gin.New()
	router.GET("/api/v1/auth/providers", server.handleAuthProviders)

	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/auth/providers", nil))
	require.Equal(t, http.StatusOK, w.Code)

	var body struct {
		Providers []authProviderInfo `json:"providers"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))

	byType := make(map[string]authProviderInfo, len(body.Providers))
	for _, p := range body.Providers {
		byType[p.Type] = p
	}

	require.Contains(t, byType, "password")
	require.Contains(t, byType, genericName)
	require.Contains(t, byType, brandedName)

	assert.Equal(t, "Acme SSO", byType[genericName].DisplayName)
	assert.Equal(t, "/api/v1/auth/"+genericName, byType[genericName].AuthorizeURL)
	assert.Empty(t, byType[brandedName].DisplayName,
		"a provider the frontend labels itself must not carry a display name")
}

// authProvidersRoleMapping drives GET /auth/providers and returns the
// role_mapping block plus the raw body, so a test can also assert on what is
// *not* in it.
func authProvidersRoleMapping(t *testing.T, server *Server) (roleMappingInfo, string) {
	t.Helper()

	router := gin.New()
	router.GET("/api/v1/auth/providers", server.handleAuthProviders)

	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/auth/providers", nil))
	require.Equal(t, http.StatusOK, w.Code)

	var body struct {
		RoleMapping roleMappingInfo `json:"role_mapping"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))

	return body.RoleMapping, w.Body.String()
}

// TestAuthProvidersExposesManagedRoles is what lets the users page badge a
// role as directory-owned: the endpoint names the roles the mapping governs,
// and nothing else about it.
func TestAuthProvidersExposesManagedRoles(t *testing.T) {
	t.Parallel()

	server, _ := setupTestServer(t)
	serverWithRoleMapping(t, server, "viewer=analysts,admin=db-admins")
	server.oauthProviders[oidc.ProviderName] = &mockProvider{name: oidc.ProviderName}

	mapping, raw := authProvidersRoleMapping(t, server)

	assert.True(t, mapping.Enabled)
	assert.Equal(t, []string{store.RoleAdmin, store.RoleViewer}, mapping.Roles,
		"the managed roles must come back sorted, whatever order they were configured in")
	assert.Equal(t, oidc.ProviderName, mapping.Provider)

	// The endpoint is unauthenticated: directory group names are topology and
	// must never travel through it.
	assert.NotContains(t, raw, "db-admins")
	assert.NotContains(t, raw, "analysts")
}

// TestAuthProvidersRoleMappingNeedsTheProvider covers a mapping configured
// while the OIDC provider is not registered. Nothing applies it, so the UI
// must not warn that anything is managed.
func TestAuthProvidersRoleMappingNeedsTheProvider(t *testing.T) {
	t.Parallel()

	server, _ := setupTestServer(t)
	serverWithRoleMapping(t, server, "admin=db-admins")

	mapping, _ := authProvidersRoleMapping(t, server)

	assert.False(t, mapping.Enabled)
	assert.Empty(t, mapping.Roles)
	assert.Empty(t, mapping.Provider)
}

// TestAuthProvidersRoleMappingUnset is the default deployment: SSO may well be
// on, but nobody mapped a group to a role, so every role stays manual.
func TestAuthProvidersRoleMappingUnset(t *testing.T) {
	t.Parallel()

	server, _ := setupTestServer(t)
	serverWithRoleMapping(t, server, "")
	server.oauthProviders[oidc.ProviderName] = &mockProvider{name: oidc.ProviderName}

	mapping, _ := authProvidersRoleMapping(t, server)

	assert.False(t, mapping.Enabled)
	assert.Empty(t, mapping.Roles)
}

func TestFindOrCreateOAuthUser_OrphanIdentity(t *testing.T) {
	t.Parallel()

	server, dataStore := setupTestServer(t)
	ctx := context.Background()
	suffix := uuid.NewString()[:8]

	server.config = &config.Config{
		Auth: config.OAuthUsersConfig{
			AutoCreateUsers: true,
			DefaultRole:     store.RoleConnector,
		},
	}

	provider := &mockProvider{name: store.IdentityTypeSlack}
	providerID := "UORPHAN-" + suffix

	// Create a user, link a Slack identity, then delete the user (without the fix
	// this would leave an orphan identity pointing to a soft-deleted user).
	user, err := dataStore.CreateUser(ctx, "orphan-user-"+suffix, "hash", []string{store.RoleConnector})
	require.NoError(t, err)

	_, err = dataStore.CreateUserIdentity(ctx, &store.UserIdentity{
		UserID:     user.UID,
		Provider:   store.IdentityTypeSlack,
		ProviderID: providerID,
	})
	require.NoError(t, err)

	require.NoError(t, dataStore.DeleteUser(ctx, user.UID))

	// After DeleteUser the identity must already be gone (cascade fix).
	// But even if it were orphaned (pre-fix DBs), findOrCreateOAuthUser must recover.
	oauthUser := &auth.OAuthUser{
		ProviderID:  providerID,
		Email:       "orphan-" + suffix + "@example.com",
		DisplayName: "Orphan User " + suffix,
	}

	newUser, err := server.findOrCreateOAuthUser(ctx, provider, oauthUser)
	require.NoError(t, err, "findOrCreateOAuthUser must succeed after user deletion")
	assert.NotEqual(t, user.UID, newUser.UID, "a new user must be created")
}

// TestFindOrCreateOAuthUserPerProviderAutoCreate is the feature seen from the
// login path: two providers, one allowed to mint accounts and one not, in the
// same process. Before the per-provider override the stricter policy applied to
// both, which made the looser provider unusable.
func TestFindOrCreateOAuthUserPerProviderAutoCreate(t *testing.T) {
	t.Parallel()

	server, dataStore := setupTestServer(t)
	ctx := context.Background()
	suffix := uuid.NewString()[:8]

	gated := false
	server.config = &config.Config{
		Auth: config.OAuthUsersConfig{
			AutoCreateUsers: true,
			DefaultRole:     store.RoleConnector,
			Providers: map[string]config.OAuthProviderUsersConfig{
				// Slack: a workspace full of contractors — accounts by hand only.
				store.IdentityTypeSlack: {AutoCreateUsers: &gated},
				// The corporate issuer keeps the instance-wide "yes", on its own role.
				oidc.ProviderName: {DefaultRole: store.RoleViewer},
			},
		},
	}
	require.NoError(t, server.config.Auth.Validate())

	slackUser := &auth.OAuthUser{
		ProviderID:  "USLACKGATED-" + suffix,
		Email:       "slack-" + suffix + "@example.com",
		DisplayName: "Slack User " + suffix,
	}

	_, err := server.findOrCreateOAuthUser(ctx, &mockProvider{name: store.IdentityTypeSlack}, slackUser)
	require.ErrorIs(t, err, errOAuthUserNotLinked,
		"the gated provider must not provision an account despite the instance-wide true")

	// The same identity through the other provider still gets an account, on
	// that provider's own default role.
	created, err := server.findOrCreateOAuthUser(ctx, &mockProvider{name: oidc.ProviderName}, &auth.OAuthUser{
		ProviderID:  "OIDCALLOWED-" + suffix,
		Email:       "oidc-" + suffix + "@example.com",
		DisplayName: "OIDC User " + suffix,
	})
	require.NoError(t, err, "a provider with no auto-create override keeps the instance-wide yes")
	assert.Equal(t, []string{store.RoleViewer}, []string(created.Roles),
		"the per-provider default role must apply to the account it creates")

	// Gating creation must not gate *login*: an account an admin created by
	// hand is exactly what the deployment wants the Slack provider used for.
	manual, err := dataStore.CreateUser(ctx, slackUser.Email, "hash", []string{store.RoleConnector})
	require.NoError(t, err)

	linked, err := server.findOrCreateOAuthUser(ctx, &mockProvider{name: store.IdentityTypeSlack}, slackUser)
	require.NoError(t, err, "an existing account must still sign in through the gated provider")
	assert.Equal(t, manual.UID, linked.UID)
}

// errTestExchangeFailed stands in for a provider token endpoint refusing the
// authorization code.
var errTestExchangeFailed = errors.New("token endpoint said no")

// callbackOutcome selects which branch of the OAuth callback a round-trip case
// exercises.
type callbackOutcome int

const (
	outcomeSuccess       callbackOutcome = iota // code exchange works, session created
	outcomeExchangeError                        // provider.ExchangeCode fails
	outcomeProviderError                        // provider bounces back ?error=access_denied
)

// TestOAuthLoginRedirectRoundTrip covers the regression where a user bounced
// to login from a protected page (typically the device consent page) lost that
// destination when signing in through OAuth: the redirect target must ride the
// state row from /auth/slack to the callback's /login redirect — on the happy
// path *and* on every failure, since the login page drops the error param on
// mount and a retry would otherwise fall back to "/". Unsafe targets must be
// dropped, not forwarded, on both paths.
func TestOAuthLoginRedirectRoundTrip(t *testing.T) {
	t.Parallel()

	server, _ := setupTestServer(t)
	suffix := uuid.NewString()[:8]

	// The callback encrypts the web session token into the login exchange row.
	server.encryptionKey = dbTestEncryptionKey

	server.config = &config.Config{
		Auth: config.OAuthUsersConfig{
			AutoCreateUsers: true,
			DefaultRole:     store.RoleConnector,
		},
	}

	tests := []struct {
		name         string
		redirect     string
		outcome      callbackOutcome
		wantRedirect string // expected redirect param on /login; "" = absent
		wantError    string // expected error param; "" = expect a token instead
	}{
		{
			name:         "same-app path survives the round trip",
			redirect:     "/device?user_code=WDJP-4KXR",
			wantRedirect: "/device?user_code=WDJP-4KXR",
		},
		{
			name:     "absolute URL is dropped",
			redirect: "https://evil.example/phish",
		},
		{
			name:     "protocol-relative URL is dropped",
			redirect: "//evil.example/phish",
		},
		{
			name: "no redirect param",
		},
		{
			name:         "code exchange failure keeps the target",
			redirect:     "/device?user_code=WDJP-4KXR",
			outcome:      outcomeExchangeError,
			wantRedirect: "/device?user_code=WDJP-4KXR",
			wantError:    string(ErrCodeOAuthFailed),
		},
		{
			name:      "code exchange failure drops an absolute URL",
			redirect:  "https://evil.example/phish",
			outcome:   outcomeExchangeError,
			wantError: string(ErrCodeOAuthFailed),
		},
		{
			name:      "code exchange failure drops a protocol-relative URL",
			redirect:  "//evil.example/phish",
			outcome:   outcomeExchangeError,
			wantError: string(ErrCodeOAuthFailed),
		},
		{
			name:         "provider error keeps the target",
			redirect:     "/grants",
			outcome:      outcomeProviderError,
			wantRedirect: "/grants",
			wantError:    string(ErrCodeOAuthProviderError),
		},
		{
			name:      "provider error drops an absolute URL",
			redirect:  "https://evil.example/phish",
			outcome:   outcomeProviderError,
			wantError: string(ErrCodeOAuthProviderError),
		},
	}

	// One provider (and route pair) per case: recordingProvider.lastState is
	// written during the authorize call, so parallel subtests must not share
	// a provider.
	router := gin.New()
	providers := make([]*recordingProvider, len(tests))
	for i := range tests {
		providerName := fmt.Sprintf("slack%d", i)
		providers[i] = &recordingProvider{
			name: providerName,
			user: &auth.OAuthUser{
				ProviderID:  fmt.Sprintf("UREDIR-%d-%s", i, suffix),
				Email:       fmt.Sprintf("redir-%d-%s@example.com", i, suffix),
				DisplayName: fmt.Sprintf("Redir User %d %s", i, suffix),
			},
		}
		if tests[i].outcome == outcomeExchangeError {
			providers[i].exchangeErr = errTestExchangeFailed
		}
		server.oauthProviders[providerName] = providers[i]
		router.GET("/api/v1/auth/"+providerName, server.handleOAuthAuthorize(providerName))
		router.GET("/api/v1/auth/"+providerName+"/callback", server.handleOAuthCallback(providerName))
	}

	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			provider := providers[i]
			authorizeTarget := "/api/v1/auth/" + provider.name
			if tt.redirect != "" {
				authorizeTarget += "?redirect=" + url.QueryEscape(tt.redirect)
			}
			w := httptest.NewRecorder()
			router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, authorizeTarget, nil))
			require.Equal(t, http.StatusFound, w.Code)
			require.NotEmpty(t, provider.lastState)

			callbackTarget := "/api/v1/auth/" + provider.name + "/callback?state=" + provider.lastState
			if tt.outcome == outcomeProviderError {
				callbackTarget += "&error=access_denied&error_description=user+said+no"
			} else {
				callbackTarget += "&code=x"
			}

			w = httptest.NewRecorder()
			router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, callbackTarget, nil))
			require.Equal(t, http.StatusFound, w.Code)

			location := w.Header().Get("Location")
			loc, err := url.Parse(location)
			require.NoError(t, err)
			assert.Equal(t, "/app/login", loc.Path)
			assert.Empty(t, loc.Host, "the login redirect must stay same-origin")
			assert.NotContains(t, location, "evil.example",
				"a hostile redirect target must never be echoed back")

			// The callback hands back a one-time exchange code, never a
			// session token: nothing in the Location may look like a `web_`
			// credential.
			assert.NotContains(t, location, store.WebKeyPrefix,
				"the session token must never appear in the redirect URL")
			assert.Empty(t, loc.Query().Get("token"),
				"the legacy ?token= handoff must be gone")

			if tt.wantError == "" {
				assert.NotEmpty(t, loc.Query().Get("code"))
				assert.Empty(t, loc.Query().Get("error"))
			} else {
				assert.Equal(t, tt.wantError, loc.Query().Get("error"))
				assert.Empty(t, loc.Query().Get("code"))
			}
			assert.Equal(t, tt.wantRedirect, loc.Query().Get("redirect"))
		})
	}
}

// TestOAuthCallbackErrorRedirects covers the remaining callback failures: the
// unlinked-user branch (which needs auto-create off, hence its own server) must
// keep the redirect target, while the branches that fail before the state row
// is read have nothing to forward.
func TestOAuthCallbackErrorRedirects(t *testing.T) {
	t.Parallel()

	server, _ := setupTestServer(t)
	suffix := uuid.NewString()[:8]

	// Auto-create off and no matching local user: findOrCreateOAuthUser bails
	// out with errOAuthUserNotLinked.
	server.config = &config.Config{Auth: config.OAuthUsersConfig{AutoCreateUsers: false}}

	providerName := "slack-nolink-" + suffix
	provider := &recordingProvider{
		name: providerName,
		user: &auth.OAuthUser{
			ProviderID:  "UNOLINK-" + suffix,
			Email:       "nolink-" + suffix + "@example.com",
			DisplayName: "No Link " + suffix,
		},
	}
	server.oauthProviders[providerName] = provider

	router := gin.New()
	router.GET("/api/v1/auth/"+providerName, server.handleOAuthAuthorize(providerName))
	router.GET("/api/v1/auth/"+providerName+"/callback", server.handleOAuthCallback(providerName))

	get := func(target string) *url.URL {
		t.Helper()
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, target, nil))
		require.Equal(t, http.StatusFound, w.Code)
		loc, err := url.Parse(w.Header().Get("Location"))
		require.NoError(t, err)
		return loc
	}

	// Kept sequential rather than split into parallel subtests: every case
	// shares one provider, whose lastState is written by the authorize call.

	// Unlinked user: the target captured at authorize time must survive.
	get("/api/v1/auth/" + providerName + "?redirect=" + url.QueryEscape("/device?user_code=WDJP-4KXR"))
	require.NotEmpty(t, provider.lastState)

	loc := get("/api/v1/auth/" + providerName + "/callback?code=x&state=" + provider.lastState)
	assert.Equal(t, "/app/login", loc.Path)
	assert.Equal(t, string(ErrCodeOAuthUserNotLinked), loc.Query().Get("error"), "unlinked user")
	assert.Equal(t, "/device?user_code=WDJP-4KXR", loc.Query().Get("redirect"), "unlinked user keeps the target")

	// Unknown state: nothing to forward.
	loc = get("/api/v1/auth/" + providerName + "/callback?code=x&state=deadbeef" + suffix)
	assert.Equal(t, string(ErrCodeOAuthStateMismatch), loc.Query().Get("error"), "unknown state")
	assert.Empty(t, loc.Query().Get("redirect"), "unknown state has nothing to forward")

	// Missing state: same.
	loc = get("/api/v1/auth/" + providerName + "/callback?code=x")
	assert.Equal(t, string(ErrCodeOAuthStateMismatch), loc.Query().Get("error"), "missing state")
	assert.Empty(t, loc.Query().Get("redirect"), "missing state has nothing to forward")

	// Provider error without a state: still a clean login redirect, no target.
	loc = get("/api/v1/auth/" + providerName + "/callback?error=access_denied")
	assert.Equal(t, string(ErrCodeOAuthProviderError), loc.Query().Get("error"), "stateless provider error")
	assert.Empty(t, loc.Query().Get("redirect"), "stateless provider error has nothing to forward")
}

// TestOAuthLoginExchange drives the full handoff the way a browser does: the
// callback puts a one-time code in the URL, the SPA trades it for the session
// token, and the token is a real, usable web session. It also pins the
// single-use property — the replay that a leaked access log would enable must
// fail.
func TestOAuthLoginExchange(t *testing.T) {
	t.Parallel()

	server, dataStore := setupTestServer(t)
	server.encryptionKey = dbTestEncryptionKey
	suffix := uuid.NewString()[:8]

	server.config = &config.Config{
		Auth: config.OAuthUsersConfig{
			AutoCreateUsers: true,
			DefaultRole:     store.RoleConnector,
		},
	}

	providerName := "slack-exchange-" + suffix
	provider := &recordingProvider{
		name: providerName,
		user: &auth.OAuthUser{
			ProviderID:  "UEXCHANGE-" + suffix,
			Email:       "exchange-" + suffix + "@example.com",
			DisplayName: "Exchange User " + suffix,
		},
	}
	server.oauthProviders[providerName] = provider

	router := gin.New()
	router.GET("/api/v1/auth/"+providerName, server.handleOAuthAuthorize(providerName))
	router.GET("/api/v1/auth/"+providerName+"/callback", server.handleOAuthCallback(providerName))
	router.POST("/api/v1/auth/oauth/exchange", server.handleOAuthExchange)

	exchange := func(code string) *httptest.ResponseRecorder {
		body, err := json.Marshal(map[string]string{"code": code})
		require.NoError(t, err)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/oauth/exchange", strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}

	// Walk the flow: authorize, then callback.
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/auth/"+providerName, nil))
	require.Equal(t, http.StatusFound, w.Code)
	require.NotEmpty(t, provider.lastState)

	w = httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet,
		"/api/v1/auth/"+providerName+"/callback?code=x&state="+provider.lastState, nil))
	require.Equal(t, http.StatusFound, w.Code)

	loc, err := url.Parse(w.Header().Get("Location"))
	require.NoError(t, err)
	exchangeCode := loc.Query().Get("code")
	require.NotEmpty(t, exchangeCode, "the callback must hand back an exchange code")
	require.NotContains(t, exchangeCode, store.WebKeyPrefix,
		"the exchange code must not be the session token in disguise")

	// Redeeming it yields a real web session token.
	w = exchange(exchangeCode)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var resp LoginExchangeResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "Bearer", resp.TokenType)
	require.True(t, strings.HasPrefix(resp.AccessToken, store.WebKeyPrefix),
		"the exchange must return a web session token, got %q", resp.AccessToken)

	apiKey, err := dataStore.VerifyAPIKey(context.Background(), resp.AccessToken)
	require.NoError(t, err, "the exchanged token must authenticate")
	assert.True(t, apiKey.IsWebSession())

	linked, err := dataStore.GetUserByIdentity(context.Background(), providerName, provider.user.ProviderID)
	require.NoError(t, err)
	assert.Equal(t, linked.UID, apiKey.UserID,
		"the session must belong to the user who just signed in")

	// Replay: the code is burned.
	w = exchange(exchangeCode)
	assert.Equal(t, http.StatusUnauthorized, w.Code, "an exchange code must be single-use")

	// Unknown code: same answer, no existence leak.
	w = exchange("deadbeef" + suffix)
	assert.Equal(t, http.StatusUnauthorized, w.Code)

	// Missing code: a validation error, not a 500.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/oauth/exchange", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// TestBuildLoginRedirectSanitizes pins the chokepoint: even a hostile value
// read straight back from the (unconstrained) oauth_states column must not
// make it into the Location header.
func TestBuildLoginRedirectSanitizes(t *testing.T) {
	t.Parallel()

	server := &Server{}

	tests := []struct {
		name     string
		query    string
		redirect string
		want     string
	}{
		{
			name:     "safe target is forwarded and escaped",
			query:    "error=OAUTH_FAILED",
			redirect: "/device?user_code=WDJP-4KXR",
			want:     "/app/login?error=OAUTH_FAILED&redirect=%2Fdevice%3Fuser_code%3DWDJP-4KXR",
		},
		{
			name:     "absolute URL is dropped",
			query:    "error=OAUTH_FAILED",
			redirect: "https://evil.example/",
			want:     "/app/login?error=OAUTH_FAILED",
		},
		{
			name:     "protocol-relative URL is dropped",
			query:    "error=OAUTH_FAILED",
			redirect: "//evil.example",
			want:     "/app/login?error=OAUTH_FAILED",
		},
		{
			name:     "backslash trickery is dropped",
			query:    "token=abc",
			redirect: "/\\evil.example",
			want:     "/app/login?token=abc",
		},
		{
			name:  "no target",
			query: "error=OAUTH_STATE_MISMATCH",
			want:  "/app/login?error=OAUTH_STATE_MISMATCH",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, server.buildLoginRedirect(tt.query, tt.redirect))
		})
	}
}

func TestSanitizeLoginRedirect(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		target string
		want   string
	}{
		{name: "app path with query", target: "/device?user_code=WDJP-4KXR", want: "/device?user_code=WDJP-4KXR"},
		{name: "root", target: "/", want: "/"},
		{name: "empty", target: "", want: ""},
		{name: "relative path", target: "device", want: ""},
		{name: "protocol-relative", target: "//evil.example", want: ""},
		{name: "absolute URL", target: "https://evil.example", want: ""},
		{name: "backslash trickery", target: "/\\evil.example", want: ""},
		{name: "over length cap", target: "/" + strings.Repeat("a", loginRedirectMaxLength), want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, sanitizeLoginRedirect(tt.target))
		})
	}
}

func TestCanonicalizeUsername(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		displayName string
		email       string
		want        string
	}{
		// Acceptance criteria from the spec.
		{
			name:        "single accent in display name",
			displayName: "mélanie.samedi",
			want:        "melanie.samedi",
		},
		{
			name:        "mixed accents and capitals",
			displayName: "José.García",
			want:        "jose.garcia",
		},
		{
			name:        "space and accent",
			displayName: "François Müller",
			want:        "francois.muller",
		},
		{
			name:        "pure ASCII passes through unchanged",
			displayName: "alice.smith",
			want:        "alice.smith",
		},

		// Edge cases.
		{
			name:        "empty display name falls back to user",
			displayName: "",
			email:       "",
			want:        "user",
		},
		{
			name:        "whitespace-only display name falls back to user",
			displayName: "   ",
			email:       "",
			want:        "user",
		},
		{
			name:        "email fallback when display empty",
			displayName: "",
			email:       "alice@example.com",
			want:        "alice",
		},
		{
			name:        "email fallback folds accents in local part",
			displayName: "",
			email:       "josé@example.com",
			want:        "jose",
		},

		// Non-Latin scripts should fall through to the regex strip and the
		// fallback path (no panic, no empty username escaping). The CJK cases
		// are present intentionally to exercise the fallback path.
		{
			name: "CJK display name with email fallback",
			//nolint:gosmopolitan // testing CJK fallback is the point
			displayName: "山田太郎",
			email:       "yamada@example.com",
			// CJK strips to empty → falls back to "user" in canonicalize.
			// Note: when display is non-empty, email is *not* tried in this function.
			want: "user",
		},
		{
			name:        "Cyrillic display name with email fallback",
			displayName: "Иван",
			email:       "ivan@example.com",
			want:        "user",
		},
		{
			name: "all non-Latin and no email at all",
			//nolint:gosmopolitan // testing CJK fallback is the point
			displayName: "山田",
			email:       "",
			want:        "user",
		},

		// Length cap (30 chars).
		{
			name:        "long name truncates at 30",
			displayName: strings.Repeat("a", 50),
			want:        strings.Repeat("a", 30),
		},

		// Mixed content keeps allowed characters.
		{
			name:        "dots and dashes preserved",
			displayName: "alice-smith.jr",
			want:        "alice-smith.jr",
		},
		{
			name:        "underscore preserved",
			displayName: "alice_smith",
			want:        "alice_smith",
		},

		// Letters that don't decompose under NFD: ø, ß, æ, ł — these strip out.
		// The fallback path covers the all-non-decomposable case.
		{
			name:        "non-decomposable letters strip out",
			displayName: "Bjørn",
			want:        "bjrn", // ø strips, b/j/r/n remain
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := canonicalizeUsername(tt.displayName, tt.email)
			if got != tt.want {
				t.Errorf("canonicalizeUsername(%q, %q) = %q, want %q",
					tt.displayName, tt.email, got, tt.want)
			}
		})
	}
}
