package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/fclairamb/dbbat/internal/auth"
)

var (
	// ErrWrongWorkspace is returned when the user's workspace doesn't match the expected one.
	ErrWrongWorkspace = errors.New("wrong workspace")
	// ErrUnexpectedStatus is returned when the HTTP response has a non-200 status code.
	ErrUnexpectedStatus = errors.New("unexpected status")
	// ErrSlackAPI is returned when the Slack API returns an error.
	ErrSlackAPI = errors.New("slack error")
)

const (
	defaultAuthorizeURL = "https://slack.com/openid/connect/authorize"
	defaultTokenURL     = "https://slack.com/api/openid.connect.token"
	defaultUserInfoURL  = "https://slack.com/api/openid.connect.userInfo"
)

// Provider implements auth.OAuthProvider for Slack OpenID Connect.
type Provider struct {
	clientID     string
	clientSecret string
	teamID       string // Optional workspace restriction
	httpClient   *http.Client

	// Allow overriding URLs for testing.
	TokenURL    string
	UserInfoURL string
}

// tokenResponse represents the Slack OIDC token endpoint response.
type tokenResponse struct {
	OK          bool   `json:"ok"`
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	Error       string `json:"error,omitempty"`
}

// userInfoResponse represents the Slack OIDC userinfo endpoint response.
type userInfoResponse struct {
	OK       bool   `json:"ok"`
	Sub      string `json:"sub"`
	Name     string `json:"name"`
	Email    string `json:"email"`
	Picture  string `json:"picture"`
	TeamID   string `json:"https://slack.com/team_id"`   //nolint:tagliatelle // Slack OIDC standard claim name
	TeamName string `json:"https://slack.com/team_name"` //nolint:tagliatelle // Slack OIDC standard claim name
	Error    string `json:"error,omitempty"`
}

// NewProvider creates a new Slack OIDC provider.
func NewProvider(clientID, clientSecret, teamID string) *Provider {
	return &Provider{
		clientID:     clientID,
		clientSecret: clientSecret,
		teamID:       teamID,
		httpClient:   http.DefaultClient,
		TokenURL:     defaultTokenURL,
		UserInfoURL:  defaultUserInfoURL,
	}
}

// Name returns "slack".
func (p *Provider) Name() string {
	return "slack"
}

// AuthorizeURL builds the Slack OIDC authorization URL.
func (p *Provider) AuthorizeURL(state, redirectURI string) string {
	params := url.Values{
		"response_type": {"code"},
		"client_id":     {p.clientID},
		"scope":         {"openid email profile"},
		"redirect_uri":  {redirectURI},
		"state":         {state},
	}

	if p.teamID != "" {
		params.Set("team", p.teamID)
	}

	return defaultAuthorizeURL + "?" + params.Encode()
}

// ExchangeCode exchanges an authorization code for user info.
// It calls the token endpoint, then the userinfo endpoint, checks team
// restrictions, and returns a normalized OAuthUser.
func (p *Provider) ExchangeCode(ctx context.Context, code, redirectURI string) (*auth.OAuthUser, error) {
	// 1. Exchange code for access token.
	token, err := p.exchangeToken(ctx, code, redirectURI)
	if err != nil {
		return nil, fmt.Errorf("token exchange: %w", err)
	}

	// 2. Fetch user info.
	userInfo, err := p.fetchUserInfo(ctx, token)
	if err != nil {
		return nil, fmt.Errorf("user info: %w", err)
	}

	// 3. Check team restriction.
	if p.teamID != "" && userInfo.TeamID != p.teamID {
		return nil, fmt.Errorf("%w: got %q, want %q", ErrWrongWorkspace, userInfo.TeamID, p.teamID)
	}

	// 4. Build raw data for metadata storage.
	rawData, _ := json.Marshal(userInfo)

	return &auth.OAuthUser{
		ProviderID:  userInfo.Sub,
		Email:       userInfo.Email,
		DisplayName: userInfo.Name,
		TeamID:      userInfo.TeamID,
		TeamName:    userInfo.TeamName,
		AvatarURL:   userInfo.Picture,
		RawData:     rawData,
	}, nil
}

func (p *Provider) exchangeToken(ctx context.Context, code, redirectURI string) (string, error) {
	data := url.Values{
		"client_id":     {p.clientID},
		"client_secret": {p.clientSecret},
		"code":          {code},
		"redirect_uri":  {redirectURI},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.TokenURL, strings.NewReader(data.Encode()))
	if err != nil {
		return "", fmt.Errorf("creating request: %w", err)
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("sending request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%w %d: %s", ErrUnexpectedStatus, resp.StatusCode, body)
	}

	var tokenResp tokenResponse
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return "", fmt.Errorf("decoding response: %w", err)
	}

	if !tokenResp.OK {
		return "", fmt.Errorf("%w: %s", ErrSlackAPI, tokenResp.Error)
	}

	return tokenResp.AccessToken, nil
}

func (p *Provider) fetchUserInfo(ctx context.Context, accessToken string) (*userInfoResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.UserInfoURL, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("sending request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w %d: %s", ErrUnexpectedStatus, resp.StatusCode, body)
	}

	var userInfo userInfoResponse
	if err := json.Unmarshal(body, &userInfo); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}

	if !userInfo.OK {
		return nil, fmt.Errorf("%w: %s", ErrSlackAPI, userInfo.Error)
	}

	return &userInfo, nil
}
