package upstream

import "crypto/tls"

// SSL modes recognized on a server row. The names and the semantics are
// libpq's, because that is what an operator filling in the field expects.
const (
	// SSLModeDisable never encrypts and never offers to.
	SSLModeDisable = "disable"
	// SSLModeAllow prefers plaintext and accepts TLS when the server insists.
	SSLModeAllow = "allow"
	// SSLModePrefer offers TLS and falls back to plaintext when refused. The
	// empty string means this too — it is the default of every unset row.
	SSLModePrefer = "prefer"
	// SSLModeRequire encrypts without authenticating the server.
	SSLModeRequire = "require"
	// SSLModeVerifyCA encrypts and verifies the certificate chain.
	SSLModeVerifyCA = "verify-ca"
	// SSLModeVerifyFull encrypts and verifies chain and hostname.
	SSLModeVerifyFull = "verify-full"
)

// Attempt is one way of reaching the target. A nil TLS config means "connect in
// plaintext"; a non-nil one means "encrypt with exactly this config".
type Attempt struct {
	// TLS is the configuration for an encrypted attempt, or nil for plaintext.
	TLS *tls.Config
}

// Encrypted reports whether this attempt encrypts.
func (a Attempt) Encrypted() bool { return a.TLS != nil }

// Plan is the ordered list of attempts an ssl_mode authorizes, best first. It
// is the *only* place dbbat decides what an ssl_mode means — every protocol,
// on both the proxy side and the probe side, reads its policy from here.
//
// Not every protocol can honor the *order*, and that is the one place where the
// single policy still produces more than one behavior. Read the list as the SET
// of attempts a mode permits; the order is advisory and only binding for
// protocols that can act on it:
//
//   - PostgreSQL negotiates in band (SSLRequest / 'S' / 'N') and so never walks
//     the list: it reads OffersTLS and AllowsPlaintext and nothing else. Under
//     `allow` it therefore behaves exactly like `prefer` — it offers TLS and
//     upgrades if the server says yes. That is the pre-existing behavior of both
//     the proxy and libpq-with-one-round-trip, it errs toward encryption, and
//     re-ordering it would cost a second dial for no confidentiality gain.
//   - MySQL (CLIENT_SSL capability) and MongoDB (TLS from the first byte)
//     cannot negotiate in band, so they redial between attempts and DO honor the
//     order: `allow` really is plaintext-first for them.
//   - Oracle only reads RequiresTLS: the opportunistic modes never encrypt. See
//     the note on ConnectOracle.
//
// So `allow` is the only mode whose meaning still varies by protocol, and it
// varies within what the mode permits — never outside it. `disable`, `require`
// and the verify-* modes have a single attempt each, so order cannot apply and
// every protocol agrees exactly.
type Plan struct {
	// Mode is the ssl_mode this plan was built from, kept for error messages.
	Mode string
	// Attempts are the permitted ways to connect, in preference order. Never
	// empty.
	Attempts []Attempt
}

// PlanFor maps an ssl_mode (and the host it applies to) onto its attempts,
// encoding libpq's semantics once:
//
//   - disable:                 plaintext only, no TLS offered.
//   - allow:                   plaintext and TLS both permitted, plaintext
//     preferred — but see the Plan doc: only the redialing protocols honor that
//     preference, and PostgreSQL treats allow as prefer.
//   - prefer / "":             TLS first, plaintext when the server refuses.
//   - require:                 TLS only, certificate not verified — libpq's
//     "encrypt, don't authenticate".
//   - verify-ca / verify-full: TLS only, chain *and* hostname verified.
//
// verify-ca is deliberately treated as verify-full: Go's TLS stack cannot
// cleanly express "verify the chain but not the name" without a custom
// VerifyPeerCertificate, and erring stricter than libpq is the safe direction.
//
// An unrecognized mode is treated as prefer, matching the historical behavior
// of both the proxy and the probe: a typo'd row keeps working, opportunistically
// encrypted, rather than failing closed on a field nobody validates.
func PlanFor(mode, host string) Plan {
	plaintext := Attempt{}
	// InsecureSkipVerify is libpq parity, not an oversight: require/prefer/allow
	// mean "encrypt" without meaning "authenticate the server". The verify-*
	// modes are the ones that promise a verified peer.
	encrypted := Attempt{TLS: &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: true}}
	verified := Attempt{TLS: &tls.Config{MinVersion: tls.VersionTLS12, ServerName: host}}

	switch mode {
	case SSLModeDisable:
		return Plan{Mode: mode, Attempts: []Attempt{plaintext}}
	case SSLModeAllow:
		return Plan{Mode: mode, Attempts: []Attempt{plaintext, encrypted}}
	case SSLModeRequire:
		return Plan{Mode: mode, Attempts: []Attempt{encrypted}}
	case SSLModeVerifyCA, SSLModeVerifyFull:
		return Plan{Mode: mode, Attempts: []Attempt{verified}}
	default: // "prefer" and the empty default.
		return Plan{Mode: mode, Attempts: []Attempt{encrypted, plaintext}}
	}
}

// OffersTLS reports whether any attempt encrypts, i.e. whether dbbat should
// tell the server it can speak TLS at all.
func (p Plan) OffersTLS() bool {
	for _, a := range p.Attempts {
		if a.Encrypted() {
			return true
		}
	}

	return false
}

// AllowsPlaintext reports whether an unencrypted connection is acceptable. Its
// negation is "TLS is mandatory": a server that refuses to encrypt must be
// treated as a failure rather than silently downgraded.
func (p Plan) AllowsPlaintext() bool {
	for _, a := range p.Attempts {
		if !a.Encrypted() {
			return true
		}
	}

	return false
}

// RequiresTLS reports whether the mode forbids a plaintext fallback.
func (p Plan) RequiresTLS() bool { return !p.AllowsPlaintext() }

// PrefersTLS reports whether the first (best) attempt is the encrypted one.
// Only protocols that redial between attempts care: it is the difference
// between "allow" and "prefer".
func (p Plan) PrefersTLS() bool {
	return len(p.Attempts) > 0 && p.Attempts[0].Encrypted()
}

// TLSConfig returns the config of the first encrypted attempt, or nil when the
// mode never encrypts. In-band protocols use it as "the config to upgrade with
// if the server says yes".
func (p Plan) TLSConfig() *tls.Config {
	for _, a := range p.Attempts {
		if a.Encrypted() {
			return a.TLS
		}
	}

	return nil
}
