package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/store"
)

var grantsTestKey = []byte("grantstest-key-01234567890123456")

// grantsRouter wires just the grant endpoints the priority tests exercise.
func grantsRouter(server *Server) *gin.Engine {
	router := gin.New()
	router.Use(server.authMiddleware())
	router.POST("/api/v1/grants", server.requireAdmin(), server.handleCreateGrant)
	router.GET("/api/v1/grants/:uid", server.handleGetGrant)

	return router
}

// TestCreateGrant_Priority covers the API contract for the priority field:
// omitted means "derive it from the controls", supplied means "use this", and
// either way the response echoes the resolved value so a caller never has to
// guess what was stored.
func TestCreateGrant_Priority(t *testing.T) {
	t.Parallel()

	server, dataStore := setupTestServer(t)
	ctx := context.Background()
	suffix := "grantprio"

	createTestUser(t, dataStore, "admin-"+suffix, "adminpass123", []string{store.RoleAdmin})
	adminToken := loginUser(t, server, "admin-"+suffix, "adminpass123")

	target := createTestUser(t, dataStore, "target-"+suffix, "targetpass123", []string{store.RoleConnector})

	database, err := dataStore.CreateServer(ctx, &store.Server{
		Name:         "prio-db-" + suffix,
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

	now := time.Now()
	body := func(controls []string, priority *int) map[string]any {
		payload := map[string]any{
			"user_id":     target.UID.String(),
			"database_id": database.UID.String(),
			"controls":    controls,
			"starts_at":   now.Format(time.RFC3339),
			"expires_at":  now.Add(time.Hour).Format(time.RFC3339),
		}

		if priority != nil {
			payload["priority"] = *priority
		}

		return payload
	}

	t.Run("omitted priority is derived from the controls", func(t *testing.T) {
		t.Parallel()

		cases := []struct {
			name     string
			controls []string
			want     float64
		}{
			{"full write", []string{}, float64(store.PriorityFullWrite)},
			{"restricted write", []string{store.ControlBlockDDL}, float64(store.PriorityRestrictedWrite)},
			{"read only", []string{store.ControlReadOnly}, float64(store.PriorityReadOnly)},
			{
				"read only beats a companion control",
				[]string{store.ControlBlockCopy, store.ControlReadOnly},
				float64(store.PriorityReadOnly),
			},
		}

		for _, tc := range cases {
			w, resp := doJSON(t, router, http.MethodPost, "/api/v1/grants", adminToken, body(tc.controls, nil))
			require.Equal(t, http.StatusOK, w.Code, "%s: %s", tc.name, w.Body.String())
			require.Equal(t, tc.want, resp["priority"], "%s", tc.name)
		}
	})

	t.Run("explicit priority is echoed back", func(t *testing.T) {
		t.Parallel()

		explicit := 7
		w, resp := doJSON(t, router, http.MethodPost, "/api/v1/grants", adminToken,
			body([]string{}, &explicit))
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		require.Equal(t, float64(explicit), resp["priority"])

		// And it survives a re-read, so the override was persisted rather than
		// only reflected in the create response.
		uid, ok := resp["uid"].(string)
		require.True(t, ok, "create response has no uid: %s", w.Body.String())

		w, fetched := doJSON(t, router, http.MethodGet, "/api/v1/grants/"+uid, adminToken, nil)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		require.Equal(t, float64(explicit), fetched["priority"])
	})

	t.Run("an out-of-range priority is rejected", func(t *testing.T) {
		t.Parallel()

		// priority is a smallint; anything wider must fail validation rather
		// than wrapping silently.
		tooBig := 40000
		w, resp := doJSON(t, router, http.MethodPost, "/api/v1/grants", adminToken,
			body([]string{}, &tooBig))
		require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
		require.Equal(t, "VALIDATION_ERROR", resp["code"])
	})
}
