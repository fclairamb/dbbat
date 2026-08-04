package upstream

import (
	"crypto/tls"
	"testing"
)

// TestPlanFor is the anti-drift gate for the ssl_mode policy: it is the single
// description of what every mode means, and after the connectors were unified
// there is exactly one implementation for it to describe. A change to the table
// below is a change to dbbat's confidentiality guarantees, on every protocol
// and on both the proxy and the connectivity-check path.
//
// What every protocol honors is the attempt SET — which encryption states a
// mode permits at all. Only the protocols that redial between attempts honor
// the attempt ORDER:
//
//	protocol    | honors set | honors order | note
//	------------|------------|--------------|--------------------------------
//	PostgreSQL  | yes        | no           | in-band SSLRequest; allow == prefer
//	MySQL       | yes        | yes          | redials between attempts
//	MongoDB     | yes        | yes          | redials between attempts
//	Oracle      | yes*       | no           | only RequiresTLS; opportunistic modes never encrypt
//
// (*) Oracle stays inside what the mode permits — the opportunistic modes do
// permit plaintext — but it never takes the encrypted option they also permit.
//
// `allow` is therefore the only mode whose behavior still varies by protocol.
// Every other mode has one attempt, so order cannot apply. See the Plan doc.
func TestPlanFor(t *testing.T) {
	t.Parallel()

	const host = "db.example.com"

	for _, tc := range []struct {
		name string
		mode string
		// want is the ordered attempt list: one entry per attempt, true when
		// the attempt is encrypted.
		want []bool
		// wantVerified is whether the encrypted attempts authenticate the
		// server (chain + hostname) rather than merely encrypting.
		wantVerified bool
	}{
		{name: "disable is plaintext only", mode: SSLModeDisable, want: []bool{false}},
		{name: "allow tries plaintext then TLS", mode: SSLModeAllow, want: []bool{false, true}},
		{name: "prefer tries TLS then plaintext", mode: SSLModePrefer, want: []bool{true, false}},
		{name: "empty defaults to prefer", mode: "", want: []bool{true, false}},
		{name: "unknown mode defaults to prefer", mode: "sure-why-not", want: []bool{true, false}},
		{name: "require is TLS only, unverified", mode: SSLModeRequire, want: []bool{true}},
		{name: "verify-ca is TLS only, verified", mode: SSLModeVerifyCA, want: []bool{true}, wantVerified: true},
		{name: "verify-full is TLS only, verified", mode: SSLModeVerifyFull, want: []bool{true}, wantVerified: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			plan := PlanFor(tc.mode, host)

			if len(plan.Attempts) != len(tc.want) {
				t.Fatalf("ssl_mode=%q: %d attempts, want %d", tc.mode, len(plan.Attempts), len(tc.want))
			}

			for i, wantEncrypted := range tc.want {
				if got := plan.Attempts[i].Encrypted(); got != wantEncrypted {
					t.Fatalf("ssl_mode=%q attempt %d: encrypted=%v, want %v", tc.mode, i, got, wantEncrypted)
				}
			}

			assertDerived(t, plan, tc.want)
			assertTLSConfig(t, plan, host, tc.wantVerified)
		})
	}
}

// assertDerived checks the accessors the connectors actually branch on stay
// consistent with the attempt list they are derived from.
func assertDerived(t *testing.T, plan Plan, want []bool) {
	t.Helper()

	var wantOffers, wantPlain bool

	for _, encrypted := range want {
		if encrypted {
			wantOffers = true
		} else {
			wantPlain = true
		}
	}

	if plan.OffersTLS() != wantOffers {
		t.Fatalf("OffersTLS = %v, want %v", plan.OffersTLS(), wantOffers)
	}

	if plan.AllowsPlaintext() != wantPlain {
		t.Fatalf("AllowsPlaintext = %v, want %v", plan.AllowsPlaintext(), wantPlain)
	}

	if plan.RequiresTLS() == wantPlain {
		t.Fatalf("RequiresTLS = %v, want %v", plan.RequiresTLS(), !wantPlain)
	}

	if plan.PrefersTLS() != want[0] {
		t.Fatalf("PrefersTLS = %v, want %v", plan.PrefersTLS(), want[0])
	}
}

// assertTLSConfig checks the security-carrying half of every encrypted attempt:
// the version floor, and whether the server is authenticated.
func assertTLSConfig(t *testing.T, plan Plan, host string, wantVerified bool) {
	t.Helper()

	cfg := plan.TLSConfig()

	if !plan.OffersTLS() {
		if cfg != nil {
			t.Fatal("a mode that never encrypts must not hand out a tls.Config")
		}

		return
	}

	if cfg == nil {
		t.Fatal("a mode that offers TLS must hand out a tls.Config")
	}

	if cfg.MinVersion != tls.VersionTLS12 {
		t.Fatalf("MinVersion = %x, want TLS 1.2", cfg.MinVersion)
	}

	if wantVerified {
		if cfg.InsecureSkipVerify {
			t.Fatal("verify-ca/verify-full must not skip certificate verification")
		}

		if cfg.ServerName != host {
			t.Fatalf("ServerName = %q, want %q", cfg.ServerName, host)
		}

		return
	}

	if !cfg.InsecureSkipVerify {
		t.Fatal("require/prefer/allow encrypt without authenticating the server (libpq parity)")
	}

	if cfg.ServerName != "" {
		t.Fatalf("ServerName = %q, want empty for an unverified attempt", cfg.ServerName)
	}
}
