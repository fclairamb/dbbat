package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/auth"
	"github.com/fclairamb/dbbat/internal/store"
)

// newFilterTestServer creates a server plus a target database to open sessions
// against.
func newFilterTestServer(t *testing.T) (*Server, *store.Store, *store.Server) {
	t.Helper()

	server, dataStore := setupTestServer(t)
	server.encryptionKey = dbTestEncryptionKey

	// The name is a slug (^[a-z0-9_]{1,63}$ — see store.ErrServerNameInvalid),
	// so it takes underscores, never the hyphens the other fixtures here use:
	// a server name is what a client puts in its connection string, and the
	// grammar is narrower than for a display name.
	db, err := dataStore.CreateServer(context.Background(), &store.Server{
		Name:         "obs_db_" + uuid.NewString()[:8],
		Host:         "localhost",
		Port:         5432,
		DatabaseName: "db",
		Username:     "u",
		Password:     "p",
		SSLMode:      "disable",
	}, dbTestEncryptionKey)
	require.NoError(t, err)

	return server, dataStore, db
}

// TestObservabilityFilters_RejectMalformedUUIDs pins the deliberate difference
// between the new parameters and the old ones.
//
// A filter that silently vanishes on a typo fails *open*: the page then shows
// more rows than were asked for, with nothing saying so. Every parameter added
// by this feature answers 400 instead. The pre-existing user_id / database_id /
// before keep their silent-ignore behavior for compatibility, which the second
// half of this test pins so the distinction is deliberate rather than
// accidental.
func TestObservabilityFilters_RejectMalformedUUIDs(t *testing.T) {
	t.Parallel()

	server, dataStore, _ := newFilterTestServer(t)
	createTestUser(t, dataStore, "admin", "adminpassword123", []string{"admin"})
	token := loginUser(t, server, "admin", "adminpassword123")
	router := server.setupRouter()

	strict := []string{"server_group_uid", "grant_uid", "grant_definition_uid"}

	for _, endpoint := range []string{"/api/v1/connections", "/api/v1/queries"} {
		for _, param := range strict {
			w := doGet(t, router, token, endpoint+"?"+param+"=not-a-uuid")
			assert.Equal(t, http.StatusBadRequest, w.Code,
				"%s?%s=not-a-uuid must be refused, not silently unfiltered: %s", endpoint, param, w.Body.String())
		}

		// The multi-valued enum refuses an unknown member too, and refuses the
		// whole request rather than dropping the bad one.
		w := doGet(t, router, token, endpoint+"?grant_provenance=approved,manual")
		assert.Equal(t, http.StatusBadRequest, w.Code,
			"%s with an unknown provenance value must be refused: %s", endpoint, w.Body.String())

		// The legacy parameters are still tolerated, on purpose.
		for _, param := range []string{"user_id", "database_id", "before"} {
			w := doGet(t, router, token, endpoint+"?"+param+"=not-a-uuid")
			assert.Equal(t, http.StatusOK, w.Code,
				"%s?%s is pre-existing and stays silent-ignore for compatibility: %s",
				endpoint, param, w.Body.String())
		}
	}

	// approval_status is /queries-only and likewise validated.
	w := doGet(t, router, token, "/api/v1/queries?approval_status=released")
	assert.Equal(t, http.StatusBadRequest, w.Code,
		"an unknown approval_status must be refused: %s", w.Body.String())

	w = doGet(t, router, token, "/api/v1/queries?approval_status=pending")
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
}

// TestObservabilityFilters_Accepted proves the parameters actually reach the
// store and narrow the result, rather than merely being parsed and dropped.
func TestObservabilityFilters_Accepted(t *testing.T) {
	t.Parallel()

	server, dataStore, db := newFilterTestServer(t)
	ctx := context.Background()

	admin := createTestUser(t, dataStore, "admin", "adminpassword123", []string{"admin"})
	token := loginUser(t, server, "admin", "adminpassword123")
	router := server.setupRouter()

	// A grant issued straight through the store: the `direct` provenance route.
	def, err := dataStore.CreateGrantDefinition(ctx, &store.GrantDefinition{
		Name:            "obs-def-" + uuid.NewString()[:8],
		Slug:            "obs-def-" + uuid.NewString()[:8],
		DurationSeconds: 3600,
		Controls:        []string{store.ControlReadOnly},
		CreatedBy:       admin.UID,
	})
	require.NoError(t, err)

	grant := store.BuildGrantFromDefinition(def, admin.UID, db.UID, admin.UID, time.Now().Add(-time.Minute))
	grant.ExpiresAt = time.Now().Add(time.Hour)
	created, err := dataStore.CreateGrant(ctx, grant)
	require.NoError(t, err)

	granted, err := dataStore.CreateConnection(ctx, admin.UID, db.UID, "10.0.0.1", store.WithGrantUID(created.UID))
	require.NoError(t, err)

	ungranted, err := dataStore.CreateConnection(ctx, admin.UID, db.UID, "10.0.0.2")
	require.NoError(t, err)

	listed := func(query string) []string {
		w := doGet(t, router, token, "/api/v1/connections"+query)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())

		var body struct {
			Connections []struct {
				UID string `json:"uid"`
			} `json:"connections"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))

		uids := make([]string, 0, len(body.Connections))
		for _, c := range body.Connections {
			uids = append(uids, c.UID)
		}

		return uids
	}

	assert.ElementsMatch(t, []string{granted.UID.String(), ungranted.UID.String()}, listed(""),
		"unfiltered, both sessions are visible")
	assert.Equal(t, []string{granted.UID.String()}, listed("?grant_uid="+created.UID.String()))
	assert.Equal(t, []string{granted.UID.String()}, listed("?grant_definition_uid="+def.UID.String()))
	assert.Equal(t, []string{granted.UID.String()}, listed("?grant_provenance=direct"),
		"the admin-issued grant has no approval on record")
	assert.Empty(t, listed("?grant_provenance=approved"))

	// An empty server group returns nothing — never "unfiltered".
	group, err := dataStore.CreateServerGroup(ctx, &store.ServerGroup{Name: "obs-grp-" + uuid.NewString()[:8]})
	require.NoError(t, err)
	assert.Empty(t, listed("?server_group_uid="+group.UID.String()),
		"an empty group must select zero rows, not every row")

	require.NoError(t, dataStore.AddServerToGroup(ctx, group.UID, db.UID))
	assert.ElementsMatch(t, []string{granted.UID.String(), ungranted.UID.String()},
		listed("?server_group_uid="+group.UID.String()),
		"membership is resolved live: adding the server makes its past sessions visible")
}

// TestConnectionsConnectorCannotWidenScope is the non-negotiable one: the
// connector scoping overwrite runs *after* every parameter is parsed, so a
// crafted user_id cannot widen what a connector sees. If the overwrite ever
// moves above the parsing, this test fails.
func TestConnectionsConnectorCannotWidenScope(t *testing.T) {
	t.Parallel()

	server, dataStore, db := newFilterTestServer(t)
	ctx := context.Background()

	admin := createTestUser(t, dataStore, "admin", "adminpassword123", []string{"admin"})
	connector := createTestUser(t, dataStore, "connector", "connectorpassword123", []string{"connector"})

	adminSession, err := dataStore.CreateConnection(ctx, admin.UID, db.UID, "10.0.0.1")
	require.NoError(t, err)

	ownSession, err := dataStore.CreateConnection(ctx, connector.UID, db.UID, "10.0.0.2")
	require.NoError(t, err)

	router := server.setupRouter()
	token := loginUser(t, server, "connector", "connectorpassword123")

	// The crafted user_id names the admin, and is combined with a legitimate
	// new filter so the request also exercises the new parsing path.
	w := doGet(t, router, token,
		"/api/v1/connections?user_id="+admin.UID.String()+"&grant_provenance=approved,auto,direct")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var body struct {
		Connections []struct {
			UID    string `json:"uid"`
			UserID string `json:"user_id"`
		} `json:"connections"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))

	for _, c := range body.Connections {
		assert.Equal(t, connector.UID.String(), c.UserID,
			"a connector must never see another user's session, whatever user_id they send")
		assert.NotEqual(t, adminSession.UID.String(), c.UID)
	}

	// And without the provenance filter they still see only their own.
	w = doGet(t, router, token, "/api/v1/connections?user_id="+admin.UID.String())
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Len(t, body.Connections, 1)
	assert.Equal(t, ownSession.UID.String(), body.Connections[0].UID)
}

// TestQueriesConnectorCannotWidenScope is the /queries half of the same
// guarantee. There is no scoping overwrite to protect here because the route
// is behind requireAdminOrViewer: a connector never reaches the handler at all,
// crafted user_id or not, and is answered 403 before any parsing happens.
func TestQueriesConnectorCannotWidenScope(t *testing.T) {
	t.Parallel()

	server, dataStore, _ := newFilterTestServer(t)

	admin := createTestUser(t, dataStore, "admin", "adminpassword123", []string{"admin"})
	createTestUser(t, dataStore, "connector", "connectorpassword123", []string{"connector"})

	router := server.setupRouter()
	token := loginUser(t, server, "connector", "connectorpassword123")

	for _, query := range []string{
		"",
		"?user_id=" + admin.UID.String(),
		"?user_id=" + admin.UID.String() + "&grant_provenance=approved",
	} {
		w := doGet(t, router, token, "/api/v1/queries"+query)
		assert.Equal(t, http.StatusForbidden, w.Code,
			"a connector must not reach GET /queries%s: %s", query, w.Body.String())
	}
}

// TestListLastConnections covers the bulk endpoint: one row per user, newest
// session, absence meaning never, and admin-or-viewer gating.
func TestListLastConnections(t *testing.T) {
	t.Parallel()

	server, dataStore, db := newFilterTestServer(t)
	ctx := context.Background()

	createTestUser(t, dataStore, "admin", "adminpassword123", []string{"admin"})
	viewer := createTestUser(t, dataStore, "viewer", "viewerpassword123", []string{"viewer"})
	// Never connected: must be absent, which is what lets the UI read absence
	// as "Never".
	createTestUser(t, dataStore, "connector", "connectorpassword123", []string{"connector"})

	_, err := dataStore.CreateConnectionAt(ctx, viewer.UID, db.UID, "10.0.0.1", time.Now().Add(-48*time.Hour))
	require.NoError(t, err)
	newest, err := dataStore.CreateConnectionAt(ctx, viewer.UID, db.UID, "10.0.0.2", time.Now().Add(-time.Hour))
	require.NoError(t, err)

	router := server.setupRouter()
	token := loginUser(t, server, "admin", "adminpassword123")

	w := doGet(t, router, token, "/api/v1/users/last-connections")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var body struct {
		LastConnections []struct {
			UserID      string    `json:"user_id"`
			Username    string    `json:"username"`
			ConnectedAt time.Time `json:"connected_at"`
		} `json:"last_connections"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Len(t, body.LastConnections, 1, "one row per user that has connected: %s", w.Body.String())

	got := body.LastConnections[0]
	assert.Equal(t, viewer.UID.String(), got.UserID)
	assert.Equal(t, "viewer", got.Username)
	assert.WithinDuration(t, newest.ConnectedAt, got.ConnectedAt, time.Second,
		"the newest session wins, not the oldest")

	// A viewer may read it; a connector may not — it enumerates everyone's
	// activity, like GET /connections.
	viewerToken := loginUser(t, server, "viewer", "viewerpassword123")
	assert.Equal(t, http.StatusOK,
		doGet(t, router, viewerToken, "/api/v1/users/last-connections").Code)

	connectorToken := loginUser(t, server, "connector", "connectorpassword123")
	assert.Equal(t, http.StatusForbidden,
		doGet(t, router, connectorToken, "/api/v1/users/last-connections").Code)

	// The literal segment coexists with /users/:uid.
	assert.Equal(t, http.StatusOK,
		doGet(t, router, token, "/api/v1/users/"+viewer.UID.String()).Code)
}

// lastLoginOf reads the stored timestamp, nil when never.
func lastLoginOf(t *testing.T, dataStore *store.Store, uid uuid.UUID) *time.Time {
	t.Helper()

	user, err := dataStore.GetUserByUID(context.Background(), uid)
	require.NoError(t, err)

	return user.LastLoginAt
}

// TestLastLoginStampedByInteractiveLoginOnly is the whole point of the column:
// it moves on a password login, and on nothing else that merely carries an
// existing credential. The moment a token check or an API-key request moves it,
// it stops meaning "last seen in the UI" and starts meaning "last HTTP request
// from anything".
func TestLastLoginStampedByInteractiveLoginOnly(t *testing.T) {
	t.Parallel()

	server, dataStore, _ := newFilterTestServer(t)
	ctx := context.Background()

	user := createTestUser(t, dataStore, "admin", "adminpassword123", []string{"admin"})
	require.Nil(t, lastLoginOf(t, dataStore, user.UID), "a fresh account has never signed in")

	token := loginUser(t, server, "admin", "adminpassword123")

	stamped := lastLoginOf(t, dataStore, user.UID)
	require.NotNil(t, stamped, "a password login must stamp last_login_at")

	router := server.setupRouter()

	// Validating the session token (GET /auth/me) must not move it.
	require.Equal(t, http.StatusOK, doGet(t, router, token, "/api/v1/auth/me").Code)
	afterMe := lastLoginOf(t, dataStore, user.UID)
	require.NotNil(t, afterMe)
	assert.True(t, afterMe.Equal(*stamped),
		"GET /auth/me moved last_login_at from %s to %s; carrying a token is not a login", stamped, afterMe)

	// Nor must an API-key authenticated request.
	_, plainKey, err := dataStore.CreateAPIKey(ctx, user.UID, "obs-key", nil)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/connections", nil)
	req.Header.Set("Authorization", "Bearer "+plainKey)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	afterKey := lastLoginOf(t, dataStore, user.UID)
	require.NotNil(t, afterKey)
	assert.True(t, afterKey.Equal(*stamped),
		"an API-key request moved last_login_at from %s to %s", stamped, afterKey)
}

// TestLastLoginStampedByOAuthCallback covers the SSO half: the callback (and
// the exchange behind it) are interactive logins too.
func TestLastLoginStampedByOAuthCallback(t *testing.T) {
	t.Parallel()

	server, dataStore, _ := newFilterTestServer(t)
	suffix := uuid.NewString()[:8]

	user := createTestUser(t, dataStore, "sso-"+suffix, "ssopassword123456", []string{"connector"})
	require.Nil(t, lastLoginOf(t, dataStore, user.UID))

	providerName := "slack-lastlogin-" + suffix
	provider := &recordingProvider{
		name: providerName,
		user: &auth.OAuthUser{
			ProviderID:  "ULASTLOGIN-" + suffix,
			Email:       "sso-" + suffix + "@example.com",
			DisplayName: "SSO User",
		},
	}
	server.oauthProviders[providerName] = provider

	// Link the identity to the existing local account so the callback resolves
	// to a user whose stamp we already know is nil.
	_, err := dataStore.CreateUserIdentity(context.Background(), &store.UserIdentity{
		UserID:     user.UID,
		Provider:   providerName,
		ProviderID: provider.user.ProviderID,
		Email:      provider.user.Email,
	})
	require.NoError(t, err)

	router := gin.New()
	router.GET("/api/v1/auth/"+providerName, server.handleOAuthAuthorize(providerName))
	router.GET("/api/v1/auth/"+providerName+"/callback", server.handleOAuthCallback(providerName))

	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/auth/"+providerName, nil))
	require.Equal(t, http.StatusFound, w.Code)
	require.NotEmpty(t, provider.lastState)

	w = httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet,
		"/api/v1/auth/"+providerName+"/callback?code=x&state="+provider.lastState, nil))
	require.Equal(t, http.StatusFound, w.Code, w.Body.String())

	assert.NotNil(t, lastLoginOf(t, dataStore, user.UID),
		"an OAuth callback is an interactive login and must stamp last_login_at")
}
