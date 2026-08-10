package auth

import (
	"context"
	"encoding/json"
)

// OAuthProvider defines the contract for OAuth identity providers.
type OAuthProvider interface {
	// Name returns the provider identifier (e.g., "slack").
	Name() string
	// AuthorizeURL builds the URL to redirect the user to for authorization.
	AuthorizeURL(state, redirectURI string) string
	// ExchangeCode exchanges an authorization code for user info.
	ExchangeCode(ctx context.Context, code, redirectURI string) (*OAuthUser, error)
}

// PKCEProvider is an optional extension implemented by providers that use
// PKCE (RFC 7636). The code verifier is minted when the flow starts and must
// survive until the callback, so the two halves are surfaced separately: the
// caller persists the verifier alongside the CSRF state row and hands it back
// on exchange.
//
// A provider that does not implement this interface is driven through the
// plain OAuthProvider methods.
type PKCEProvider interface {
	OAuthProvider
	// AuthorizeURLWithPKCE builds the authorization URL and returns the code
	// verifier whose S256 challenge it carries.
	AuthorizeURLWithPKCE(ctx context.Context, state, redirectURI string) (authorizeURL, verifier string, err error)
	// ExchangeCodeWithVerifier exchanges an authorization code, presenting
	// the verifier minted by AuthorizeURLWithPKCE.
	ExchangeCodeWithVerifier(ctx context.Context, code, redirectURI, verifier string) (*OAuthUser, error)
}

// DisplayNamer is an optional extension implemented by providers whose login
// button label is operator-configured rather than fixed by the brand. The
// login page falls back to a name derived from the provider key when a
// provider does not implement it.
type DisplayNamer interface {
	DisplayName() string
}

// OAuthUser represents normalized user info from any OAuth provider.
type OAuthUser struct {
	ProviderID  string          // Provider-specific user ID
	Email       string          // User email
	DisplayName string          // User display name
	TeamID      string          // Optional workspace/org ID
	TeamName    string          // Optional workspace/org name
	AvatarURL   string          // Optional profile picture URL
	RawData     json.RawMessage // Full provider response
	// Groups is the directory group membership the provider asserted, read
	// out of a verified token. It is authorization input, so a provider must
	// only populate it from something the issuer signed.
	//
	// nil and empty mean the same thing to consumers — "this identity is in
	// none of the groups we asked about" — which, under a configured role
	// mapping, revokes the mapped roles. Whether a mapping is applied to a
	// given provider at all is decided by the caller, not by this field.
	Groups []string
}
