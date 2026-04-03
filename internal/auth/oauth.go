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

// OAuthUser represents normalized user info from any OAuth provider.
type OAuthUser struct {
	ProviderID  string          // Provider-specific user ID
	Email       string          // User email
	DisplayName string          // User display name
	TeamID      string          // Optional workspace/org ID
	TeamName    string          // Optional workspace/org name
	AvatarURL   string          // Optional profile picture URL
	RawData     json.RawMessage // Full provider response
}
