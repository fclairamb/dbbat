package api

import (
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/store"
)

// kubernetesRouter wires just the server endpoints these tests exercise.
func kubernetesRouter(server *Server) *gin.Engine {
	router := gin.New()
	router.Use(server.authMiddleware())
	router.POST("/api/v1/servers", server.requireAdmin(), server.handleCreateDatabase)
	router.GET("/api/v1/servers", server.handleListDatabases)
	router.PUT("/api/v1/servers/:uid", server.requireAdmin(), server.handleUpdateDatabase)
	router.GET("/api/v1/ssh-servers", server.requireAdmin(), server.handleListSSHServers)
	router.GET("/api/v1/tunnel-servers", server.requireAdmin(), server.handleListTunnelServers)

	return router
}

func kubernetesTestAdmin(t *testing.T) (*gin.Engine, *Server, string) {
	t.Helper()

	server, dataStore := setupTestServer(t)
	server.encryptionKey = dbTestEncryptionKey

	createTestUser(t, dataStore, "k8s-admin", "adminpass123", []string{store.RoleAdmin})
	token := loginUser(t, server, "k8s-admin", "adminpass123")

	return kubernetesRouter(server), server, token
}

func validClusterPayload() map[string]any {
	return map[string]any{
		"name":          "prod-cluster",
		"host":          "https://api.cluster.example.com",
		"port":          6443,
		"username":      "dbbat",
		"password":      "sa-token",
		"protocol":      "kubernetes",
		"k8s_ca_cert":   "-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n",
		"k8s_namespace": "data",
	}
}

func TestCreateKubernetesServer(t *testing.T) {
	t.Parallel()

	router, _, token := kubernetesTestAdmin(t)

	w, data := doJSON(t, router, "POST", "/api/v1/servers", token, validClusterPayload())
	require.Equal(t, 200, w.Code, w.Body.String())

	assert.Equal(t, "kubernetes", data["protocol"])
	assert.Equal(t, "data", data["k8s_namespace"])
	// The CA bundle is public challenge material and round-trips...
	assert.Contains(t, data["k8s_ca_cert"], "BEGIN CERTIFICATE")
	// ...the ServiceAccount token never does.
	assert.NotContains(t, w.Body.String(), "sa-token")
	// A dial path is never a grantable target.
	assert.Equal(t, false, data["listable"])
}

func TestCreateKubernetesServerRejectsIncompleteRows(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(map[string]any)
		wantMsg string
	}{
		{
			name:    "no token",
			mutate:  func(p map[string]any) { delete(p, "password") },
			wantMsg: "service account bearer token",
		},
		{
			name:    "no CA bundle and no opt-out",
			mutate:  func(p map[string]any) { delete(p, "k8s_ca_cert") },
			wantMsg: "k8s_ca_cert is required",
		},
		{
			name:    "no namespace",
			mutate:  func(p map[string]any) { delete(p, "k8s_namespace") },
			wantMsg: "k8s_namespace is required",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			router, _, token := kubernetesTestAdmin(t)

			payload := validClusterPayload()
			tc.mutate(payload)

			w, _ := doJSON(t, router, "POST", "/api/v1/servers", token, payload)
			require.Equal(t, 400, w.Code, w.Body.String())
			assert.Contains(t, w.Body.String(), tc.wantMsg)
		})
	}
}

// TestCreateKubernetesServerAllowsSkippingTLSVerification pins that the CA
// requirement has exactly one escape hatch, and that it is explicit.
func TestCreateKubernetesServerAllowsSkippingTLSVerification(t *testing.T) {
	t.Parallel()

	router, _, token := kubernetesTestAdmin(t)

	payload := validClusterPayload()
	delete(payload, "k8s_ca_cert")
	payload["k8s_insecure_skip_tls_verify"] = true

	w, data := doJSON(t, router, "POST", "/api/v1/servers", token, payload)
	require.Equal(t, 200, w.Code, w.Body.String())

	assert.Equal(t, true, data["k8s_insecure_skip_tls_verify"])
}

// TestTunnelServersListsBothKinds is what the target form's "via" dropdown
// depends on: one endpoint returning every dial path, while /ssh-servers stays
// the bastion-only management view.
func TestTunnelServersListsBothKinds(t *testing.T) {
	t.Parallel()

	router, _, token := kubernetesTestAdmin(t)

	w, _ := doJSON(t, router, "POST", "/api/v1/servers", token, validClusterPayload())
	require.Equal(t, 200, w.Code, w.Body.String())

	w, _ = doJSON(t, router, "POST", "/api/v1/servers", token, map[string]any{
		"name": "jump", "host": "bastion.example.com", "port": 22,
		"username": "www-data", "protocol": "ssh", "password": "hunter2",
	})
	require.Equal(t, 200, w.Code, w.Body.String())

	_, resp := doJSON(t, router, "GET", "/api/v1/tunnel-servers", token, nil)
	tunnels := resp["servers"].([]any)
	assert.Len(t, tunnels, 2)

	_, resp = doJSON(t, router, "GET", "/api/v1/ssh-servers", token, nil)
	sshOnly := resp["servers"].([]any)
	require.Len(t, sshOnly, 1)
	assert.Equal(t, "ssh", sshOnly[0].(map[string]any)["protocol"])

	// Neither kind may leak into the target listing.
	_, resp = doJSON(t, router, "GET", "/api/v1/servers", token, nil)
	targets := resp["databases"]
	if list, ok := targets.([]any); ok {
		for _, entry := range list {
			row, ok := entry.(map[string]any)
			if !ok {
				continue
			}
			assert.NotContains(t, []any{"ssh", "kubernetes"}, row["protocol"])
		}
	}
}

// TestTargetMayReferenceAKubernetesCluster is the store-level rule seen through
// the API: a via_uid pointing at a cluster row is accepted, one pointing at a
// database is not.
func TestTargetMayReferenceAKubernetesCluster(t *testing.T) {
	t.Parallel()

	router, _, token := kubernetesTestAdmin(t)

	_, cluster := doJSON(t, router, "POST", "/api/v1/servers", token, validClusterPayload())
	clusterUID := cluster["uid"].(string)

	w, _ := doJSON(t, router, "POST", "/api/v1/servers", token, map[string]any{
		"name": "pg-in-cluster", "host": "svc/postgres", "port": 5432,
		"database_name": "app", "username": "app", "password": "pw",
		"protocol": "postgresql", "via_uid": clusterUID,
	})
	require.Equal(t, 200, w.Code, w.Body.String())

	targetUID := func() string {
		_, r := doJSON(t, router, "GET", "/api/v1/servers", token, nil)
		for _, entry := range r["databases"].([]any) {
			row := entry.(map[string]any)
			if row["name"] == "pg-in-cluster" {
				return row["uid"].(string)
			}
		}

		t.Fatal("the created target was not listed")

		return ""
	}()

	// A cluster row is not grantable, so pointing a target's via at a *database*
	// must still fail.
	w, _ = doJSON(t, router, "PUT", "/api/v1/servers/"+targetUID, token, map[string]any{
		"via_uid": targetUID,
	})
	assert.Equal(t, 400, w.Code, w.Body.String())
}

func TestUpdateKubernetesServerMaterial(t *testing.T) {
	t.Parallel()

	router, _, token := kubernetesTestAdmin(t)

	w, cluster := doJSON(t, router, "POST", "/api/v1/servers", token, validClusterPayload())
	require.Equal(t, 200, w.Code, w.Body.String())
	uid := cluster["uid"].(string)

	w, _ = doJSON(t, router, "PUT", "/api/v1/servers/"+uid, token, map[string]any{
		"k8s_namespace": "staging",
		"k8s_ca_cert":   "-----BEGIN CERTIFICATE-----\nBBBB\n-----END CERTIFICATE-----\n",
	})
	require.Equal(t, 200, w.Code, w.Body.String())

	_, resp := doJSON(t, router, "GET", "/api/v1/tunnel-servers", token, nil)
	row := resp["servers"].([]any)[0].(map[string]any)
	assert.Equal(t, "staging", row["k8s_namespace"])
	assert.Contains(t, row["k8s_ca_cert"], "BBBB")
}
