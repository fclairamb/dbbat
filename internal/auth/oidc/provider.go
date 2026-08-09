// Package oidc implements a generic, configurable-issuer OpenID Connect
// login provider — the one that lets an organization sign in to dbbat with
// Google Workspace, Okta, Microsoft Entra, Keycloak, Authentik or anything
// else that speaks OIDC discovery.
//
// It differs from the Slack provider in one security-relevant way: Slack is
// trusted to answer its own userInfo endpoint, whereas a generic issuer is
// only trusted through a **verified ID token**. Every identity this package
// returns comes from a JWT whose signature was checked against the issuer's
// JWKS, and whose `iss`, `aud` and `exp` were checked against the configured
// issuer and client id.
package oidc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"

	coreoidc "github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/fclairamb/dbbat/internal/auth"
)

var (
	// ErrNoIDToken is returned when the token endpoint answered without an
	// id_token — the response of an OAuth2 server that is not an OIDC one.
	ErrNoIDToken = errors.New("token response carries no id_token")
	// ErrEmailNotVerified is returned when the issuer explicitly reports the
	// email claim as unverified.
	ErrEmailNotVerified = errors.New("email is not verified by the identity provider")
	// ErrEmailDomainNotAllowed is returned when the verified email's domain
	// is outside the configured allowlist.
	ErrEmailDomainNotAllowed = errors.New("email domain is not allowed")
	// ErrEmailRequired is returned when a domain allowlist is configured but
	// the ID token carries no email claim to check it against.
	ErrEmailRequired = errors.New("identity provider returned no email claim")
	// ErrIssuerRequired is returned by NewProvider when no issuer is set.
	ErrIssuerRequired = errors.New("oidc issuer is required")
)

// ProviderName is the key this provider is registered under, and the value
// stored in the `provider` column of user identities and OAuth states.
const ProviderName = "oidc"

// Config is the operator-supplied configuration of the generic provider.
type Config struct {
	// Issuer is the OIDC issuer URL, e.g. "https://accounts.google.com".
	// Discovery is done against <issuer>/.well-known/openid-configuration.
	Issuer string
	// ClientID and ClientSecret identify this dbbat instance to the issuer.
	ClientID     string
	ClientSecret string
	// Scopes requested at authorization time. "openid" is always included.
	Scopes []string
	// Label is the login-button text, e.g. "Acme SSO".
	Label string
	// EmailDomains, when non-empty, is the allowlist the *verified* email
	// claim's domain must belong to. It is the generic equivalent of the
	// Slack provider's workspace gating.
	EmailDomains []string
}

// Provider implements auth.OAuthProvider, auth.PKCEProvider and
// auth.DisplayNamer against any OIDC-compliant issuer.
type Provider struct {
	cfg Config

	// HTTPClient, when set, is used for discovery, JWKS fetches and token
	// exchange. Tests point it at an httptest issuer.
	HTTPClient *http.Client

	// Discovery is lazy and cached: resolving it at construction time would
	// make dbbat's startup depend on the IdP being reachable, and would make
	// the provider impossible to build offline (tests, route-parity checks).
	mu         sync.Mutex
	discovered *coreoidc.Provider
}

// NewProvider builds a generic OIDC provider. It performs no network I/O:
// discovery happens on first use.
func NewProvider(cfg Config) (*Provider, error) {
	if cfg.Issuer == "" {
		return nil, ErrIssuerRequired
	}

	cfg.Scopes = normalizeScopes(cfg.Scopes)
	cfg.EmailDomains = normalizeDomains(cfg.EmailDomains)

	if cfg.Label == "" {
		cfg.Label = "SSO"
	}

	return &Provider{cfg: cfg}, nil
}

// normalizeScopes lower-cases nothing (scopes are case-sensitive) but drops
// blanks and guarantees "openid" is present — without it the issuer answers
// an OAuth2 token with no id_token, and this provider has nothing to verify.
func normalizeScopes(scopes []string) []string {
	out := make([]string, 0, len(scopes)+1)
	seen := make(map[string]bool, len(scopes)+1)

	for _, s := range scopes {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}

		seen[s] = true

		out = append(out, s)
	}

	if !seen[coreoidc.ScopeOpenID] {
		out = append([]string{coreoidc.ScopeOpenID}, out...)
	}

	return out
}

// normalizeDomains lower-cases the allowlist and tolerates entries written
// with a leading "@".
func normalizeDomains(domains []string) []string {
	out := make([]string, 0, len(domains))

	for _, d := range domains {
		d = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(d), "@")))
		if d == "" {
			continue
		}

		out = append(out, d)
	}

	return out
}

// Name returns "oidc".
func (p *Provider) Name() string {
	return ProviderName
}

// DisplayName returns the operator-configured login-button label.
func (p *Provider) DisplayName() string {
	return p.cfg.Label
}

// httpContext binds the configured HTTP client to ctx so go-oidc and
// x/oauth2 use it for discovery, JWKS and token requests.
func (p *Provider) httpContext(ctx context.Context) context.Context {
	if p.HTTPClient == nil {
		return ctx
	}

	return coreoidc.ClientContext(ctx, p.HTTPClient)
}

// provider resolves (and caches) the issuer's discovery document.
func (p *Provider) provider(ctx context.Context) (*coreoidc.Provider, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.discovered != nil {
		return p.discovered, nil
	}

	discovered, err := coreoidc.NewProvider(p.httpContext(ctx), p.cfg.Issuer)
	if err != nil {
		return nil, fmt.Errorf("oidc discovery for %q: %w", p.cfg.Issuer, err)
	}

	p.discovered = discovered

	return discovered, nil
}

// oauthConfig builds the x/oauth2 config for a given callback URL.
func (p *Provider) oauthConfig(discovered *coreoidc.Provider, redirectURI string) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     p.cfg.ClientID,
		ClientSecret: p.cfg.ClientSecret,
		Endpoint:     discovered.Endpoint(),
		RedirectURL:  redirectURI,
		Scopes:       p.cfg.Scopes,
	}
}

// AuthorizeURL satisfies auth.OAuthProvider. The API drives this provider
// through AuthorizeURLWithPKCE instead (it implements auth.PKCEProvider), so
// this is the degraded path: same URL, no code challenge. It returns "" when
// discovery fails, which the caller surfaces as a failed login rather than a
// redirect to nowhere.
func (p *Provider) AuthorizeURL(state, redirectURI string) string {
	discovered, err := p.provider(context.Background())
	if err != nil {
		return ""
	}

	return p.oauthConfig(discovered, redirectURI).AuthCodeURL(state)
}

// AuthorizeURLWithPKCE builds the authorization URL carrying an S256 code
// challenge, and returns the verifier the caller must persist alongside the
// OAuth state row. Some IdPs (and every sane deployment) require PKCE.
func (p *Provider) AuthorizeURLWithPKCE(
	ctx context.Context,
	state, redirectURI string,
) (string, string, error) {
	discovered, err := p.provider(ctx)
	if err != nil {
		return "", "", err
	}

	verifier := oauth2.GenerateVerifier()
	authorizeURL := p.oauthConfig(discovered, redirectURI).AuthCodeURL(state, oauth2.S256ChallengeOption(verifier))

	return authorizeURL, verifier, nil
}

// ExchangeCode exchanges an authorization code without PKCE.
func (p *Provider) ExchangeCode(ctx context.Context, code, redirectURI string) (*auth.OAuthUser, error) {
	return p.ExchangeCodeWithVerifier(ctx, code, redirectURI, "")
}

// idTokenClaims is the subset of standard OIDC claims mapped onto
// auth.OAuthUser. The full claim set is preserved separately in RawData.
type idTokenClaims struct {
	Subject       string `json:"sub"`
	Email         string `json:"email"`
	EmailVerified *bool  `json:"email_verified"`
	Name          string `json:"name"`
	Picture       string `json:"picture"`
	// Entra and a few others send the tenant/org id here; harmless elsewhere.
	TenantID string `json:"tid"`
	// Keycloak-style realm/organization hints, best effort.
	Organization string `json:"organization"`
}

// ExchangeCodeWithVerifier completes the OIDC code flow: it exchanges the
// authorization code, **verifies the ID token's signature, issuer, audience
// and expiry against the issuer's JWKS**, then applies the email-verification
// and domain-allowlist policy before returning a normalized user.
//
// Neither the client secret nor any token ever reaches an error string or a
// log line: failures are described, not quoted.
func (p *Provider) ExchangeCodeWithVerifier(
	ctx context.Context,
	code, redirectURI, codeVerifier string,
) (*auth.OAuthUser, error) {
	discovered, err := p.provider(ctx)
	if err != nil {
		return nil, err
	}

	ctx = p.httpContext(ctx)

	var opts []oauth2.AuthCodeOption
	if codeVerifier != "" {
		opts = append(opts, oauth2.VerifierOption(codeVerifier))
	}

	token, err := p.oauthConfig(discovered, redirectURI).Exchange(ctx, code, opts...)
	if err != nil {
		return nil, fmt.Errorf("token exchange: %w", err)
	}

	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok || rawIDToken == "" {
		return nil, ErrNoIDToken
	}

	// This is the whole point of the package: the identity is only as
	// trustworthy as this verification. It checks the JWT signature against
	// the issuer's JWKS, `iss` against the configured issuer, `aud` against
	// our client id, and the expiry.
	idToken, err := discovered.VerifierContext(ctx, &coreoidc.Config{ClientID: p.cfg.ClientID}).Verify(ctx, rawIDToken)
	if err != nil {
		return nil, fmt.Errorf("id token verification: %w", err)
	}

	var claims idTokenClaims
	if err := idToken.Claims(&claims); err != nil {
		return nil, fmt.Errorf("decoding id token claims: %w", err)
	}

	var rawClaims json.RawMessage
	if err := idToken.Claims(&rawClaims); err != nil {
		return nil, fmt.Errorf("capturing id token claims: %w", err)
	}

	email := strings.ToLower(strings.TrimSpace(claims.Email))

	// An issuer that bothers to tell us the address is unverified is telling
	// us not to trust it as an identity.
	if email != "" && claims.EmailVerified != nil && !*claims.EmailVerified {
		return nil, ErrEmailNotVerified
	}

	if err := p.checkEmailDomain(email); err != nil {
		return nil, err
	}

	return &auth.OAuthUser{
		ProviderID:  idToken.Subject,
		Email:       email,
		DisplayName: claims.Name,
		TeamID:      claims.TenantID,
		TeamName:    claims.Organization,
		AvatarURL:   claims.Picture,
		RawData:     rawClaims,
	}, nil
}

// checkEmailDomain enforces the optional allowlist against the *verified*
// email claim. No allowlist means no restriction.
func (p *Provider) checkEmailDomain(email string) error {
	if len(p.cfg.EmailDomains) == 0 {
		return nil
	}

	if email == "" {
		return ErrEmailRequired
	}

	at := strings.LastIndex(email, "@")
	if at < 0 {
		return fmt.Errorf("%w: malformed address", ErrEmailDomainNotAllowed)
	}

	domain := email[at+1:]
	for _, allowed := range p.cfg.EmailDomains {
		if domain == allowed {
			return nil
		}
	}

	return fmt.Errorf("%w: %q", ErrEmailDomainNotAllowed, domain)
}
