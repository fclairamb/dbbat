package api

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/auth"
	"github.com/fclairamb/dbbat/internal/config"
	"github.com/fclairamb/dbbat/internal/store"
)

type linkHarness struct {
	server   *Server
	store    *store.Store
	provider *recordingProvider
	name     string
	user     *store.User
	router   *gin.Engine
	method   *string
}

func newLinkHarness(t *testing.T, roles []string) *linkHarness {
	t.Helper()

	server, dataStore := setupTestServer(t)
	suffix := uuid.NewString()[:8]
	server.config = &config.Config{}

	name := "oidc-" + suffix
	provider := &recordingProvider{name: name, user: &auth.OAuthUser{
		ProviderID: "SUB-" + suffix, Email: "x-" + suffix + "@example.com", DisplayName: "X",
	}}
	server.oauthProviders[name] = provider

	user, err := dataStore.CreateUser(t.Context(), "link-"+suffix, "hash", roles)
	require.NoError(t, err)

	method := authMethodWebSession
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(contextKeyUser, user)
		c.Set(contextKeyAuthMethod, method)
		c.Next()
	})
	router.POST("/auth/"+name+"/link", server.requireWebSession(), server.handleOAuthLink(name))
	router.GET("/auth/"+name+"/callback", server.handleOAuthCallback(name))
	router.GET("/users/me/identities", server.handleListMyIdentities)
	router.DELETE("/users/me/identities/:provider", server.requireWebSession(), server.handleUnlinkMyIdentity)

	return &linkHarness{
		server: server, store: dataStore, provider: provider, name: name,
		user: user, router: router, method: &method,
	}
}

func (h *linkHarness) do(method, path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.router.ServeHTTP(w, httptest.NewRequest(method, path, nil))

	return w
}

// start runs POST /link and returns the state token the provider was handed.
func (h *linkHarness) start(t *testing.T) string {
	t.Helper()

	w := h.do(http.MethodPost, "/auth/"+h.name+"/link")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "authorize_url")
	require.NotEmpty(t, h.provider.lastState)

	return h.provider.lastState
}

func TestOAuthLinkFromSessionLinksAdminAndAudits(t *testing.T) {
	t.Parallel()

	h := newLinkHarness(t, []string{store.RoleAdmin})
	state := h.start(t)

	w := h.do(http.MethodGet, "/auth/"+h.name+"/callback?code=x&state="+state)
	require.Equal(t, http.StatusFound, w.Code)

	loc, err := url.Parse(w.Header().Get("Location"))
	require.NoError(t, err)
	assert.Equal(t, "/app/profile", loc.Path)
	assert.Equal(t, h.name, loc.Query().Get("linked"))

	got, err := h.store.GetUserByIdentity(t.Context(), h.name, h.provider.user.ProviderID)
	require.NoError(t, err)
	assert.Equal(t, h.user.UID, got.UID, "the identity must land on the session's user, admin or not")

	events, err := h.store.ListAuditEvents(t.Context(), store.AuditFilter{
		EventType: ptrOf(AuditEventUserIdentityLinked), UserID: &h.user.UID,
	})
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.Contains(t, string(events[0].Details), `"method": "session"`)

	// The state row is single use.
	w = h.do(http.MethodGet, "/auth/"+h.name+"/callback?code=x&state="+state)
	assert.Contains(t, w.Header().Get("Location"), "OAUTH_STATE_MISMATCH")
}

func TestOAuthLinkRefusesIdentityOwnedByAnotherUser(t *testing.T) {
	t.Parallel()

	h := newLinkHarness(t, []string{store.RoleConnector})
	other, err := h.store.CreateUser(t.Context(), "other-"+uuid.NewString()[:8], "hash", []string{store.RoleConnector})
	require.NoError(t, err)
	_, err = h.store.CreateUserIdentity(t.Context(), &store.UserIdentity{
		UserID: other.UID, Provider: h.name, ProviderID: h.provider.user.ProviderID,
	})
	require.NoError(t, err)

	state := h.start(t)
	w := h.do(http.MethodGet, "/auth/"+h.name+"/callback?code=x&state="+state)
	assert.Equal(t, http.StatusConflict, w.Code)

	ids, err := h.store.GetUserIdentities(t.Context(), h.user.UID)
	require.NoError(t, err)
	assert.Empty(t, ids)
}

func TestOAuthLinkRequiresWebSession(t *testing.T) {
	t.Parallel()

	h := newLinkHarness(t, []string{store.RoleAdmin})
	*h.method = authMethodAPIKey

	w := h.do(http.MethodPost, "/auth/"+h.name+"/link")
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Empty(t, h.provider.lastState, "no state row may be minted for an API key")

	w = h.do(http.MethodDelete, "/users/me/identities/"+h.name)
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestUnlinkMyIdentity(t *testing.T) {
	t.Parallel()

	h := newLinkHarness(t, []string{store.RoleConnector})
	state := h.start(t)
	h.do(http.MethodGet, "/auth/"+h.name+"/callback?code=x&state="+state)

	w := h.do(http.MethodGet, "/users/me/identities")
	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), h.name)

	w = h.do(http.MethodDelete, "/users/me/identities/"+h.name)
	require.Equal(t, http.StatusNoContent, w.Code)

	ids, err := h.store.GetUserIdentities(t.Context(), h.user.UID)
	require.NoError(t, err)
	assert.Empty(t, ids)

	events, err := h.store.ListAuditEvents(t.Context(), store.AuditFilter{
		EventType: ptrOf(AuditEventUserIdentityUnlinked), UserID: &h.user.UID,
	})
	require.NoError(t, err)
	assert.Len(t, events, 1)

	w = h.do(http.MethodDelete, "/users/me/identities/"+h.name)
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestUnlinkRefusesLastSignInMethod(t *testing.T) {
	t.Parallel()

	h := newLinkHarness(t, []string{store.RoleConnector})
	hash := unusablePasswordHash
	require.NoError(t, h.store.UpdateUser(t.Context(), h.user.UID, store.UserUpdate{PasswordHash: &hash}))
	h.user.PasswordHash = hash

	state := h.start(t)
	h.do(http.MethodGet, "/auth/"+h.name+"/callback?code=x&state="+state)

	w := h.do(http.MethodDelete, "/users/me/identities/"+h.name)
	require.Equal(t, http.StatusConflict, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "only sign-in method")

	ids, err := h.store.GetUserIdentities(t.Context(), h.user.UID)
	require.NoError(t, err)
	assert.Len(t, ids, 1, "the identity must survive the refused unlink")
}

func TestOAuthLinkExpiredStateFails(t *testing.T) {
	t.Parallel()

	h := newLinkHarness(t, []string{store.RoleConnector})
	token := "expired-" + uuid.NewString()
	_, err := h.store.CreateOAuthState(t.Context(), &store.OAuthState{
		State: token, Provider: h.name, LinkUserID: &h.user.UID,
	}, -time.Minute)
	require.NoError(t, err)

	w := h.do(http.MethodGet, "/auth/"+h.name+"/callback?code=x&state="+token)
	assert.Contains(t, w.Header().Get("Location"), "OAUTH_STATE_MISMATCH")

	ids, err := h.store.GetUserIdentities(t.Context(), h.user.UID)
	require.NoError(t, err)
	assert.Empty(t, ids)
}

func TestOAuthLinkStateProviderMismatchFails(t *testing.T) {
	t.Parallel()

	h := newLinkHarness(t, []string{store.RoleConnector})
	otherName := "other-" + uuid.NewString()[:8]
	h.server.oauthProviders[otherName] = &recordingProvider{name: otherName, user: h.provider.user}
	h.router.GET("/auth/"+otherName+"/callback", h.server.handleOAuthCallback(otherName))

	state := h.start(t) // state row is bound to h.name

	w := h.do(http.MethodGet, "/auth/"+otherName+"/callback?code=x&state="+state)
	assert.Contains(t, w.Header().Get("Location"), "OAUTH_STATE_MISMATCH")

	ids, err := h.store.GetUserIdentities(t.Context(), h.user.UID)
	require.NoError(t, err)
	assert.Empty(t, ids)
}

func ptrOf[T any](v T) *T { return &v }
