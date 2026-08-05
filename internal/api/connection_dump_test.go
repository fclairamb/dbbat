package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/dump"
	"github.com/fclairamb/dbbat/internal/store"
)

// newDumpTestRouter mounts the capture routes with the production middleware
// chain and role gates (see server.go).
func newDumpTestRouter(server *Server) *gin.Engine {
	gin.SetMode(gin.TestMode)

	router := gin.New()
	router.Use(server.authMiddleware())
	router.GET("/api/v1/connections/:uid/dump", server.requireAdminOrViewer(), server.handleGetConnectionDump)
	router.DELETE("/api/v1/connections/:uid/dump", server.requireAdmin(), server.handleDeleteConnectionDump)

	return router
}

func doDumpRequest(router *gin.Engine, method, token, uid string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "/api/v1/connections/"+uid+"/dump", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	return w
}

// enableDumpStorage points the server at a spool directory and a file:// blob
// bucket — the same code path an s3:// deployment takes, minus the cloud.
func enableDumpStorage(t *testing.T, server *Server, dataStore *store.Store) (string, *dump.Uploader) {
	t.Helper()

	spool := t.TempDir()
	server.config.Dump.Dir = spool

	uploader, err := dump.OpenUploader(t.Context(), dump.UploaderOptions{
		URL:        "file://" + t.TempDir(),
		SpoolDir:   spool,
		InstanceID: "api-test",
		Recorder:   dataStore,
	})
	require.NoError(t, err)

	server.SetDumpStorage(uploader)
	t.Cleanup(func() { _ = uploader.Close() })

	return spool, uploader
}

// uploadCapture runs a capture through the real write path — spool file, then
// the uploader — and returns the key once it has been recorded on the row.
// That is exactly what a proxy session does when it closes.
func uploadCapture(
	t *testing.T, dataStore *store.Store, uploader *dump.Uploader,
	spool string, uid uuid.UUID, body string,
) string {
	t.Helper()

	path := filepath.Join(spool, uid.String()+dump.FileExt)
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))

	uploader.Finish(t.Context(), uid)

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := dataStore.GetConnectionByUID(t.Context(), uid)
		require.NoError(t, err)

		if conn.DumpKey != "" {
			// The local copy is only dropped once the key is stored, so by
			// this point the bucket is the single source for this capture.
			require.NoFileExists(t, path)

			return conn.DumpKey
		}

		time.Sleep(10 * time.Millisecond)
	}

	t.Fatalf("capture for %s was never uploaded", uid)

	return ""
}

// TestGetConnectionDump_FallsBackToBlobStorage is the case local-only storage
// cannot serve at all: the capture was uploaded and the local copy is gone, so
// the only way to find it is the key recorded on the connection row.
func TestGetConnectionDump_FallsBackToBlobStorage(t *testing.T) { //nolint:paralleltest // shared database state
	server, dataStore := setupTestServer(t)
	suffix := "dumpremote"

	admin := createTestUser(t, dataStore, "admin-"+suffix, "adminpass123", []string{store.RoleAdmin})
	token := loginUser(t, server, "admin-"+suffix, "adminpass123")

	db := createTestDBEntry(t, dataStore, "db-"+suffix, true)
	conn, err := dataStore.CreateConnection(t.Context(), admin.UID, db.UID, "10.1.1.1")
	require.NoError(t, err)

	spool, uploader := enableDumpStorage(t, server, dataStore)
	uploadCapture(t, dataStore, uploader, spool, conn.UID, "remote-capture-bytes")

	router := newDumpTestRouter(server)
	w := doDumpRequest(router, http.MethodGet, token, conn.UID.String())

	require.Equal(t, http.StatusOK, w.Code, "response body: %s", w.Body.String())
	require.Equal(t, "remote-capture-bytes", w.Body.String())
	require.Equal(t, dump.ContentType, w.Header().Get("Content-Type"))
	require.Contains(t, w.Header().Get("Content-Disposition"), conn.UID.String()+dump.FileExt)
}

// TestGetConnectionDump_PrefersTheLocalSpool keeps the pre-existing behavior
// honest: a capture still in the spool is served without touching the bucket.
func TestGetConnectionDump_PrefersTheLocalSpool(t *testing.T) { //nolint:paralleltest // shared database state
	server, dataStore := setupTestServer(t)
	suffix := "dumplocal"

	admin := createTestUser(t, dataStore, "admin-"+suffix, "adminpass123", []string{store.RoleAdmin})
	token := loginUser(t, server, "admin-"+suffix, "adminpass123")

	db := createTestDBEntry(t, dataStore, "db-"+suffix, true)
	conn, err := dataStore.CreateConnection(t.Context(), admin.UID, db.UID, "10.1.1.1")
	require.NoError(t, err)

	spool, _ := enableDumpStorage(t, server, dataStore)
	local := filepath.Join(spool, conn.UID.String()+dump.FileExt)
	require.NoError(t, os.WriteFile(local, []byte("local-capture-bytes"), 0o600))

	router := newDumpTestRouter(server)
	w := doDumpRequest(router, http.MethodGet, token, conn.UID.String())

	require.Equal(t, http.StatusOK, w.Code, "response body: %s", w.Body.String())
	require.Equal(t, "local-capture-bytes", w.Body.String())
}

// TestGetConnectionDump_NotFoundWithoutACapture covers both 404 cases: captures
// disabled server-wide, and no capture for this connection.
func TestGetConnectionDump_NotFoundWithoutACapture(t *testing.T) { //nolint:paralleltest // shared database state
	server, dataStore := setupTestServer(t)
	suffix := "dumpmissing"

	admin := createTestUser(t, dataStore, "admin-"+suffix, "adminpass123", []string{store.RoleAdmin})
	token := loginUser(t, server, "admin-"+suffix, "adminpass123")

	db := createTestDBEntry(t, dataStore, "db-"+suffix, true)
	conn, err := dataStore.CreateConnection(t.Context(), admin.UID, db.UID, "10.1.1.1")
	require.NoError(t, err)

	router := newDumpTestRouter(server)

	// Captures disabled entirely — the default.
	require.Equal(t, http.StatusNotFound,
		doDumpRequest(router, http.MethodGet, token, conn.UID.String()).Code)

	enableDumpStorage(t, server, dataStore)

	// Enabled, but this connection has neither a spool file nor a stored key.
	require.Equal(t, http.StatusNotFound,
		doDumpRequest(router, http.MethodGet, token, conn.UID.String()).Code)
}

// TestDeleteConnectionDump_RemovesTheUploadedObject checks the delete path ends
// with the capture unreachable *and* the row no longer pointing at it.
func TestDeleteConnectionDump_RemovesTheUploadedObject(t *testing.T) { //nolint:paralleltest // shared database state
	server, dataStore := setupTestServer(t)
	suffix := "dumpdelete"

	admin := createTestUser(t, dataStore, "admin-"+suffix, "adminpass123", []string{store.RoleAdmin})
	token := loginUser(t, server, "admin-"+suffix, "adminpass123")

	db := createTestDBEntry(t, dataStore, "db-"+suffix, true)
	conn, err := dataStore.CreateConnection(t.Context(), admin.UID, db.UID, "10.1.1.1")
	require.NoError(t, err)

	spool, uploader := enableDumpStorage(t, server, dataStore)
	key := uploadCapture(t, dataStore, uploader, spool, conn.UID, "remote-capture-bytes")

	router := newDumpTestRouter(server)
	require.Equal(t, http.StatusNoContent,
		doDumpRequest(router, http.MethodDelete, token, conn.UID.String()).Code)

	stored, err := dataStore.GetConnectionByUID(t.Context(), conn.UID)
	require.NoError(t, err)
	require.Empty(t, stored.DumpKey)

	_, err = uploader.Open(t.Context(), key)
	require.ErrorIs(t, err, os.ErrNotExist)

	// Nothing left to delete.
	require.Equal(t, http.StatusNotFound,
		doDumpRequest(router, http.MethodDelete, token, conn.UID.String()).Code)
}

// TestConnectionDumpKeyIsNotAPISurface: the key names a bucket and a layout,
// which callers have no use for — they have the download endpoint.
func TestConnectionDumpKeyIsNotAPISurface(t *testing.T) { //nolint:paralleltest // shared database state
	server, dataStore := setupTestServer(t)
	suffix := "dumpkeyhidden"

	admin := createTestUser(t, dataStore, "admin-"+suffix, "adminpass123", []string{store.RoleAdmin})
	token := loginUser(t, server, "admin-"+suffix, "adminpass123")

	db := createTestDBEntry(t, dataStore, "db-"+suffix, true)
	conn, err := dataStore.CreateConnection(t.Context(), admin.UID, db.UID, "10.1.1.1")
	require.NoError(t, err)

	require.NoError(t, dataStore.SetConnectionDumpKey(t.Context(), conn.UID, "2026/08/05/api-test/x.pcapng"))

	router := newConnectionsTestRouter(server)
	w := doGetConnection(router, token, conn.UID.String())

	require.Equal(t, http.StatusOK, w.Code, "response body: %s", w.Body.String())
	require.NotContains(t, w.Body.String(), "dump_key")
	require.NotContains(t, w.Body.String(), "api-test")
}
