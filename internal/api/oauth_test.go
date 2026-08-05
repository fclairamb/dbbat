package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/auth"
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
}

func (p *recordingProvider) Name() string { return p.name }
func (p *recordingProvider) AuthorizeURL(state, _ string) string {
	p.lastState = state
	return "https://provider.example/authorize?state=" + state
}

func (p *recordingProvider) ExchangeCode(_ context.Context, _, _ string) (*auth.OAuthUser, error) {
	return p.user, nil
}

func TestFindOrCreateOAuthUser_OrphanIdentity(t *testing.T) {
	t.Parallel()

	server, dataStore := setupTestServer(t)
	ctx := context.Background()
	suffix := uuid.NewString()[:8]

	server.config = &config.Config{
		SlackAuth: config.SlackAuthConfig{
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

// TestOAuthLoginRedirectRoundTrip covers the regression where a user bounced
// to login from a protected page (typically the device consent page) lost that
// destination when signing in through OAuth: the redirect target must ride the
// state row from /auth/slack to the callback's /login?token=... redirect —
// and unsafe targets must be dropped, not forwarded.
func TestOAuthLoginRedirectRoundTrip(t *testing.T) {
	t.Parallel()

	server, _ := setupTestServer(t)
	suffix := uuid.NewString()[:8]

	server.config = &config.Config{
		SlackAuth: config.SlackAuthConfig{
			AutoCreateUsers: true,
			DefaultRole:     store.RoleConnector,
		},
	}

	tests := []struct {
		name         string
		redirect     string
		wantRedirect string // expected redirect param on /login; "" = absent
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

			w = httptest.NewRecorder()
			router.ServeHTTP(w, httptest.NewRequest(http.MethodGet,
				"/api/v1/auth/"+provider.name+"/callback?code=x&state="+provider.lastState, nil))
			require.Equal(t, http.StatusFound, w.Code)

			loc, err := url.Parse(w.Header().Get("Location"))
			require.NoError(t, err)
			assert.Equal(t, "/app/login", loc.Path)
			assert.NotEmpty(t, loc.Query().Get("token"))
			assert.Equal(t, tt.wantRedirect, loc.Query().Get("redirect"))
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
