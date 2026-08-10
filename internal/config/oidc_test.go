package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setOIDCBaseEnv sets the two variables every Load() needs, so each case below
// only has to say what it is actually testing.
func setOIDCBaseEnv(t *testing.T) {
	t.Helper()
	t.Setenv("DBB_DSN", "postgres://x:x@localhost/x")
	t.Setenv("DBB_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
}

// TestOIDCDefaultsWhenUnset pins the "nobody configured OIDC" case: the
// provider is off, and the defaults are in place for whoever turns it on.
func TestOIDCDefaultsWhenUnset(t *testing.T) {
	// t.Setenv inline rather than through setOIDCBaseEnv: it is what tells
	// the linter this test cannot run in parallel.
	t.Setenv("DBB_DSN", "postgres://x:x@localhost/x")
	t.Setenv("DBB_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")

	cfg, err := Load(LoadOptions{})
	require.NoError(t, err)

	assert.False(t, cfg.OIDCAuth.Enabled(), "no issuer means no OIDC provider")
	assert.Equal(t, DefaultOIDCScopes, cfg.OIDCAuth.Scopes)
	assert.Equal(t, DefaultOIDCDisplayName, cfg.OIDCAuth.DisplayName)
	assert.Equal(t, DefaultOIDCGroupsClaim, cfg.OIDCAuth.GroupsClaimName())
	assert.False(t, cfg.OIDCAuth.RoleMappingEnabled(), "no mapping means roles stay manual")
}

// TestOIDCEnvMapping verifies every DBB_OIDC_* variable lands on the right
// field through the oidc_* koanf prefix rule.
func TestOIDCEnvMapping(t *testing.T) {
	setOIDCBaseEnv(t)
	t.Setenv("DBB_OIDC_ISSUER", "https://accounts.google.com")
	t.Setenv("DBB_OIDC_CLIENT_ID", "client-abc")
	t.Setenv("DBB_OIDC_CLIENT_SECRET", "secret-xyz")
	t.Setenv("DBB_OIDC_SCOPES", "openid,email,profile,groups")
	t.Setenv("DBB_OIDC_DISPLAY_NAME", "Acme SSO")
	t.Setenv("DBB_OIDC_EMAIL_DOMAINS", "acme.test, corp.example")
	t.Setenv("DBB_OIDC_GROUPS_CLAIM", "roles")
	t.Setenv("DBB_OIDC_ROLE_MAPPING", "admin=db-admins,viewer=analysts")

	cfg, err := Load(LoadOptions{})
	require.NoError(t, err)

	assert.Equal(t, "roles", cfg.OIDCAuth.GroupsClaimName())

	mapping, err := cfg.OIDCAuth.ParseRoleMapping()
	require.NoError(t, err)
	assert.Equal(t, map[string][]string{"admin": {"db-admins"}, "viewer": {"analysts"}}, mapping)

	assert.True(t, cfg.OIDCAuth.Enabled(), "an issuer enables the provider")
	assert.Equal(t, "https://accounts.google.com", cfg.OIDCAuth.Issuer)
	assert.Equal(t, "client-abc", cfg.OIDCAuth.ClientID)
	assert.Equal(t, "secret-xyz", cfg.OIDCAuth.ClientSecret)
	assert.Equal(t, "Acme SSO", cfg.OIDCAuth.DisplayName)
	assert.Equal(t, []string{"openid", "email", "profile", "groups"}, cfg.OIDCAuth.ScopeList())
	assert.Equal(t, []string{"acme.test", "corp.example"}, cfg.OIDCAuth.EmailDomainList())
}

// TestOIDCIssuerWithoutCredentialsFails covers the half-configured deployment:
// failing at startup beats offering a login button that can only error out.
func TestOIDCIssuerWithoutCredentialsFails(t *testing.T) {
	setOIDCBaseEnv(t)
	t.Setenv("DBB_OIDC_ISSUER", "https://accounts.google.com")

	_, err := Load(LoadOptions{})
	require.ErrorIs(t, err, ErrOIDCClientCredentialsRequired)

	t.Setenv("DBB_OIDC_CLIENT_ID", "client-abc")

	_, err = Load(LoadOptions{})
	require.ErrorIs(t, err, ErrOIDCClientCredentialsRequired,
		"a client id without a secret is still half-configured")
}

// TestOIDCListSplitting covers the tokenizer behind the two list-shaped
// variables: commas, whitespace and a trailing separator are all tolerated.
func TestOIDCListSplitting(t *testing.T) {
	t.Parallel()

	cfg := OIDCAuthConfig{Scopes: " openid  email,profile, ", EmailDomains: ",acme.test,,"}

	assert.Equal(t, []string{"openid", "email", "profile"}, cfg.ScopeList())
	assert.Equal(t, []string{"acme.test"}, cfg.EmailDomainList())
	assert.Empty(t, OIDCAuthConfig{}.EmailDomainList(), "no allowlist means no restriction")
}

// TestParseRoleMapping covers the grammar of DBB_OIDC_ROLE_MAPPING. The
// separator rules matter more than they look: pairs split on commas *only*,
// because "admin=Domain Admins" is a perfectly ordinary directory group.
func TestParseRoleMapping(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		value string
		want  map[string][]string
	}{
		{
			name:  "empty means no mapping at all",
			value: "",
			want:  nil,
		},
		{
			name:  "a single pair",
			value: "admin=db-admins",
			want:  map[string][]string{"admin": {"db-admins"}},
		},
		{
			name:  "several roles",
			value: "admin=db-admins,viewer=analysts,connector=engineering",
			want: map[string][]string{
				"admin":     {"db-admins"},
				"viewer":    {"analysts"},
				"connector": {"engineering"},
			},
		},
		{
			name:  "repeating a role unions its groups",
			value: "admin=db-admins, admin=sre , admin=db-admins",
			want:  map[string][]string{"admin": {"db-admins", "sre"}},
		},
		{
			name:  "group names may contain spaces",
			value: "admin=Domain Admins,viewer=Data Analysts",
			want:  map[string][]string{"admin": {"Domain Admins"}, "viewer": {"Data Analysts"}},
		},
		{
			name:  "role names are case-insensitive, group values are not",
			value: "ADMIN=DB-Admins",
			want:  map[string][]string{"admin": {"DB-Admins"}},
		},
		{
			name:  "entra object ids are ordinary values",
			value: "admin=1f4e8c02-9b7a-4f61-8a1e-3c2d5e6f7a8b",
			want:  map[string][]string{"admin": {"1f4e8c02-9b7a-4f61-8a1e-3c2d5e6f7a8b"}},
		},
		{
			name:  "a trailing comma is harmless",
			value: "admin=db-admins,",
			want:  map[string][]string{"admin": {"db-admins"}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			mapping, err := OIDCAuthConfig{RoleMapping: tc.value}.ParseRoleMapping()
			require.NoError(t, err)
			assert.Equal(t, tc.want, mapping)
		})
	}
}

// TestParseRoleMappingRejectsGarbage pins the fail-loud half: a mapping that
// cannot be read is a mapping that would demote everyone at the next login,
// so it must stop the process rather than resolve to "nobody matches".
func TestParseRoleMappingRejectsGarbage(t *testing.T) {
	t.Parallel()

	for _, value := range []string{
		"db-admins",       // no "=" at all
		"root=db-admins",  // not a dbbat role
		"admin=",          // no group
		"admin=a,viewer=", // one good pair does not save a bad one
		",,",              // nothing usable
		"superuser=Domain Admins",
	} {
		_, err := OIDCAuthConfig{RoleMapping: value}.ParseRoleMapping()
		require.ErrorIsf(t, err, ErrOIDCRoleMappingInvalid, "value %q must be refused", value)
	}
}

// TestLoadRejectsBadRoleMapping proves the parse runs at startup, not at the
// first login.
func TestLoadRejectsBadRoleMapping(t *testing.T) {
	setOIDCBaseEnv(t)
	t.Setenv("DBB_OIDC_ISSUER", "https://accounts.google.com")
	t.Setenv("DBB_OIDC_CLIENT_ID", "client-abc")
	t.Setenv("DBB_OIDC_CLIENT_SECRET", "secret-xyz")
	t.Setenv("DBB_OIDC_ROLE_MAPPING", "superuser=db-admins")

	_, err := Load(LoadOptions{})
	require.ErrorIs(t, err, ErrOIDCRoleMappingInvalid)
}
