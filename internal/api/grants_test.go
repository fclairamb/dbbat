package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/store"
)

var grantsTestKey = []byte("grantstest-key-01234567890123456")

// newTestDefinition persists a grant definition carrying the given shape.
// Grants have no shape of their own, so every grant these tests issue starts
// from one of these; name and slug are generated when the caller doesn't care.
func newTestDefinition(
	t *testing.T,
	dataStore *store.Store,
	createdBy uuid.UUID,
	shape store.GrantDefinition,
) *store.GrantDefinition {
	t.Helper()

	suffix := uuid.NewString()[:8]

	if shape.Name == "" {
		shape.Name = "api-def-" + suffix
	}

	if shape.Slug == "" {
		shape.Slug = "api-def-" + suffix
	}

	if shape.DurationSeconds == 0 {
		shape.DurationSeconds = 3600
	}

	shape.CreatedBy = createdBy

	def, err := dataStore.CreateGrantDefinition(context.Background(), &shape)
	require.NoError(t, err, "newTestDefinition")

	return def
}

// persistGrantWithShape persists a definition carrying `shape` and a grant
// instantiating it over the given window — the API-package equivalent of what
// tests used to express as a grant literal with inline controls.
func persistGrantWithShape(
	t *testing.T,
	dataStore *store.Store,
	shape store.GrantDefinition,
	userID, databaseID, grantedBy uuid.UUID,
	startsAt, expiresAt time.Time,
	priority int16,
) *store.Grant {
	t.Helper()

	def := newTestDefinition(t, dataStore, grantedBy, shape)

	grant := store.BuildGrantFromDefinition(def, userID, databaseID, grantedBy, startsAt)
	grant.ExpiresAt = expiresAt

	if priority != 0 {
		grant.Priority = priority
	}

	created, err := dataStore.CreateGrant(context.Background(), grant)
	require.NoError(t, err, "persistGrantWithShape")

	return created
}

// grantsRouter wires just the grant endpoints these tests exercise.
func grantsRouter(server *Server) *gin.Engine {
	router := gin.New()
	router.Use(server.authMiddleware())
	router.POST("/api/v1/grants", server.requireAdmin(), server.handleAssignGrant)
	router.GET("/api/v1/grants/:uid", server.handleGetGrant)

	return router
}

// priorityOf pulls the priority out of a decoded JSON body. encoding/json
// turns every number into a float64; converting back to int keeps the
// assertions exact rather than approximate.
func priorityOf(t *testing.T, resp map[string]any) int {
	t.Helper()

	raw, ok := resp["priority"].(float64)
	require.True(t, ok, "response carries no numeric priority: %v", resp)

	return int(raw)
}

// TestAssignGrant_Priority covers the API contract for the priority field.
// Priority is no longer something a caller can type at a grant: it comes from
// the definition being instantiated — derived from its controls, or pinned by
// its own explicit priority.
func TestAssignGrant_Priority(t *testing.T) {
	t.Parallel()

	server, dataStore := setupTestServer(t)
	ctx := context.Background()
	suffix := "grantprio"

	admin := createTestUser(t, dataStore, "admin-"+suffix, "adminpass123", []string{store.RoleAdmin})
	adminToken := loginUser(t, server, "admin-"+suffix, "adminpass123")

	target := createTestUser(t, dataStore, "target-"+suffix, "targetpass123", []string{store.RoleConnector})

	database, err := dataStore.CreateServer(ctx, &store.Server{
		Name:         "prio_db_" + suffix,
		Host:         "127.0.0.1",
		Port:         5432,
		DatabaseName: "prod",
		Username:     "pg",
		Password:     "secret",
		Protocol:     store.ProtocolPostgreSQL,
		SSLMode:      "disable",
	}, grantsTestKey)
	require.NoError(t, err)

	router := grantsRouter(server)

	assign := func(defRef string) map[string]any {
		return map[string]any{
			"grant_definition_id": defRef,
			"user_id":             target.UID.String(),
			"database_id":         database.UID.String(),
		}
	}

	t.Run("priority is derived from the definition's controls", func(t *testing.T) {
		t.Parallel()

		cases := []struct {
			name     string
			controls []string
			want     int
		}{
			{"full write", []string{}, int(store.PriorityFullWrite)},
			{"restricted write", []string{store.ControlBlockDDL}, int(store.PriorityRestrictedWrite)},
			{"read only", []string{store.ControlReadOnly}, int(store.PriorityReadOnly)},
			{
				"read only beats a companion control",
				[]string{store.ControlBlockCopy, store.ControlReadOnly},
				int(store.PriorityReadOnly),
			},
		}

		for _, tc := range cases {
			def := newTestDefinition(t, dataStore, admin.UID, store.GrantDefinition{Controls: tc.controls})

			w, resp := doJSON(t, router, http.MethodPost, "/api/v1/grants", adminToken, assign(def.UID.String()))
			require.Equal(t, http.StatusOK, w.Code, "%s: %s", tc.name, w.Body.String())
			require.Equal(t, tc.want, priorityOf(t, resp), "%s", tc.name)
		}
	})

	t.Run("a definition's explicit priority is stamped on the grant", func(t *testing.T) {
		t.Parallel()

		explicit := int16(7)
		def := newTestDefinition(t, dataStore, admin.UID, store.GrantDefinition{Priority: &explicit})

		w, resp := doJSON(t, router, http.MethodPost, "/api/v1/grants", adminToken, assign(def.UID.String()))
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		require.Equal(t, int(explicit), priorityOf(t, resp))

		// And it survives a re-read, so the value was persisted rather than
		// only reflected in the create response.
		uid, ok := resp["uid"].(string)
		require.True(t, ok, "assign response has no uid: %s", w.Body.String())

		w, fetched := doJSON(t, router, http.MethodGet, "/api/v1/grants/"+uid, adminToken, nil)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		require.Equal(t, int(explicit), priorityOf(t, fetched))
	})
}

// TestAssignGrant is the contract of the endpoint that replaced ad-hoc grant
// creation: a grant is an instance of a definition, it carries the
// definition's shape and none of its own, and the definition's own state
// decides whether it may be issued at all.
func TestAssignGrant(t *testing.T) {
	t.Parallel()

	server, dataStore := setupTestServer(t)
	ctx := context.Background()
	suffix := "grantassign"

	admin := createTestUser(t, dataStore, "admin-"+suffix, "adminpass123", []string{store.RoleAdmin})
	adminToken := loginUser(t, server, "admin-"+suffix, "adminpass123")

	target := createTestUser(t, dataStore, "target-"+suffix, "targetpass123", []string{store.RoleConnector})

	database, err := dataStore.CreateServer(ctx, &store.Server{
		Name:         "assign_db_" + suffix,
		Host:         "127.0.0.1",
		Port:         5432,
		DatabaseName: "prod",
		Username:     "pg",
		Password:     "secret",
		Protocol:     store.ProtocolPostgreSQL,
		SSLMode:      "disable",
	}, grantsTestKey)
	require.NoError(t, err)

	other, err := dataStore.CreateServer(ctx, &store.Server{
		Name:         "assign_other_db_" + suffix,
		Host:         "127.0.0.1",
		Port:         5432,
		DatabaseName: "other",
		Username:     "pg",
		Password:     "secret",
		Protocol:     store.ProtocolPostgreSQL,
		SSLMode:      "disable",
	}, grantsTestKey)
	require.NoError(t, err)

	router := grantsRouter(server)

	t.Run("the grant instantiates the definition", func(t *testing.T) {
		t.Parallel()

		maxQueries := int64(42)
		def := newTestDefinition(t, dataStore, admin.UID, store.GrantDefinition{
			Controls:        []string{store.ControlReadOnly},
			MaxQueryCounts:  &maxQueries,
			DurationSeconds: 7200,
		})

		w, resp := doJSON(t, router, http.MethodPost, "/api/v1/grants", adminToken, map[string]any{
			"grant_definition_id": def.UID.String(),
			"user_id":             target.UID.String(),
			"database_id":         database.UID.String(),
		})
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())

		require.Equal(t, def.UID.String(), resp["grant_definition_id"])

		// The shape is on the embedded definition, never on the grant itself.
		require.NotContains(t, resp, "controls")
		require.NotContains(t, resp, "max_query_counts")
		require.NotContains(t, resp, "approval_patterns")

		embedded, ok := resp["definition"].(map[string]any)
		require.True(t, ok, "assign response carries no definition: %s", w.Body.String())
		require.Equal(t, []any{store.ControlReadOnly}, embedded["controls"])

		// The window's length comes from the definition's duration.
		rawStartsAt, ok := resp["starts_at"].(string)
		require.True(t, ok, "assign response has no starts_at: %s", w.Body.String())

		rawExpiresAt, ok := resp["expires_at"].(string)
		require.True(t, ok, "assign response has no expires_at: %s", w.Body.String())

		startsAt, err := time.Parse(time.RFC3339, rawStartsAt)
		require.NoError(t, err)
		expiresAt, err := time.Parse(time.RFC3339, rawExpiresAt)
		require.NoError(t, err)
		require.InDelta(t, 7200.0, expiresAt.Sub(startsAt).Seconds(), 1.0)
	})

	t.Run("a slug resolves to the live definition", func(t *testing.T) {
		t.Parallel()

		def := newTestDefinition(t, dataStore, admin.UID, store.GrantDefinition{
			Slug:     "assign-by-slug",
			Name:     "assign by slug",
			Controls: []string{store.ControlReadOnly},
		})

		// Edit it: the slug now belongs to the new version, and assigning by
		// slug must issue that one rather than the archived original.
		def.Controls = []string{store.ControlBlockDDL}
		updated, err := dataStore.UpdateGrantDefinition(ctx, def)
		require.NoError(t, err)
		require.NotEqual(t, def.UID, updated.UID)

		w, resp := doJSON(t, router, http.MethodPost, "/api/v1/grants", adminToken, map[string]any{
			"grant_definition_id": "assign-by-slug",
			"user_id":             target.UID.String(),
			"database_id":         database.UID.String(),
		})
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		require.Equal(t, updated.UID.String(), resp["grant_definition_id"])
	})

	t.Run("an unknown definition is rejected", func(t *testing.T) {
		t.Parallel()

		w, resp := doJSON(t, router, http.MethodPost, "/api/v1/grants", adminToken, map[string]any{
			"grant_definition_id": "no-such-definition",
			"user_id":             target.UID.String(),
			"database_id":         database.UID.String(),
		})
		require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
		require.Equal(t, "VALIDATION_ERROR", resp["code"])
	})

	t.Run("a deactivated definition cannot be assigned", func(t *testing.T) {
		t.Parallel()

		def := newTestDefinition(t, dataStore, admin.UID, store.GrantDefinition{})
		require.NoError(t, dataStore.DeactivateGrantDefinition(ctx, def.UID))

		w, resp := doJSON(t, router, http.MethodPost, "/api/v1/grants", adminToken, map[string]any{
			"grant_definition_id": def.UID.String(),
			"user_id":             target.UID.String(),
			"database_id":         database.UID.String(),
		})
		require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
		require.Equal(t, "VALIDATION_ERROR", resp["code"])
	})

	t.Run("the definition's server group scope is enforced", func(t *testing.T) {
		t.Parallel()

		serverGroup, err := dataStore.CreateServerGroup(ctx, &store.ServerGroup{Name: "scope-" + suffix})
		require.NoError(t, err)
		require.NoError(t, dataStore.AddServerToGroup(ctx, serverGroup.UID, database.UID))

		def := newTestDefinition(t, dataStore, admin.UID, store.GrantDefinition{
			ServerGroupUIDs: []uuid.UUID{serverGroup.UID},
		})

		w, resp := doJSON(t, router, http.MethodPost, "/api/v1/grants", adminToken, map[string]any{
			"grant_definition_id": def.UID.String(),
			"user_id":             target.UID.String(),
			"database_id":         other.UID.String(),
		})
		require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
		require.Equal(t, "VALIDATION_ERROR", resp["code"])
	})

	t.Run("a non-admin cannot assign a grant", func(t *testing.T) {
		t.Parallel()

		createTestUser(t, dataStore, "plain-"+suffix, "plainpass123", []string{store.RoleConnector})
		plainToken := loginUser(t, server, "plain-"+suffix, "plainpass123")

		def := newTestDefinition(t, dataStore, admin.UID, store.GrantDefinition{})

		w, _ := doJSON(t, router, http.MethodPost, "/api/v1/grants", plainToken, map[string]any{
			"grant_definition_id": def.UID.String(),
			"user_id":             target.UID.String(),
			"database_id":         database.UID.String(),
		})
		require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	})
}
