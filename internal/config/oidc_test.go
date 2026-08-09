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
	setOIDCBaseEnv(t)

	cfg, err := Load(LoadOptions{})
	require.NoError(t, err)

	assert.False(t, cfg.OIDCAuth.Enabled(), "no issuer means no OIDC provider")
	assert.Equal(t, DefaultOIDCScopes, cfg.OIDCAuth.Scopes)
	assert.Equal(t, DefaultOIDCDisplayName, cfg.OIDCAuth.DisplayName)
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

	cfg, err := Load(LoadOptions{})
	require.NoError(t, err)

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
