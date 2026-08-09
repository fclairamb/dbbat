package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/store"
)

var mcpTestKey = []byte("mcptest-key-0123456789012345678a")

// mcpFixture is a running router with the MCP endpoint on, plus a user holding
// an API key.
type mcpFixture struct {
	router   *gin.Engine
	store    *store.Store
	user     *store.User
	plainKey string
}

func mcpTestServer(t *testing.T) *mcpFixture {
	t.Helper()

	server, dataStore := setupTestServer(t)
	server.config.MCP.Enabled = true

	suffix := strings.ToLower(t.Name())
	suffix = strings.NewReplacer("/", "-", "_", "-").Replace(suffix)

	user := createTestUser(t, dataStore, "agent-"+suffix, "agentpass123", []string{store.RoleConnector})

	_, plainKey, err := dataStore.CreateAPIKey(context.Background(), user.UID, "agent key", nil, mcpTestKey)
	require.NoError(t, err)

	return &mcpFixture{router: server.setupRouter(), store: dataStore, user: user, plainKey: plainKey}
}

// mcpRequest issues one JSON-RPC call at the endpoint.
func mcpRequest(t *testing.T, router *gin.Engine, auth string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()

	raw, err := json.Marshal(body)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/mcp", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")

	if auth != "" {
		req.Header.Set("Authorization", auth)
	}

	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	return w
}

func toolsListBody() map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list", "params": map[string]any{}}
}

// TestMCPRequiresAuthentication: the endpoint sits behind the ordinary
// authenticated group, so an anonymous agent gets nowhere.
func TestMCPRequiresAuthentication(t *testing.T) {
	t.Parallel()

	f := mcpTestServer(t)

	w := mcpRequest(t, f.router, "", toolsListBody())
	assert.Equal(t, http.StatusUnauthorized, w.Code)

	w = mcpRequest(t, f.router, "Bearer dbb_nosuchkey0123456789012345678", toolsListBody())
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// TestMCPRejectsRevokedKey: revocation has to bite immediately, which is one
// of the reasons the MCP server runs stateless.
func TestMCPRejectsRevokedKey(t *testing.T) {
	t.Parallel()

	f := mcpTestServer(t)
	ctx := context.Background()

	w := mcpRequest(t, f.router, "Bearer "+f.plainKey, toolsListBody())
	require.Equal(t, http.StatusOK, w.Code, "sanity: the key works before revocation")

	keys, err := f.store.ListAPIKeys(ctx, store.APIKeyFilter{UserID: &f.user.UID})
	require.NoError(t, err)
	require.NotEmpty(t, keys)
	require.NoError(t, f.store.RevokeAPIKey(ctx, keys[0].ID, f.user.UID))

	w = mcpRequest(t, f.router, "Bearer "+f.plainKey, toolsListBody())
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// TestMCPRejectsNonAPIKeyAuth: the key is also the password the loopback
// client authenticates to the proxy with, so Basic Auth — which would
// otherwise pass the shared middleware — is refused here.
func TestMCPRejectsNonAPIKeyAuth(t *testing.T) {
	t.Parallel()

	f := mcpTestServer(t)

	basic := "Basic " + base64.StdEncoding.EncodeToString([]byte(f.user.Username+":agentpass123"))

	w := mcpRequest(t, f.router, basic, toolsListBody())
	require.Equal(t, http.StatusForbidden, w.Code)

	var body ErrorBody
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, ErrCodeForbidden, body.Code)
}

// TestMCPListsTools pins the surface an agent sees.
func TestMCPListsTools(t *testing.T) {
	t.Parallel()

	f := mcpTestServer(t)

	w := mcpRequest(t, f.router, "Bearer "+f.plainKey, toolsListBody())
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	var resp struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}

	require.NoError(t, json.Unmarshal(mcpResponsePayload(t, w), &resp))

	names := make([]string, 0, len(resp.Result.Tools))
	for _, tool := range resp.Result.Tools {
		names = append(names, tool.Name)
	}

	assert.ElementsMatch(t, []string{"list_databases", "query", "describe", "await_approval"}, names)
}

// TestMCPQueryWithoutGrantIsRefused drives a real tools/call through the
// transport: a connector with no grants cannot reach any database, and the
// refusal never touches an executor.
func TestMCPQueryWithoutGrantIsRefused(t *testing.T) {
	t.Parallel()

	f := mcpTestServer(t)

	w := mcpRequest(t, f.router, "Bearer "+f.plainKey, map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "query",
			"arguments": map[string]any{"database": "prod-pg", "sql": "SELECT 1"},
		},
	})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	// Decoded loosely: MCP's own wire keys are camelCase, and mirroring them in
	// a Go struct would fight this repo's snake_case JSON convention for no
	// gain in a test.
	var resp map[string]any
	require.NoError(t, json.Unmarshal(mcpResponsePayload(t, w), &resp))

	result, ok := resp["result"].(map[string]any)
	require.True(t, ok, "no result in %v", resp)
	assert.Equal(t, true, result["isError"], "an ungranted database must be a tool error")
	assert.Contains(t, mcpResultText(t, result), "no active grant")
}

// TestMCPDisabledRemovesTheRoutes: turning the feature off must remove the
// endpoint, not leave it answering.
func TestMCPDisabledRemovesTheRoutes(t *testing.T) {
	t.Parallel()

	server, _ := setupTestServer(t)
	server.config.MCP.Enabled = false

	router := server.setupRouter()

	for _, route := range router.Routes() {
		assert.NotEqual(t, "/api/v1/mcp", route.Path, "%s /api/v1/mcp should not be registered", route.Method)
	}

	assert.Nil(t, server.mcp)
}

// mcpResultText concatenates the text content blocks of a tools/call result.
func mcpResultText(t *testing.T, result map[string]any) string {
	t.Helper()

	blocks, ok := result["content"].([]any)
	require.True(t, ok, "no content in %v", result)

	var sb strings.Builder

	for _, raw := range blocks {
		block, isMap := raw.(map[string]any)
		if !isMap {
			continue
		}

		if text, isText := block["text"].(string); isText {
			sb.WriteString(text)
		}
	}

	return sb.String()
}

// mcpResponsePayload extracts the JSON-RPC message from the response, which
// the Streamable HTTP transport may deliver either as a plain JSON body or as
// a single SSE event.
func mcpResponsePayload(t *testing.T, w *httptest.ResponseRecorder) []byte {
	t.Helper()

	body := w.Body.Bytes()

	if !strings.HasPrefix(w.Header().Get("Content-Type"), "text/event-stream") {
		return body
	}

	for _, line := range strings.Split(string(body), "\n") {
		if payload, ok := strings.CutPrefix(line, "data: "); ok {
			return []byte(payload)
		}
	}

	t.Fatalf("no SSE data frame in response: %s", body)

	return nil
}
