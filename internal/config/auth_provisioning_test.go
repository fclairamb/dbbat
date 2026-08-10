package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setAuthBaseEnv sets the two variables every Load() needs, so each case below
// only says what it is actually testing.
func setAuthBaseEnv(t *testing.T) {
	t.Helper()
	t.Setenv("DBB_DSN", "postgres://x:x@localhost/x")
	t.Setenv("DBB_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
}

// TestAuthProvisioningDefaults pins the "neither name set" case: auto-creation
// on, default role connector — exactly what the Slack-shaped settings did
// before they moved, so an upgrade changes nothing.
func TestAuthProvisioningDefaults(t *testing.T) {
	// t.Setenv inline rather than through setAuthBaseEnv: it is what tells the
	// linter this test cannot run in parallel.
	t.Setenv("DBB_DSN", "postgres://x:x@localhost/x")
	t.Setenv("DBB_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")

	cfg, err := Load(LoadOptions{})
	require.NoError(t, err)

	assert.True(t, cfg.Auth.AutoCreateUsers, "auto-provisioning is on by default")
	assert.Equal(t, DefaultOAuthRole, cfg.Auth.Role())
}

// TestAuthProvisioningCanonicalEnv verifies the canonical names land on
// OAuthUsersConfig through the auth_* koanf prefix rule — and that the rule
// does not swallow DBB_AUTH_CACHE_*, which is tested next to it on purpose.
func TestAuthProvisioningCanonicalEnv(t *testing.T) {
	setAuthBaseEnv(t)
	t.Setenv("DBB_AUTH_AUTO_CREATE_USERS", "false")
	t.Setenv("DBB_AUTH_DEFAULT_ROLE", "viewer")
	t.Setenv("DBB_AUTH_CACHE_TTL_SECONDS", "42")

	cfg, err := Load(LoadOptions{})
	require.NoError(t, err)

	assert.False(t, cfg.Auth.AutoCreateUsers)
	assert.Equal(t, "viewer", cfg.Auth.Role())
	assert.Equal(t, 42, cfg.AuthCache.TTLSeconds,
		"the auth_* rule must not swallow the auth_cache_* keys")
}

// TestAuthProvisioningLegacyEnv covers the deployments in the wild: they set
// the Slack-shaped names, and silently ignoring them would turn
// auto-provisioning back on for everyone who turned it off.
func TestAuthProvisioningLegacyEnv(t *testing.T) {
	setAuthBaseEnv(t)
	t.Setenv("DBB_SLACK_AUTH_AUTO_CREATE_USERS", "false")
	t.Setenv("DBB_SLACK_AUTH_DEFAULT_ROLE", "admin")

	cfg, err := Load(LoadOptions{})
	require.NoError(t, err)

	assert.False(t, cfg.Auth.AutoCreateUsers, "the legacy alias must still turn it off")
	assert.Equal(t, "admin", cfg.Auth.Role())
}

// TestAuthProvisioningCanonicalWinsOverLegacy pins the resolution order when
// both are set. koanf gives no ordering guarantee across env keys, hence the
// explicit re-apply in applyAuthProvisioningAliases.
func TestAuthProvisioningCanonicalWinsOverLegacy(t *testing.T) {
	setAuthBaseEnv(t)
	t.Setenv("DBB_AUTH_AUTO_CREATE_USERS", "true")
	t.Setenv("DBB_AUTH_DEFAULT_ROLE", "viewer")
	t.Setenv("DBB_SLACK_AUTH_AUTO_CREATE_USERS", "false")
	t.Setenv("DBB_SLACK_AUTH_DEFAULT_ROLE", "admin")

	cfg, err := Load(LoadOptions{})
	require.NoError(t, err)

	assert.True(t, cfg.Auth.AutoCreateUsers, "canonical DBB_AUTH_AUTO_CREATE_USERS must win")
	assert.Equal(t, "viewer", cfg.Auth.Role(), "canonical DBB_AUTH_DEFAULT_ROLE must win")
}

// TestAuthProvisioningLegacyDoesNotClobberCanonicalFalse is the direction the
// alias promotion could plausibly get wrong: the canonical value is the falsy
// one, so "canonical wins" cannot be implemented as "keep the truthy one".
func TestAuthProvisioningLegacyDoesNotClobberCanonicalFalse(t *testing.T) {
	setAuthBaseEnv(t)
	t.Setenv("DBB_AUTH_AUTO_CREATE_USERS", "false")
	t.Setenv("DBB_SLACK_AUTH_AUTO_CREATE_USERS", "true")

	cfg, err := Load(LoadOptions{})
	require.NoError(t, err)

	assert.False(t, cfg.Auth.AutoCreateUsers,
		"an explicit canonical false must survive a legacy true")
}

// TestAuthProvisioningUnknownDefaultRoleFails covers the fail-closed rule: a
// typo'd role must stop the process rather than provision every OAuth user
// into a role no permission check has heard of.
func TestAuthProvisioningUnknownDefaultRoleFails(t *testing.T) {
	setAuthBaseEnv(t)
	t.Setenv("DBB_AUTH_DEFAULT_ROLE", "conector")

	_, err := Load(LoadOptions{})
	require.ErrorIs(t, err, ErrAuthDefaultRoleInvalid)

	t.Setenv("DBB_AUTH_DEFAULT_ROLE", "viewer")

	_, err = Load(LoadOptions{})
	require.NoError(t, err)
}

// TestAuthProvisioningLegacyUnknownRoleFails checks the alias path reaches the
// same validation — a typo is a typo whichever name carried it.
func TestAuthProvisioningLegacyUnknownRoleFails(t *testing.T) {
	setAuthBaseEnv(t)
	t.Setenv("DBB_SLACK_AUTH_DEFAULT_ROLE", "superuser")

	_, err := Load(LoadOptions{})
	require.ErrorIs(t, err, ErrAuthDefaultRoleInvalid)
}

// TestAuthProvisioningLegacyConfigFile covers the same alias promotion for a
// YAML file, which is how the settings were documented as well: the keys used
// to be `slack_auth.auto_create_users` / `.default_role`, and the field they
// unmarshalled into no longer exists.
func TestAuthProvisioningLegacyConfigFile(t *testing.T) {
	setAuthBaseEnv(t)

	configFile := filepath.Join(t.TempDir(), "config.yaml")
	content := "slack_auth:\n  auto_create_users: false\n  default_role: viewer\n"
	require.NoError(t, os.WriteFile(configFile, []byte(content), 0o600))

	cfg, err := Load(LoadOptions{ConfigFile: configFile})
	require.NoError(t, err)

	assert.False(t, cfg.Auth.AutoCreateUsers, "the legacy file keys must still be honored")
	assert.Equal(t, "viewer", cfg.Auth.Role())

	t.Setenv("DBB_AUTH_DEFAULT_ROLE", "admin")

	cfg, err = Load(LoadOptions{ConfigFile: configFile})
	require.NoError(t, err)

	assert.Equal(t, "admin", cfg.Auth.Role(), "the canonical variable outranks the legacy file key")
}

// TestOAuthUsersConfigRole covers the normalization the accessor applies:
// trimmed, lower-cased, and falling back to the default when unset — the same
// treatment ParseRoleMapping gives the role names in a group mapping.
func TestOAuthUsersConfigRole(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{"unset falls back", "", DefaultOAuthRole},
		{"whitespace only falls back", "   ", DefaultOAuthRole},
		{"trimmed", "  viewer ", "viewer"},
		{"lower-cased", "Admin", "admin"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg := OAuthUsersConfig{DefaultRole: tc.in}
			assert.Equal(t, tc.want, cfg.Role())
			require.NoError(t, cfg.Validate())
		})
	}
}
