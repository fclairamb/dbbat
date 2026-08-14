package api

import (
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
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

func kubernetesTestAdmin(t *testing.T) (*gin.Engine, string) {
	t.Helper()

	router, token, _ := kubernetesTestAdminWithStore(t)

	return router, token
}

// kubernetesTestAdminWithStore also hands back the store, for the tests that
// need to simulate what the *dialer* writes — the TOFU-learned CA bundle, which
// no API request can set.
func kubernetesTestAdminWithStore(t *testing.T) (*gin.Engine, string, *store.Store) {
	t.Helper()

	server, dataStore := setupTestServer(t)
	server.encryptionKey = dbTestEncryptionKey

	createTestUser(t, dataStore, "k8s-admin", "adminpass123", []string{store.RoleAdmin})
	token := loginUser(t, server, "k8s-admin", "adminpass123")

	return kubernetesRouter(server), token, dataStore
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

	router, token := kubernetesTestAdmin(t)

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
			name:    "no namespace",
			mutate:  func(p map[string]any) { delete(p, "k8s_namespace") },
			wantMsg: "k8s_namespace is required",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			router, token := kubernetesTestAdmin(t)

			payload := validClusterPayload()
			tc.mutate(payload)

			w, _ := doJSON(t, router, "POST", "/api/v1/servers", token, payload)
			require.Equal(t, 400, w.Code, w.Body.String())
			assert.Contains(t, w.Body.String(), tc.wantMsg)
		})
	}
}

// TestCreateKubernetesServerAllowsOmittingTheCABundle is the behavior change
// TOFU is built on: the CA bundle is no longer required, because a row that
// supplies none learns one on first connect. Without this there is no "no CA
// supplied" case for the pin to serve.
func TestCreateKubernetesServerAllowsOmittingTheCABundle(t *testing.T) {
	t.Parallel()

	router, token := kubernetesTestAdmin(t)

	payload := validClusterPayload()
	delete(payload, "k8s_ca_cert")

	w, data := doJSON(t, router, "POST", "/api/v1/servers", token, payload)
	require.Equal(t, 200, w.Code, w.Body.String())

	assert.Equal(t, "kubernetes", data["protocol"])
	// Nothing is pinned *yet*: the pin is the dialer's to write, on first connect.
	assert.Empty(t, data["k8s_ca_cert"])
	assert.Empty(t, data["k8s_learned_ca_cert"])
	// And it is still not the insecure escape hatch.
	assert.Empty(t, data["k8s_insecure_skip_tls_verify"])
}

// TestKubernetesLearnedCABundleIsReadOnlyAndResettable covers the two halves of
// the learned pin's lifecycle at the API: it round-trips separately from the
// supplied bundle, and there is an explicit way to forget it — the exit a
// rotated cluster CA needs.
func TestKubernetesLearnedCABundleIsReadOnlyAndResettable(t *testing.T) {
	t.Parallel()

	router, token, dataStore := kubernetesTestAdminWithStore(t)

	payload := validClusterPayload()
	delete(payload, "k8s_ca_cert")

	_, cluster := doJSON(t, router, "POST", "/api/v1/servers", token, payload)
	uid, ok := cluster["uid"].(string)
	require.True(t, ok, cluster)

	parsed := mustParseUID(t, uid)
	learned := "-----BEGIN CERTIFICATE-----\nLEARNED\n-----END CERTIFICATE-----\n"
	require.NoError(t, dataStore.SetKubernetesCACert(t.Context(), parsed, learned))

	row := findTunnelRow(t, router, token, uid)
	assert.Contains(t, row["k8s_learned_ca_cert"], "LEARNED")
	assert.Empty(t, row["k8s_ca_cert"], "a learned pin must never be reported as an operator-supplied one")

	// Exit #1: forget the learned value outright, so the next connect re-pins.
	w, _ := doJSON(t, router, "PUT", "/api/v1/servers/"+uid, token, map[string]any{
		"k8s_reset_learned_ca_cert": true,
	})
	require.Equal(t, 200, w.Code, w.Body.String())

	row = findTunnelRow(t, router, token, uid)
	assert.Empty(t, row["k8s_learned_ca_cert"])

	// Exit #2: paste a bundle. It supersedes anything learned, so the stale pin
	// must not survive alongside it.
	require.NoError(t, dataStore.SetKubernetesCACert(t.Context(), parsed, learned))

	w, _ = doJSON(t, router, "PUT", "/api/v1/servers/"+uid, token, map[string]any{
		"k8s_ca_cert": "-----BEGIN CERTIFICATE-----\nPASTED\n-----END CERTIFICATE-----\n",
	})
	require.Equal(t, 200, w.Code, w.Body.String())

	row = findTunnelRow(t, router, token, uid)
	assert.Contains(t, row["k8s_ca_cert"], "PASTED")
	assert.Empty(t, row["k8s_learned_ca_cert"], "pasting a bundle must retire the learned pin")
}

// findTunnelRow reads one cluster row back through the tunnel-servers listing.
func findTunnelRow(t *testing.T, router *gin.Engine, token, uid string) map[string]any {
	t.Helper()

	_, resp := doJSON(t, router, "GET", "/api/v1/tunnel-servers", token, nil)
	listed, ok := resp["servers"].([]any)
	require.True(t, ok, resp)

	for _, entry := range listed {
		row, ok := entry.(map[string]any)
		if ok && row["uid"] == uid {
			return row
		}
	}

	t.Fatalf("cluster row %s was not listed", uid)

	return nil
}

func mustParseUID(t *testing.T, uid string) uuid.UUID {
	t.Helper()

	parsed, err := uuid.Parse(uid)
	require.NoError(t, err)

	return parsed
}

// TestCreateKubernetesServerAllowsSkippingTLSVerification pins that the
// insecure escape hatch stays available, and that it is explicit.
func TestCreateKubernetesServerAllowsSkippingTLSVerification(t *testing.T) {
	t.Parallel()

	router, token := kubernetesTestAdmin(t)

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

	router, token := kubernetesTestAdmin(t)

	w, _ := doJSON(t, router, "POST", "/api/v1/servers", token, validClusterPayload())
	require.Equal(t, 200, w.Code, w.Body.String())

	w, _ = doJSON(t, router, "POST", "/api/v1/servers", token, map[string]any{
		"name": "jump", "host": "bastion.example.com", "port": 22,
		"username": "www-data", "protocol": "ssh", "password": "hunter2",
	})
	require.Equal(t, 200, w.Code, w.Body.String())

	_, resp := doJSON(t, router, "GET", "/api/v1/tunnel-servers", token, nil)
	tunnels, ok := resp["servers"].([]any)
	require.True(t, ok, resp)
	assert.Len(t, tunnels, 2)

	_, resp = doJSON(t, router, "GET", "/api/v1/ssh-servers", token, nil)
	sshOnly, ok := resp["servers"].([]any)
	require.True(t, ok, resp)
	require.Len(t, sshOnly, 1)
	first, ok := sshOnly[0].(map[string]any)
	require.True(t, ok, sshOnly[0])
	assert.Equal(t, "ssh", first["protocol"])

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

	router, token := kubernetesTestAdmin(t)

	_, cluster := doJSON(t, router, "POST", "/api/v1/servers", token, validClusterPayload())
	clusterUID, ok := cluster["uid"].(string)
	require.True(t, ok, cluster)

	w, _ := doJSON(t, router, "POST", "/api/v1/servers", token, map[string]any{
		"name": "pg-in-cluster", "host": "svc/postgres", "port": 5432,
		"database_name": "app", "username": "app", "password": "pw",
		"protocol": "postgresql", "via_uid": clusterUID,
	})
	require.Equal(t, 200, w.Code, w.Body.String())

	targetUID := func() string {
		_, r := doJSON(t, router, "GET", "/api/v1/servers", token, nil)
		listed, ok := r["databases"].([]any)
		require.True(t, ok, r)

		for _, entry := range listed {
			row, ok := entry.(map[string]any)
			if !ok || row["name"] != "pg-in-cluster" {
				continue
			}
			uid, ok := row["uid"].(string)
			require.True(t, ok, row)

			return uid
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

// TestUpdateKubernetesServerMayBlankTheCABundle: clearing ca_cert used to be
// refused, because it produced a row that neither pinned a CA nor admitted to
// skipping verification. It is now the way back to trust-on-first-use — what
// has not changed is that the dialer still refuses to fall back to the host's
// system trust store, so the row is weaker, never open.
func TestUpdateKubernetesServerMayBlankTheCABundle(t *testing.T) {
	t.Parallel()

	router, token := kubernetesTestAdmin(t)

	_, cluster := doJSON(t, router, "POST", "/api/v1/servers", token, validClusterPayload())
	uid, ok := cluster["uid"].(string)
	require.True(t, ok, cluster)

	w, _ := doJSON(t, router, "PUT", "/api/v1/servers/"+uid, token, map[string]any{
		"k8s_ca_cert": "",
	})
	require.Equal(t, 200, w.Code, w.Body.String())

	row := findTunnelRow(t, router, token, uid)
	assert.Empty(t, row["k8s_ca_cert"])
	assert.Empty(t, row["k8s_insecure_skip_tls_verify"])

	// The explicit opt-out stays usable, it just has to be stated.
	w, _ = doJSON(t, router, "PUT", "/api/v1/servers/"+uid, token, map[string]any{
		"k8s_insecure_skip_tls_verify": true,
	})
	require.Equal(t, 200, w.Code, w.Body.String())
}

func TestUpdateKubernetesServerCannotBlankTheNamespace(t *testing.T) {
	t.Parallel()

	router, token := kubernetesTestAdmin(t)

	_, cluster := doJSON(t, router, "POST", "/api/v1/servers", token, validClusterPayload())
	uid, ok := cluster["uid"].(string)
	require.True(t, ok, cluster)

	w, _ := doJSON(t, router, "PUT", "/api/v1/servers/"+uid, token, map[string]any{
		"k8s_namespace": "",
	})
	require.Equal(t, 400, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "k8s_namespace is required")
}

// TestUpdateValidatesTheProtocolItself covers the other half of the same hole:
// the update path used to accept any protocol string, and could turn an
// ordinary database row into a kubernetes one carrying no cluster material.
func TestUpdateValidatesTheProtocolItself(t *testing.T) {
	t.Parallel()

	router, token := kubernetesTestAdmin(t)

	w, _ := doJSON(t, router, "POST", "/api/v1/servers", token, map[string]any{
		"name": "plain-db", "host": "db.example.com", "port": 5432,
		"database_name": "app", "username": "app", "password": "pw",
		"protocol": "postgresql",
	})
	require.Equal(t, 200, w.Code, w.Body.String())

	_, listing := doJSON(t, router, "GET", "/api/v1/servers", token, nil)
	rows, ok := listing["databases"].([]any)
	require.True(t, ok, listing)
	require.Len(t, rows, 1)
	row, ok := rows[0].(map[string]any)
	require.True(t, ok, rows[0])
	uid, ok := row["uid"].(string)
	require.True(t, ok, row)

	// A protocol that does not exist.
	w, _ = doJSON(t, router, "PUT", "/api/v1/servers/"+uid, token, map[string]any{
		"protocol": "cockroach",
	})
	require.Equal(t, 400, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "protocol must be one of")

	// A real protocol whose required material this row does not carry.
	w, _ = doJSON(t, router, "PUT", "/api/v1/servers/"+uid, token, map[string]any{
		"protocol": "kubernetes",
	})
	require.Equal(t, 400, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "k8s_namespace is required")
}

func TestUpdateKubernetesServerMaterial(t *testing.T) {
	t.Parallel()

	router, token := kubernetesTestAdmin(t)

	w, cluster := doJSON(t, router, "POST", "/api/v1/servers", token, validClusterPayload())
	require.Equal(t, 200, w.Code, w.Body.String())
	uid, ok := cluster["uid"].(string)
	require.True(t, ok, cluster)

	w, _ = doJSON(t, router, "PUT", "/api/v1/servers/"+uid, token, map[string]any{
		"k8s_namespace": "staging",
		"k8s_ca_cert":   "-----BEGIN CERTIFICATE-----\nBBBB\n-----END CERTIFICATE-----\n",
	})
	require.Equal(t, 200, w.Code, w.Body.String())

	_, resp := doJSON(t, router, "GET", "/api/v1/tunnel-servers", token, nil)
	listed, ok := resp["servers"].([]any)
	require.True(t, ok, resp)
	require.Len(t, listed, 1)
	row, ok := listed[0].(map[string]any)
	require.True(t, ok, listed[0])
	assert.Equal(t, "staging", row["k8s_namespace"])
	assert.Contains(t, row["k8s_ca_cert"], "BBBB")
}
