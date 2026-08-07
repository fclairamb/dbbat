package config

import (
	"crypto/tls"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The SQL Server client leg is the only one with a version ceiling to set, and
// the only one where getting it wrong is a hang rather than a refusal — so an
// unset variable has to keep meaning TLS 1.2 for as long as that is the tested
// default.
func TestMSSQLTLSMaxVersionDefaultsTo12(t *testing.T) {
	t.Setenv("DBB_DSN", "postgres://x:x@localhost/x")
	t.Setenv("DBB_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")

	cfg, err := Load(LoadOptions{})
	require.NoError(t, err)

	assert.Equal(t, MSSQLDefaultTLSMaxVersion, cfg.MSSQL.TLSMaxVersion)

	version, err := cfg.MSSQL.ResolveTLSMaxVersion()
	require.NoError(t, err)
	assert.Equal(t, uint16(tls.VersionTLS12), version)
}

func TestMSSQLTLSMaxVersionOptsIn13(t *testing.T) {
	t.Setenv("DBB_DSN", "postgres://x:x@localhost/x")
	t.Setenv("DBB_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	t.Setenv("DBB_MSSQL_TLS_MAX_VERSION", "1.3")

	cfg, err := Load(LoadOptions{})
	require.NoError(t, err)

	version, err := cfg.MSSQL.ResolveTLSMaxVersion()
	require.NoError(t, err)
	assert.Equal(t, uint16(tls.VersionTLS13), version)

	// The ceiling must not have been swallowed by the mssql_tls_* prefix rule
	// that routes cert/key/disable into the shared TLSConfig.
	assert.Empty(t, cfg.MSSQL.TLS.CertFile)
	assert.False(t, cfg.MSSQL.TLS.Disable)
}

// A ceiling nobody can honor must stop the process. Silently falling back
// would leave a deployment that asked for a TLS policy quietly running another
// one, which is the kind of thing nobody discovers until an audit.
func TestMSSQLTLSMaxVersionRejectsGarbage(t *testing.T) {
	t.Setenv("DBB_DSN", "postgres://x:x@localhost/x")
	t.Setenv("DBB_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	t.Setenv("DBB_MSSQL_TLS_MAX_VERSION", "1.4")

	_, err := Load(LoadOptions{})
	require.ErrorIs(t, err, ErrMSSQLTLSMaxVersionInvalid)
	assert.Contains(t, err.Error(), `"1.4"`)
}

func TestResolveTLSMaxVersion(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		value   string
		want    uint16
		wantErr bool
	}{
		{name: "unset", value: "", want: tls.VersionTLS12},
		{name: "1.2", value: "1.2", want: tls.VersionTLS12},
		{name: "1.3", value: "1.3", want: tls.VersionTLS13},
		{name: "surrounding whitespace", value: " 1.3 ", want: tls.VersionTLS13},
		{name: "tls1.2 spelling", value: "TLS1.2", wantErr: true},
		{name: "bare major", value: "1", wantErr: true},
		{name: "unsupported floor", value: "1.1", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := MSSQLConfig{TLSMaxVersion: tc.value}.ResolveTLSMaxVersion()
			if tc.wantErr {
				require.ErrorIs(t, err, ErrMSSQLTLSMaxVersionInvalid)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}
