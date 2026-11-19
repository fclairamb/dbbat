package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseTrustedProxies(t *testing.T) {
	t.Parallel()

	got, err := (&Config{}).ParseTrustedProxies()
	require.NoError(t, err)
	assert.Nil(t, got, "unset must trust nobody")

	got, err = (&Config{TrustedProxies: "10.0.0.0/8, 192.168.1.10 fd00::/8,::1"}).ParseTrustedProxies()
	require.NoError(t, err)
	assert.Equal(t, []string{"10.0.0.0/8", "192.168.1.10", "fd00::/8", "::1"}, got)

	for _, bad := range []string{"10.0.0.0/33", "not-an-ip", "10.0.0", "10.0.0.0/8,ingress"} {
		_, err = (&Config{TrustedProxies: bad}).ParseTrustedProxies()
		require.ErrorIs(t, err, ErrTrustedProxiesInvalid, bad)
	}
}

func TestLoadTrustedProxies(t *testing.T) {
	t.Setenv("DBB_DSN", "postgres://x:x@localhost/x")
	t.Setenv("DBB_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")

	t.Setenv("DBB_TRUSTED_PROXIES", "10.42.0.0/16")

	cfg, err := Load(LoadOptions{})
	require.NoError(t, err)
	assert.Equal(t, "10.42.0.0/16", cfg.TrustedProxies)

	t.Setenv("DBB_TRUSTED_PROXIES", "10.42.0.0/99")

	_, err = Load(LoadOptions{})
	require.ErrorIs(t, err, ErrTrustedProxiesInvalid, "a malformed CIDR is a startup error")
}
