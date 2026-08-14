package upstream

import (
	"testing"

	"github.com/sijms/go-ora/v3/configurations"
)

// TestOracleDSNDisablesFastLogin pins the one option that is not obvious from
// reading ConnectOracle: fast login must stay off.
//
// go-ora enables Oracle 23ai's one-round-trip logon by default, and its fast
// path reads the login reply as a protocol-negotiation message without checking
// for a TTC error first — so ORA-01017 came back as "message code error:
// received code 4 and expected code is 1" and a wrong password was classified
// as a handshake failure instead of an auth failure. A go-ora upgrade that
// re-defaults the option, or a careless edit to the option map, would silently
// reintroduce that; this test fails instead.
func TestOracleDSNDisablesFastLogin(t *testing.T) {
	t.Parallel()

	dsn := oracleDSN(OracleConfig{
		Host:        "db.example.com",
		Port:        1521,
		ServiceName: "FREEPDB1",
		Username:    "system",
		Password:    "s3cret",
		ProgramName: "dbbat connectivity-check",
	})

	cfg, err := configurations.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("ParseConfig(%q): %v", dsn, err)
	}

	if cfg.FastLogin {
		t.Error("FastLogin = true, want false: 23ai's one-round-trip logon hides ORA-01017 from the classifier")
	}
}

// TestOracleDSNCarriesTLSPolicy checks the ssl_mode translation stays wired
// through the extracted DSN builder: the opportunistic modes must not turn into
// mandatory TLS, and verify-* must keep server verification on.
func TestOracleDSNCarriesTLSPolicy(t *testing.T) {
	t.Parallel()

	base := OracleConfig{
		Host:        "db.example.com",
		Port:        1521,
		ServiceName: "FREEPDB1",
		Username:    "system",
		Password:    "s3cret",
		ProgramName: "dbbat connectivity-check",
	}

	// wantVerif is go-ora's own default (true) whenever SSL is off: the option is
	// only ever emitted alongside SSL=TRUE, so the plaintext rows assert that we
	// left it alone rather than that verification is meaningful there.
	for name, tc := range map[string]struct {
		sslMode   string
		wantSSL   bool
		wantVerif bool
	}{
		"empty is plaintext":        {"", false, true},
		"prefer stays plaintext":    {"prefer", false, true},
		"require encrypts only":     {"require", true, false},
		"verify-full authenticates": {"verify-full", true, true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			cfg := base
			cfg.SSLMode = tc.sslMode

			parsed, err := configurations.ParseConfig(oracleDSN(cfg))
			if err != nil {
				t.Fatalf("ParseConfig: %v", err)
			}

			if parsed.SSL != tc.wantSSL {
				t.Errorf("SSL = %v, want %v", parsed.SSL, tc.wantSSL)
			}

			if parsed.SSLVerify != tc.wantVerif {
				t.Errorf("SSL VERIFY = %v, want %v", parsed.SSLVerify, tc.wantVerif)
			}
		})
	}
}
