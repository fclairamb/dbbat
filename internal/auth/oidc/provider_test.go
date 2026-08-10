package oidc

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testIssuer is a minimal but real OIDC issuer: a discovery document, a JWKS
// served from an RSA key it owns, and a token endpoint that hands back an ID
// token signed with that key. Everything the provider is supposed to verify is
// therefore genuinely verifiable — and individually corruptible, which is what
// the negative tests do.
type testIssuer struct {
	t      *testing.T
	server *httptest.Server
	key    *rsa.PrivateKey

	// claims is the payload put in the next issued ID token. Tests mutate it
	// before calling ExchangeCode.
	claims map[string]any
	// signingKey signs the ID token. Defaults to key; a test points it at a
	// foreign key to forge a bad signature.
	signingKey *rsa.PrivateKey
	// omitIDToken makes the token endpoint answer a bare OAuth2 token.
	omitIDToken bool

	// mu guards the fields the HTTP handlers write and the test body reads.
	// The race detector has no happens-before edge across the client/server
	// boundary of an httptest server, so this is not optional.
	mu sync.Mutex
	// lastForm records the token request, so PKCE can be asserted.
	lastForm url.Values
	// discoveryHits counts discovery-document fetches.
	discoveryHits int
}

// tokenForm returns the last token request's form, safely.
func (i *testIssuer) tokenForm() url.Values {
	i.mu.Lock()
	defer i.mu.Unlock()

	return i.lastForm
}

const testKeyID = "test-key"

func newTestIssuer(t *testing.T) *testIssuer {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	issuer := &testIssuer{t: t, key: key, signingKey: key}

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		issuer.mu.Lock()
		issuer.discoveryHits++
		issuer.mu.Unlock()

		writeJSON(w, map[string]any{
			"issuer":                                issuer.URL(),
			"authorization_endpoint":                issuer.URL() + "/authorize",
			"token_endpoint":                        issuer.URL() + "/token",
			"jwks_uri":                              issuer.URL() + "/jwks",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"keys": []any{jwkFor(&key.PublicKey)}})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("token endpoint: ParseForm: %v", err)

			return
		}

		issuer.mu.Lock()
		issuer.lastForm = r.Form
		omit := issuer.omitIDToken
		issuer.mu.Unlock()

		body := map[string]any{"access_token": "at-123", "token_type": "Bearer"}
		if !omit {
			body["id_token"] = issuer.signIDToken()
		}

		writeJSON(w, body)
	})

	issuer.server = httptest.NewServer(mux)
	t.Cleanup(issuer.server.Close)

	issuer.claims = map[string]any{
		"iss":            issuer.URL(),
		"aud":            "dbbat-client",
		"sub":            "user-42",
		"exp":            time.Now().Add(time.Hour).Unix(),
		"iat":            time.Now().Unix(),
		"email":          "Alice@Example.COM",
		"email_verified": true,
		"name":           "Alice Example",
		"picture":        "https://example.test/alice.png",
	}

	return issuer
}

func (i *testIssuer) URL() string {
	if i.server == nil {
		// Discovery is served before httptest hands us the URL only in the
		// handler closures, which run after Start; this branch never fires in
		// practice but keeps the closures honest.
		return ""
	}

	return i.server.URL
}

// signIDToken produces a compact RS256 JWT over the current claims. Hand-rolled
// rather than pulled from a JOSE library so the test owns every byte the
// provider is asked to verify.
func (i *testIssuer) signIDToken() string {
	i.t.Helper()

	header, err := json.Marshal(map[string]any{"alg": "RS256", "typ": "JWT", "kid": testKeyID})
	require.NoError(i.t, err)

	payload, err := json.Marshal(i.claims)
	require.NoError(i.t, err)

	signingInput := b64(header) + "." + b64(payload)
	digest := sha256.Sum256([]byte(signingInput))

	signature, err := rsa.SignPKCS1v15(rand.Reader, i.signingKey, crypto.SHA256, digest[:])
	require.NoError(i.t, err)

	return signingInput + "." + b64(signature)
}

// provider builds a Provider pointed at this issuer, with the test server's
// HTTP client so discovery and JWKS never leave the process.
func (i *testIssuer) provider(t *testing.T, mutate ...func(*Config)) *Provider {
	t.Helper()

	cfg := Config{
		Issuer:       i.URL(),
		ClientID:     "dbbat-client",
		ClientSecret: "dbbat-secret",
		Scopes:       []string{"email", "profile"},
		Label:        "Acme SSO",
	}

	for _, m := range mutate {
		m(&cfg)
	}

	p, err := NewProvider(cfg)
	require.NoError(t, err)

	p.HTTPClient = i.server.Client()

	return p
}

func b64(b []byte) string {
	return base64.RawURLEncoding.EncodeToString(b)
}

func jwkFor(pub *rsa.PublicKey) map[string]any {
	return map[string]any{
		"kty": "RSA",
		"kid": testKeyID,
		"use": "sig",
		"alg": "RS256",
		"n":   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		"e":   base64.RawURLEncoding.EncodeToString(bigEndian(pub.E)),
	}
}

func bigEndian(e int) []byte {
	var out []byte
	for e > 0 {
		out = append([]byte{byte(e & 0xff)}, out...)
		e >>= 8
	}

	return out
}

func writeJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

func TestProviderName(t *testing.T) {
	t.Parallel()

	p, err := NewProvider(Config{Issuer: "https://issuer.example.test"})
	require.NoError(t, err)
	assert.Equal(t, "oidc", p.Name())
	assert.Equal(t, "SSO", p.DisplayName(), "display name must default to SSO")
}

func TestNewProviderRequiresIssuer(t *testing.T) {
	t.Parallel()

	_, err := NewProvider(Config{ClientID: "id"})
	require.ErrorIs(t, err, ErrIssuerRequired)
}

func TestNewProviderAlwaysRequestsOpenIDScope(t *testing.T) {
	t.Parallel()

	p, err := NewProvider(Config{Issuer: "https://issuer.example.test", Scopes: []string{"email", " ", "email"}})
	require.NoError(t, err)
	assert.Equal(t, []string{"openid", "email"}, p.cfg.Scopes,
		"openid must be present exactly once, blanks and duplicates dropped")
}

func TestAuthorizeURLWithPKCE(t *testing.T) {
	t.Parallel()

	issuer := newTestIssuer(t)
	p := issuer.provider(t)

	authorizeURL, verifier, err := p.AuthorizeURLWithPKCE(t.Context(), "state-abc", "https://dbbat.test/cb")
	require.NoError(t, err)
	require.NotEmpty(t, verifier)

	parsed, err := url.Parse(authorizeURL)
	require.NoError(t, err)

	query := parsed.Query()
	assert.Equal(t, issuer.URL()+"/authorize", parsed.Scheme+"://"+parsed.Host+parsed.Path)
	assert.Equal(t, "code", query.Get("response_type"))
	assert.Equal(t, "dbbat-client", query.Get("client_id"))
	assert.Equal(t, "state-abc", query.Get("state"))
	assert.Equal(t, "https://dbbat.test/cb", query.Get("redirect_uri"))
	assert.Equal(t, "openid email profile", query.Get("scope"))
	assert.Equal(t, "S256", query.Get("code_challenge_method"))
	assert.NotEmpty(t, query.Get("code_challenge"))
	assert.NotContains(t, authorizeURL, verifier, "the verifier itself must never travel in the URL")

	// The challenge is the S256 of the verifier, which is what makes the
	// stored verifier worth carrying.
	sum := sha256.Sum256([]byte(verifier))
	assert.Equal(t, b64(sum[:]), query.Get("code_challenge"))
}

func TestExchangeCodeHappyPath(t *testing.T) {
	t.Parallel()

	issuer := newTestIssuer(t)
	p := issuer.provider(t)

	user, err := p.ExchangeCodeWithVerifier(t.Context(), "auth-code", "https://dbbat.test/cb", "verifier-xyz")
	require.NoError(t, err)

	assert.Equal(t, "user-42", user.ProviderID)
	assert.Equal(t, "alice@example.com", user.Email, "email is normalized to lower case")
	assert.Equal(t, "Alice Example", user.DisplayName)
	assert.Equal(t, "https://example.test/alice.png", user.AvatarURL)
	assert.Equal(t, "verifier-xyz", issuer.tokenForm().Get("code_verifier"),
		"the PKCE verifier must be presented at the token endpoint")

	// RawData keeps the full claim set for the identity metadata column.
	var raw map[string]any
	require.NoError(t, json.Unmarshal(user.RawData, &raw))
	assert.Equal(t, "user-42", raw["sub"])
	assert.Equal(t, true, raw["email_verified"])
	assert.NotContains(t, string(user.RawData), "dbbat-secret", "the client secret must never reach RawData")
}

// TestExchangeCodeReadsGroupsClaim covers the encodings issuers actually use
// for group membership. The claim is read out of the *verified* ID token, so
// what this test pins is the whole trust story behind the role mapping.
func TestExchangeCodeReadsGroupsClaim(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		claim  any // value put in the ID token; nil means "omit the claim"
		config string
		want   []string
	}{
		{
			name:  "array encoding",
			claim: []any{"db-admins", "analysts"},
			want:  []string{"db-admins", "analysts"},
		},
		{
			name:  "single string encoding",
			claim: "db-admins",
			want:  []string{"db-admins"},
		},
		{
			name:  "a lone string is one group, never a delimited list",
			claim: "Domain Admins",
			want:  []string{"Domain Admins"},
		},
		{
			name:  "missing claim yields no groups",
			claim: nil,
			want:  nil,
		},
		{
			name:  "empty array yields no groups",
			claim: []any{},
			want:  nil,
		},
		{
			name:  "blanks and duplicates are dropped",
			claim: []any{" db-admins ", "", "db-admins", "   "},
			want:  []string{"db-admins"},
		},
		{
			name:  "non-string entries are dropped, strings survive",
			claim: []any{"db-admins", 42, nil, "analysts"},
			want:  []string{"db-admins", "analysts"},
		},
		{
			name:  "entra object ids pass through verbatim",
			claim: []any{"1f4e8c02-9b7a-4f61-8a1e-3c2d5e6f7a8b"},
			want:  []string{"1f4e8c02-9b7a-4f61-8a1e-3c2d5e6f7a8b"},
		},
		{
			name:   "a custom claim name is honoured",
			claim:  []any{"ignored"},
			config: "roles",
			want:   nil,
		},
		{
			name:  "an unusable claim type yields no groups",
			claim: map[string]any{"nested": "value"},
			want:  nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			issuer := newTestIssuer(t)
			if tc.claim != nil {
				issuer.claims["groups"] = tc.claim
			}

			p := issuer.provider(t, func(cfg *Config) { cfg.GroupsClaim = tc.config })

			user, err := p.ExchangeCodeWithVerifier(t.Context(), "auth-code", "https://dbbat.test/cb", "v")
			require.NoError(t, err)
			assert.Equal(t, tc.want, user.Groups)
		})
	}
}

// TestGroupsClaimDefaultsToGroups pins the default claim name, since the
// mapping is inert without it.
func TestGroupsClaimDefaultsToGroups(t *testing.T) {
	t.Parallel()

	p, err := NewProvider(Config{Issuer: "https://issuer.example.test"})
	require.NoError(t, err)
	assert.Equal(t, "groups", p.cfg.GroupsClaim)
}

// TestExchangeCodeReadsCustomGroupsClaim proves the configured claim name is
// the one actually read — the Entra case, where membership lands in a claim
// the operator names in the token configuration.
func TestExchangeCodeReadsCustomGroupsClaim(t *testing.T) {
	t.Parallel()

	issuer := newTestIssuer(t)
	issuer.claims["groups"] = []any{"decoy"}
	issuer.claims["wids"] = []any{"62e90394-69f5-4237-9190-012177145e10"}

	p := issuer.provider(t, func(cfg *Config) { cfg.GroupsClaim = "wids" })

	user, err := p.ExchangeCodeWithVerifier(t.Context(), "auth-code", "https://dbbat.test/cb", "v")
	require.NoError(t, err)
	assert.Equal(t, []string{"62e90394-69f5-4237-9190-012177145e10"}, user.Groups)
}

func TestExchangeCodeRejectsBadSignature(t *testing.T) {
	t.Parallel()

	issuer := newTestIssuer(t)

	foreign, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	issuer.signingKey = foreign

	p := issuer.provider(t)

	_, err = p.ExchangeCodeWithVerifier(t.Context(), "auth-code", "https://dbbat.test/cb", "v")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "id token verification",
		"a token signed by a key outside the issuer's JWKS must be refused")
}

func TestExchangeCodeRejectsWrongAudience(t *testing.T) {
	t.Parallel()

	issuer := newTestIssuer(t)
	issuer.claims["aud"] = "some-other-client"

	p := issuer.provider(t)

	_, err := p.ExchangeCodeWithVerifier(t.Context(), "auth-code", "https://dbbat.test/cb", "v")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "id token verification")
}

func TestExchangeCodeRejectsWrongIssuer(t *testing.T) {
	t.Parallel()

	issuer := newTestIssuer(t)
	issuer.claims["iss"] = "https://evil.example.test"

	p := issuer.provider(t)

	_, err := p.ExchangeCodeWithVerifier(t.Context(), "auth-code", "https://dbbat.test/cb", "v")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "id token verification")
}

func TestExchangeCodeRejectsExpiredToken(t *testing.T) {
	t.Parallel()

	issuer := newTestIssuer(t)
	issuer.claims["exp"] = time.Now().Add(-time.Hour).Unix()

	p := issuer.provider(t)

	_, err := p.ExchangeCodeWithVerifier(t.Context(), "auth-code", "https://dbbat.test/cb", "v")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "id token verification")
}

func TestExchangeCodeRejectsMissingIDToken(t *testing.T) {
	t.Parallel()

	issuer := newTestIssuer(t)
	issuer.omitIDToken = true

	p := issuer.provider(t)

	_, err := p.ExchangeCodeWithVerifier(t.Context(), "auth-code", "https://dbbat.test/cb", "v")
	require.ErrorIs(t, err, ErrNoIDToken)
}

func TestExchangeCodeRejectsUnverifiedEmail(t *testing.T) {
	t.Parallel()

	issuer := newTestIssuer(t)
	issuer.claims["email_verified"] = false

	p := issuer.provider(t)

	_, err := p.ExchangeCodeWithVerifier(t.Context(), "auth-code", "https://dbbat.test/cb", "v")
	require.ErrorIs(t, err, ErrEmailNotVerified)
}

func TestExchangeCodeEnforcesEmailDomainAllowlist(t *testing.T) {
	t.Parallel()

	issuer := newTestIssuer(t)
	p := issuer.provider(t, func(cfg *Config) {
		cfg.EmailDomains = []string{"@Acme.test", " corp.example "}
	})

	_, err := p.ExchangeCodeWithVerifier(t.Context(), "auth-code", "https://dbbat.test/cb", "v")
	require.ErrorIs(t, err, ErrEmailDomainNotAllowed,
		"example.com is outside the allowlist and must be refused")
}

func TestExchangeCodeAcceptsAllowlistedDomain(t *testing.T) {
	t.Parallel()

	issuer := newTestIssuer(t)
	issuer.claims["email"] = "bob@ACME.test"

	p := issuer.provider(t, func(cfg *Config) {
		cfg.EmailDomains = []string{"@Acme.test", "corp.example"}
	})

	user, err := p.ExchangeCodeWithVerifier(t.Context(), "auth-code", "https://dbbat.test/cb", "v")
	require.NoError(t, err, "the allowlist must be case-insensitive and tolerate a leading @")
	assert.Equal(t, "bob@acme.test", user.Email)
}

func TestExchangeCodeRequiresEmailWhenAllowlisted(t *testing.T) {
	t.Parallel()

	issuer := newTestIssuer(t)
	delete(issuer.claims, "email")
	delete(issuer.claims, "email_verified")

	p := issuer.provider(t, func(cfg *Config) {
		cfg.EmailDomains = []string{"acme.test"}
	})

	_, err := p.ExchangeCodeWithVerifier(t.Context(), "auth-code", "https://dbbat.test/cb", "v")
	require.ErrorIs(t, err, ErrEmailRequired,
		"an allowlist with nothing to check against must fail closed")
}

func TestExchangeCodeWithoutPKCEOmitsVerifier(t *testing.T) {
	t.Parallel()

	issuer := newTestIssuer(t)
	p := issuer.provider(t)

	_, err := p.ExchangeCode(t.Context(), "auth-code", "https://dbbat.test/cb")
	require.NoError(t, err)
	assert.Empty(t, issuer.tokenForm().Get("code_verifier"))
}

func TestDiscoveryFailureSurfacesAsError(t *testing.T) {
	t.Parallel()

	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer dead.Close()

	p, err := NewProvider(Config{Issuer: dead.URL, ClientID: "id", ClientSecret: "secret"})
	require.NoError(t, err, "construction must never do network I/O")

	p.HTTPClient = dead.Client()

	_, _, err = p.AuthorizeURLWithPKCE(t.Context(), "state", "https://dbbat.test/cb")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "oidc discovery")

	assert.Empty(t, p.AuthorizeURL("state", "https://dbbat.test/cb"),
		"the non-PKCE path reports discovery failure as an empty URL")
}

// TestDiscoveryIsCached pins the lazy-discovery contract: the issuer is
// resolved once per process, not once per login, so a busy login page does not
// hammer the IdP's well-known endpoint.
func TestDiscoveryIsCached(t *testing.T) {
	t.Parallel()

	issuer := newTestIssuer(t)
	p := issuer.provider(t)

	for range 3 {
		_, _, err := p.AuthorizeURLWithPKCE(t.Context(), "state", "https://dbbat.test/cb")
		require.NoError(t, err)
	}

	issuer.mu.Lock()
	defer issuer.mu.Unlock()

	assert.Equal(t, 1, issuer.discoveryHits, "discovery must be resolved once and cached")
}
