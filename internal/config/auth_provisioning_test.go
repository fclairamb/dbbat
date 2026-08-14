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

// TestAuthProvisioningPerProviderEnv is the feature: one provider gated, the
// other left on the instance-wide policy. Both directions are covered because
// "auto-creation defaults to on" makes the false-over-true case the one an
// unset/false confusion would break.
func TestAuthProvisioningPerProviderEnv(t *testing.T) {
	setAuthBaseEnv(t)
	t.Setenv("DBB_AUTH_AUTO_CREATE_USERS", "true")
	t.Setenv("DBB_AUTH_DEFAULT_ROLE", "connector")
	t.Setenv("DBB_AUTH_AUTO_CREATE_USERS_SLACK", "false")
	t.Setenv("DBB_AUTH_DEFAULT_ROLE_OIDC", "viewer")

	cfg, err := Load(LoadOptions{})
	require.NoError(t, err)

	assert.False(t, cfg.Auth.AutoCreateUsersFor("slack"),
		"the Slack override must beat the instance-wide true")
	assert.True(t, cfg.Auth.AutoCreateUsersFor("oidc"),
		"a provider with no override keeps the instance-wide value")
	assert.True(t, cfg.Auth.AutoCreateUsers,
		"an override must not rewrite the instance-wide value")

	assert.Equal(t, "viewer", cfg.Auth.RoleFor("oidc"), "the OIDC role override must apply")
	assert.Equal(t, "connector", cfg.Auth.RoleFor("slack"),
		"a provider with no role override keeps the instance-wide one")
}

// TestAuthProvisioningPerProviderOverridesFalseInstanceWide is the other
// direction — the deployment that turned auto-provisioning off globally and
// wants exactly one trusted issuer to mint accounts.
func TestAuthProvisioningPerProviderOverridesFalseInstanceWide(t *testing.T) {
	setAuthBaseEnv(t)
	t.Setenv("DBB_AUTH_AUTO_CREATE_USERS", "false")
	t.Setenv("DBB_AUTH_AUTO_CREATE_USERS_OIDC", "true")

	cfg, err := Load(LoadOptions{})
	require.NoError(t, err)

	assert.True(t, cfg.Auth.AutoCreateUsersFor("oidc"))
	assert.False(t, cfg.Auth.AutoCreateUsersFor("slack"))
}

// TestAuthProvisioningPerProviderFallsBackToDefaults pins the third rung of the
// chain: with nothing configured at all, a per-provider read is the default.
func TestAuthProvisioningPerProviderFallsBackToDefaults(t *testing.T) {
	// t.Setenv inline rather than through setAuthBaseEnv: it is what tells the
	// linter this test cannot run in parallel.
	t.Setenv("DBB_DSN", "postgres://x:x@localhost/x")
	t.Setenv("DBB_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")

	cfg, err := Load(LoadOptions{})
	require.NoError(t, err)

	for _, provider := range KnownOAuthProviders {
		assert.True(t, cfg.Auth.AutoCreateUsersFor(provider))
		assert.Equal(t, DefaultOAuthRole, cfg.Auth.RoleFor(provider))
	}
}

// TestAuthProvisioningPerProviderConfigFile covers the same overrides written
// as YAML — the env variables map onto exactly these keys, so both spellings
// have to reach the same place.
func TestAuthProvisioningPerProviderConfigFile(t *testing.T) {
	// t.Setenv inline rather than through setAuthBaseEnv: it is what tells the
	// linter this test cannot run in parallel.
	t.Setenv("DBB_DSN", "postgres://x:x@localhost/x")
	t.Setenv("DBB_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")

	configFile := writeAuthConfigFile(t,
		"auth:\n"+
			"  auto_create_users: true\n"+
			"  providers:\n"+
			"    slack:\n"+
			"      auto_create_users: false\n"+
			"    oidc:\n"+
			"      default_role: admin\n")

	cfg, err := Load(LoadOptions{ConfigFile: configFile})
	require.NoError(t, err)

	assert.False(t, cfg.Auth.AutoCreateUsersFor("slack"))
	assert.True(t, cfg.Auth.AutoCreateUsersFor("oidc"))
	assert.Equal(t, "admin", cfg.Auth.RoleFor("oidc"))
	assert.Equal(t, DefaultOAuthRole, cfg.Auth.RoleFor("slack"))
}

// TestAuthProvisioningPerProviderEnvBeatsConfigFile keeps the ordinary
// precedence intact one level down: the nested keys are ordinary koanf keys,
// so the environment still wins over the file.
func TestAuthProvisioningPerProviderEnvBeatsConfigFile(t *testing.T) {
	// t.Setenv inline rather than through setAuthBaseEnv: it is what tells the
	// linter this test cannot run in parallel.
	t.Setenv("DBB_DSN", "postgres://x:x@localhost/x")
	t.Setenv("DBB_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	t.Setenv("DBB_AUTH_AUTO_CREATE_USERS_SLACK", "false")

	configFile := writeAuthConfigFile(t,
		"auth:\n  providers:\n    slack:\n      auto_create_users: true\n")

	cfg, err := Load(LoadOptions{ConfigFile: configFile})
	require.NoError(t, err)

	assert.False(t, cfg.Auth.AutoCreateUsersFor("slack"))
}

// TestAuthProvisioningPerProviderDoesNotDisturbTheAliases is the regression
// guard the new envTransform rule most plausibly trips: it sits in front of the
// generic auth_ rule, and both the canonical instance-wide names and the
// DBB_AUTH_CACHE_* keys pass through the same chain.
func TestAuthProvisioningPerProviderDoesNotDisturbTheAliases(t *testing.T) {
	setAuthBaseEnv(t)
	t.Setenv("DBB_AUTH_AUTO_CREATE_USERS_OIDC", "true")
	t.Setenv("DBB_SLACK_AUTH_AUTO_CREATE_USERS", "false")
	t.Setenv("DBB_SLACK_AUTH_DEFAULT_ROLE", "viewer")
	t.Setenv("DBB_AUTH_CACHE_TTL_SECONDS", "42")

	cfg, err := Load(LoadOptions{})
	require.NoError(t, err)

	assert.False(t, cfg.Auth.AutoCreateUsers, "the legacy instance-wide alias must still apply")
	assert.Equal(t, "viewer", cfg.Auth.Role())
	assert.True(t, cfg.Auth.AutoCreateUsersFor("oidc"), "the per-provider override still wins for oidc")
	assert.False(t, cfg.Auth.AutoCreateUsersFor("slack"),
		"slack has no override, so it follows the aliased instance-wide value")
	assert.Equal(t, 42, cfg.AuthCache.TTLSeconds)
}

// TestAuthProvisioningPerProviderUnknownProviderFails covers the fail-closed
// half: a misspelled provider name would otherwise leave the operator with the
// policy they were trying to change, silently.
func TestAuthProvisioningPerProviderUnknownProviderFails(t *testing.T) {
	setAuthBaseEnv(t)
	t.Setenv("DBB_AUTH_AUTO_CREATE_USERS_OKTA", "false")

	_, err := Load(LoadOptions{})
	require.ErrorIs(t, err, ErrAuthProviderUnknown)
	assert.Contains(t, err.Error(), "okta")
}

// TestAuthProvisioningPerProviderUnknownRoleFails pins requirement three: the
// per-provider role goes through the same KnownRoles validation, mis-casing
// included.
func TestAuthProvisioningPerProviderUnknownRoleFails(t *testing.T) {
	for _, role := range []string{"conector", "Admin"} {
		t.Run(role, func(t *testing.T) {
			setAuthBaseEnv(t)
			t.Setenv("DBB_AUTH_DEFAULT_ROLE_OIDC", role)

			_, err := Load(LoadOptions{})
			require.ErrorIs(t, err, ErrAuthDefaultRoleInvalid)
			assert.Contains(t, err.Error(), `provider "oidc"`,
				"the error must say which provider carried the typo")
		})
	}
}

// TestOAuthUsersConfigPerProviderAccessors covers the resolution chain as a
// pure function, including the two cases the pointer exists for.
func TestOAuthUsersConfigPerProviderAccessors(t *testing.T) {
	t.Parallel()

	yes, no := true, false

	tests := []struct {
		name     string
		cfg      OAuthUsersConfig
		provider string
		wantAuto bool
		wantRole string
	}{
		{
			name:     "no providers map at all",
			cfg:      OAuthUsersConfig{AutoCreateUsers: true},
			provider: "oidc",
			wantAuto: true,
			wantRole: DefaultOAuthRole,
		},
		{
			name: "override false beats instance-wide true",
			cfg: OAuthUsersConfig{
				AutoCreateUsers: true,
				Providers:       map[string]OAuthProviderUsersConfig{"slack": {AutoCreateUsers: &no}},
			},
			provider: "slack",
			wantAuto: false,
			wantRole: DefaultOAuthRole,
		},
		{
			name: "override true beats instance-wide false",
			cfg: OAuthUsersConfig{
				AutoCreateUsers: false,
				Providers:       map[string]OAuthProviderUsersConfig{"oidc": {AutoCreateUsers: &yes}},
			},
			provider: "oidc",
			wantAuto: true,
			wantRole: DefaultOAuthRole,
		},
		{
			name: "a role-only override leaves auto-creation alone",
			cfg: OAuthUsersConfig{
				AutoCreateUsers: true,
				DefaultRole:     "connector",
				Providers:       map[string]OAuthProviderUsersConfig{"oidc": {DefaultRole: "viewer"}},
			},
			provider: "oidc",
			wantAuto: true,
			wantRole: "viewer",
		},
		{
			name: "an entry for another provider does not leak",
			cfg: OAuthUsersConfig{
				AutoCreateUsers: true,
				Providers:       map[string]OAuthProviderUsersConfig{"oidc": {AutoCreateUsers: &no, DefaultRole: "admin"}},
			},
			provider: "slack",
			wantAuto: true,
			wantRole: DefaultOAuthRole,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.wantAuto, tc.cfg.AutoCreateUsersFor(tc.provider))
			assert.Equal(t, tc.wantRole, tc.cfg.RoleFor(tc.provider))
			require.NoError(t, tc.cfg.Validate())
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
