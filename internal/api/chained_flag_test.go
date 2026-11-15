package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/store"
)

// TestListings_FlagUnchainedRows pins the derived `chained` flag on the audit
// and query listings: a row whose chain_seq is NULL reads as unverified, a
// sealed one does not.
func TestListings_FlagUnchainedRows(t *testing.T) {
	t.Parallel()

	server, dataStore := setupChainTestServer(t)
	suffix := "chflag"
	ctx := context.Background()

	createTestUser(t, dataStore, "admin-"+suffix, "adminpass123", []string{store.RoleAdmin})
	token := loginUser(t, server, "admin-"+suffix, "adminpass123")

	conn := createChainedSession(t, dataStore, suffix, 2)
	logChainedAuditEvents(t, dataStore, 2, suffix)

	var forgedQuery, forgedAudit string

	require.NoError(t, dataStore.DB().QueryRowContext(ctx,
		"UPDATE queries SET chain_seq = NULL WHERE uid = (SELECT uid FROM queries WHERE connection_id = ? ORDER BY chain_seq LIMIT 1) RETURNING uid::text",
		conn.UID).Scan(&forgedQuery))
	require.NoError(t, dataStore.DB().QueryRowContext(ctx,
		"UPDATE audit_log SET chain_seq = NULL WHERE uid = (SELECT uid FROM audit_log WHERE event_type = 'chain_api_test' AND details->>'marker' = ? LIMIT 1) RETURNING uid::text",
		suffix).Scan(&forgedAudit))

	router := gin.New()
	router.Use(server.authMiddleware())
	router.GET("/api/v1/audit", server.requireAdminOrViewer(), server.handleListAudit)
	router.GET("/api/v1/queries", server.requireAdminOrViewer(), server.handleListQueries)

	get := func(path string, out any) {
		req := httptest.NewRequest(http.MethodGet, path, http.NoBody)
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), out))
	}

	var queries struct {
		Queries []struct {
			UID     string `json:"uid"`
			Chained *bool  `json:"chained"`
		} `json:"queries"`
	}

	get("/api/v1/queries?connection_id="+conn.UID.String(), &queries)
	require.Len(t, queries.Queries, 2)

	for _, q := range queries.Queries {
		require.NotNil(t, q.Chained, "chained must always be present")
		require.Equal(t, q.UID != forgedQuery, *q.Chained, "query %s", q.UID)
	}

	var audit struct {
		Events []struct {
			UID     string `json:"uid"`
			Chained *bool  `json:"chained"`
		} `json:"audit_events"`
	}

	get("/api/v1/audit?event_type=chain_api_test", &audit)
	require.NotEmpty(t, audit.Events)

	seen := false

	for _, e := range audit.Events {
		require.NotNil(t, e.Chained)

		if e.UID == forgedAudit {
			seen = true

			require.False(t, *e.Chained)
		} else {
			require.True(t, *e.Chained)
		}
	}

	require.True(t, seen, "the unchained audit row must be listed")
}
