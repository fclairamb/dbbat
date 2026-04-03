package slack

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProvider_Name(t *testing.T) {
	t.Parallel()

	p := NewProvider("id", "secret", "")
	assert.Equal(t, "slack", p.Name())
}

func TestProvider_AuthorizeURL(t *testing.T) {
	t.Parallel()

	p := NewProvider("my-client-id", "my-secret", "")
	u := p.AuthorizeURL("state123", "http://localhost:8080/callback")

	assert.Contains(t, u, "https://slack.com/openid/connect/authorize?")
	assert.Contains(t, u, "response_type=code")
	assert.Contains(t, u, "client_id=my-client-id")
	assert.Contains(t, u, "scope=openid+email+profile")
	assert.Contains(t, u, "redirect_uri="+
		"http%3A%2F%2Flocalhost%3A8080%2Fcallback")
	assert.Contains(t, u, "state=state123")
	assert.NotContains(t, u, "team=")
}

func TestProvider_AuthorizeURL_WithTeam(t *testing.T) {
	t.Parallel()

	p := NewProvider("my-client-id", "my-secret", "T0123TEAM")
	u := p.AuthorizeURL("state456", "http://localhost:8080/callback")

	assert.Contains(t, u, "team=T0123TEAM")
	assert.Contains(t, u, "client_id=my-client-id")
	assert.Contains(t, u, "state=state456")
}

func TestProvider_ExchangeCode(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "application/x-www-form-urlencoded", r.Header.Get("Content-Type"))

		if err := r.ParseForm(); err != nil {
			t.Errorf("ParseForm failed: %v", err)
			return
		}
		assert.Equal(t, "my-id", r.FormValue("client_id"))
		assert.Equal(t, "my-secret", r.FormValue("client_secret"))
		assert.Equal(t, "auth-code-123", r.FormValue("code"))
		assert.Equal(t, "http://localhost/callback", r.FormValue("redirect_uri"))

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(tokenResponse{
			OK:          true,
			AccessToken: "xoxp-test-token",
			TokenType:   "bearer",
		})
	})
	mux.HandleFunc("/userinfo", func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "Bearer xoxp-test-token", r.Header.Get("Authorization"))

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(userInfoResponse{
			OK:       true,
			Sub:      "U013ZGBT0SJ",
			Name:     "Jane Doe",
			Email:    "jane@example.com",
			Picture:  "https://avatars.slack.com/jane.png",
			TeamID:   "T0123TEAM",
			TeamName: "My Workspace",
		})
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	p := NewProvider("my-id", "my-secret", "")
	p.TokenURL = server.URL + "/token"
	p.UserInfoURL = server.URL + "/userinfo"

	user, err := p.ExchangeCode(context.Background(), "auth-code-123", "http://localhost/callback")
	require.NoError(t, err)

	assert.Equal(t, "U013ZGBT0SJ", user.ProviderID)
	assert.Equal(t, "jane@example.com", user.Email)
	assert.Equal(t, "Jane Doe", user.DisplayName)
	assert.Equal(t, "T0123TEAM", user.TeamID)
	assert.Equal(t, "My Workspace", user.TeamName)
	assert.Equal(t, "https://avatars.slack.com/jane.png", user.AvatarURL)
	assert.NotEmpty(t, user.RawData)
}

func TestProvider_ExchangeCode_TokenError(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(tokenResponse{
			OK:    false,
			Error: "invalid_code",
		})
	}))
	defer server.Close()

	p := NewProvider("id", "secret", "")
	p.TokenURL = server.URL

	_, err := p.ExchangeCode(context.Background(), "bad-code", "http://localhost/callback")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid_code")
}

func TestProvider_ExchangeCode_UserInfoError(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(tokenResponse{
			OK:          true,
			AccessToken: "good-token",
			TokenType:   "bearer",
		})
	})
	mux.HandleFunc("/userinfo", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(userInfoResponse{
			OK:    false,
			Error: "token_revoked",
		})
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	p := NewProvider("id", "secret", "")
	p.TokenURL = server.URL + "/token"
	p.UserInfoURL = server.URL + "/userinfo"

	_, err := p.ExchangeCode(context.Background(), "code", "http://localhost/callback")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "token_revoked")
}

func TestProvider_ExchangeCode_WrongTeam(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(tokenResponse{
			OK:          true,
			AccessToken: "token",
			TokenType:   "bearer",
		})
	})
	mux.HandleFunc("/userinfo", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(userInfoResponse{
			OK:       true,
			Sub:      "U001",
			Name:     "User",
			Email:    "user@other.com",
			TeamID:   "T9999OTHER",
			TeamName: "Other Workspace",
		})
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	p := NewProvider("id", "secret", "T0123EXPECTED")
	p.TokenURL = server.URL + "/token"
	p.UserInfoURL = server.URL + "/userinfo"

	_, err := p.ExchangeCode(context.Background(), "code", "http://localhost/callback")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "wrong workspace")
	assert.Contains(t, err.Error(), "T9999OTHER")
	assert.Contains(t, err.Error(), "T0123EXPECTED")
}
