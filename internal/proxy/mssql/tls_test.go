package mssql

import (
	"crypto/tls"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/config"
)

// TestLoadTLSConfigVersionCeiling pins the two things an operator can observe
// about DBB_MSSQL_TLS_MAX_VERSION: the default is unchanged (1.2), and opting
// in to 1.3 also turns session tickets off — the pair is what makes the
// encapsulated handshake safe at 1.3, so they must not drift apart.
func TestLoadTLSConfigVersionCeiling(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		value       string
		wantVersion uint16
		wantTickets bool
	}{
		{name: "unset", value: "", wantVersion: tls.VersionTLS12, wantTickets: true},
		{name: "explicit 1.2", value: "1.2", wantVersion: tls.VersionTLS12, wantTickets: true},
		{name: "opt-in 1.3", value: "1.3", wantVersion: tls.VersionTLS13, wantTickets: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg, err := loadTLSConfig(config.MSSQLConfig{TLSMaxVersion: tc.value})
			require.NoError(t, err)
			require.NotNil(t, cfg)

			assert.Equal(t, tc.wantVersion, cfg.MaxVersion)
			assert.Equal(t, uint16(tls.VersionTLS12), cfg.MinVersion,
				"the floor stays at 1.2 whatever the ceiling")
			assert.Equal(t, tc.wantTickets, !cfg.SessionTicketsDisabled)
		})
	}
}

// A ceiling the proxy cannot honor must fail the listener rather than quietly
// serving a different policy. config.Load checks this too; this covers a Server
// built straight from a struct literal, as the tests and any embedder do.
func TestLoadTLSConfigRejectsAnUnknownCeiling(t *testing.T) {
	t.Parallel()

	_, err := loadTLSConfig(config.MSSQLConfig{TLSMaxVersion: "1.4"})
	require.ErrorIs(t, err, config.ErrMSSQLTLSMaxVersionInvalid)
}

// Disable wins over everything, including a bad ceiling: there is no handshake
// to have a version.
func TestLoadTLSConfigDisabled(t *testing.T) {
	t.Parallel()

	cfg, err := loadTLSConfig(config.MSSQLConfig{TLS: config.TLSConfig{Disable: true}})
	require.NoError(t, err)
	assert.Nil(t, cfg)
}
