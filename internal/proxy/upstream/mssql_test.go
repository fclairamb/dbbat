package upstream

import (
	"errors"
	"fmt"
	"testing"
)

func TestMSSQLEncryptionOption(t *testing.T) {
	t.Parallel()

	cases := []struct {
		mode string
		want []byte
	}{
		{SSLModeDisable, []byte{MSSQLEncryptNotSup}},
		{SSLModeAllow, []byte{MSSQLEncryptNotSup, MSSQLEncryptOn}},
		{SSLModePrefer, []byte{MSSQLEncryptOn, MSSQLEncryptNotSup}},
		{"", []byte{MSSQLEncryptOn, MSSQLEncryptNotSup}},
		{SSLModeRequire, []byte{MSSQLEncryptOn}},
		{SSLModeVerifyCA, []byte{MSSQLEncryptOn}},
		{SSLModeVerifyFull, []byte{MSSQLEncryptOn}},
		{"typo", []byte{MSSQLEncryptOn, MSSQLEncryptNotSup}},
	}

	for _, tc := range cases {
		t.Run(tc.mode, func(t *testing.T) {
			t.Parallel()

			plan := MSSQLPlan(MSSQLConfig{SSLMode: tc.mode, Host: "db.internal"})

			got := make([]byte, 0, len(plan.Attempts))
			for _, attempt := range plan.Attempts {
				got = append(got, MSSQLEncryptionOption(attempt))
			}

			if fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Errorf("ssl_mode %q offers %v, want %v", tc.mode, got, tc.want)
			}
		})
	}
}

// TestMSSQLEncryptionOptionNeverOffersEncryptOff pins the deliberate omission:
// ENCRYPT_OFF encrypts the login packet and then reverts, which is neither of
// the two outcomes upstream_tls can honestly report.
func TestMSSQLEncryptionOptionNeverOffersEncryptOff(t *testing.T) {
	t.Parallel()

	for _, mode := range []string{SSLModeDisable, SSLModeAllow, SSLModePrefer, SSLModeRequire, SSLModeVerifyFull} {
		for _, attempt := range MSSQLPlan(MSSQLConfig{SSLMode: mode}).Attempts {
			if got := MSSQLEncryptionOption(attempt); got == MSSQLEncryptOff {
				t.Errorf("ssl_mode %q offered ENCRYPT_OFF", mode)
			}
		}
	}
}

func TestMSSQLAnswerAcceptable(t *testing.T) {
	t.Parallel()

	encrypted := Attempt{TLS: PlanFor(SSLModeRequire, "h").Attempts[0].TLS}
	plaintext := Attempt{}

	cases := []struct {
		name    string
		attempt Attempt
		answer  byte
		want    bool
	}{
		{"encrypted attempt, server says ON", encrypted, MSSQLEncryptOn, true},
		{"encrypted attempt, server requires it", encrypted, MSSQLEncryptReq, true},
		{"encrypted attempt, server cannot", encrypted, MSSQLEncryptNotSup, false},
		{"encrypted attempt, server would revert after login", encrypted, MSSQLEncryptOff, false},
		{"plaintext attempt, server agrees", plaintext, MSSQLEncryptNotSup, true},
		{"plaintext attempt, server requires TLS", plaintext, MSSQLEncryptReq, false},
		{"plaintext attempt, server wants TLS", plaintext, MSSQLEncryptOn, false},
		{"plaintext attempt, server wants login-packet TLS", plaintext, MSSQLEncryptOff, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := MSSQLAnswerAcceptable(tc.attempt, tc.answer); got != tc.want {
				t.Errorf("MSSQLAnswerAcceptable() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestMSSQLRetryable(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"encryption mismatch", fmt.Errorf("wrapped: %w", ErrMSSQLEncryptionMismatch), true},
		{"tls handshake", fmt.Errorf("wrapped: %w", ErrMSSQLTLSHandshake), true},
		{"rejected login ends the chain", fmt.Errorf("wrapped: %w", ErrMSSQLLoginRejected), false},
		{"anything else ends the chain", errors.New("connection refused"), false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := MSSQLRetryable(tc.err); got != tc.want {
				t.Errorf("MSSQLRetryable(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
