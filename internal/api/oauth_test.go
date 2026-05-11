package api

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/auth"
	"github.com/fclairamb/dbbat/internal/config"
	"github.com/fclairamb/dbbat/internal/store"
)

// mockProvider is a minimal auth.OAuthProvider for tests.
type mockProvider struct{ name string }

func (m *mockProvider) Name() string { return m.name }
func (m *mockProvider) AuthorizeURL(_, _ string) string { return "" }
func (m *mockProvider) ExchangeCode(_ context.Context, _, _ string) (*auth.OAuthUser, error) {
	return &auth.OAuthUser{}, nil
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
