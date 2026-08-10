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

// writeAuthConfigFile writes a YAML config file and returns its path.
func writeAuthConfigFile(t *testing.T, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

	return path
}

// TestAuthProvisioningLegacyConfigFile covers the alias promotion for a YAML
// file, which is how the settings were documented too: the keys used to be
// `slack_auth.auto_create_users` / `.default_role`, and the field they
// unmarshalled into no longer exists.
func TestAuthProvisioningLegacyConfigFile(t *testing.T) {
	// t.Setenv inline rather than through setAuthBaseEnv: it is what tells the
	// linter this test cannot run in parallel.
	t.Setenv("DBB_DSN", "postgres://x:x@localhost/x")
	t.Setenv("DBB_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")

	configFile := writeAuthConfigFile(t,
		"slack_auth:\n  auto_create_users: false\n  default_role: viewer\n")

	cfg, err := Load(LoadOptions{ConfigFile: configFile})
	require.NoError(t, err)

	assert.False(t, cfg.Auth.AutoCreateUsers, "the legacy file keys must still be honored")
	assert.Equal(t, "viewer", cfg.Auth.Role())
}

// TestAuthProvisioningPrecedenceAcrossSources is the full matrix the resolution
// order has to satisfy. The canonical key wins over the legacy one **whatever
// source each arrived from** — the alias spans two koanf keys, so a canonical
// value written in the config file has to beat a legacy variable lingering in
// the environment just as surely as the other way round.
func TestAuthProvisioningPrecedenceAcrossSources(t *testing.T) {
	tests := []struct {
		name     string
		file     string
		env      map[string]string
		wantRole string
		wantAuto bool
	}{
		{
			name:     "canonical file beats legacy file",
			file:     "auth:\n  default_role: viewer\n  auto_create_users: true\nslack_auth:\n  default_role: admin\n  auto_create_users: false\n",
			wantRole: "viewer",
			wantAuto: true,
		},
		{
			name:     "canonical file beats legacy env",
			file:     "auth:\n  default_role: viewer\n  auto_create_users: true\n",
			env:      map[string]string{"DBB_SLACK_AUTH_DEFAULT_ROLE": "admin", "DBB_SLACK_AUTH_AUTO_CREATE_USERS": "false"},
			wantRole: "viewer",
			wantAuto: true,
		},
		{
			name:     "canonical env beats legacy file",
			file:     "slack_auth:\n  default_role: admin\n  auto_create_users: false\n",
			env:      map[string]string{"DBB_AUTH_DEFAULT_ROLE": "viewer", "DBB_AUTH_AUTO_CREATE_USERS": "true"},
			wantRole: "viewer",
			wantAuto: true,
		},
		{
			name:     "legacy file applies when no canonical value exists",
			file:     "slack_auth:\n  default_role: admin\n  auto_create_users: false\n",
			wantRole: "admin",
			wantAuto: false,
		},
		{
			name:     "canonical file false is not overwritten by a legacy true",
			file:     "auth:\n  auto_create_users: false\nslack_auth:\n  auto_create_users: true\n",
			wantRole: DefaultOAuthRole,
			wantAuto: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			setAuthBaseEnv(t)

			for name, value := range tc.env {
				t.Setenv(name, value)
			}

			cfg, err := Load(LoadOptions{ConfigFile: writeAuthConfigFile(t, tc.file)})
			require.NoError(t, err)

			assert.Equal(t, tc.wantRole, cfg.Auth.Role())
			assert.Equal(t, tc.wantAuto, cfg.Auth.AutoCreateUsers)
		})
	}
}

// TestAuthProvisioningMiscasedRoleFails is the upgrade-safety case. Before
// these settings moved, the default role was read raw and never validated, so a
// deployment carrying DBB_SLACK_AUTH_DEFAULT_ROLE=Admin assigned the literal
// string "Admin" — a role nothing recognizes, granting nothing. Folding it to
// "admin" would hand every auto-provisioned user real admin rights on an
// upgrade alone, so it has to fail closed, and say what the canonical spelling
// is.
func TestAuthProvisioningMiscasedRoleFails(t *testing.T) {
	for _, name := range []string{"DBB_SLACK_AUTH_DEFAULT_ROLE", "DBB_AUTH_DEFAULT_ROLE"} {
		t.Run(name, func(t *testing.T) {
			setAuthBaseEnv(t)
			t.Setenv(name, "Admin")

			_, err := Load(LoadOptions{})
			require.ErrorIs(t, err, ErrAuthDefaultRoleInvalid,
				"a mis-cased role must not be laundered into a valid one")
			assert.Contains(t, err.Error(), `"admin"`, "the error must name the canonical spelling")
		})
	}
}

// TestOAuthUsersConfigValidate pins the exact-match rule and the two error
// shapes without going through Load.
func TestOAuthUsersConfigValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		role    string
		wantErr bool
	}{
		{"unset is the default", "", false},
		{"exact known role", "viewer", false},
		{"mis-cased is refused", "Admin", true},
		{"padded is refused", " admin ", true},
		{"unknown is refused", "superuser", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := OAuthUsersConfig{DefaultRole: tc.role}.Validate()
			if tc.wantErr {
				require.ErrorIs(t, err, ErrAuthDefaultRoleInvalid)

				return
			}

			require.NoError(t, err)
		})
	}
}

// TestOAuthUsersConfigRole covers the accessor: verbatim, with a fallback when
// unset. Normalization deliberately does not happen here — Validate refuses
// anything that would need it.
func TestOAuthUsersConfigRole(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{"unset falls back", "", DefaultOAuthRole},
		{"known role passes through", "viewer", "viewer"},
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
