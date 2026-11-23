package api

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/archive"
	"github.com/fclairamb/dbbat/internal/store"
)

// An archived capture above the whole-fetch limit is served page by page with
// every chunk checked against the digests sealed by the real archiver, and a
// rewritten object is a 502, not altered rows.
func TestArchivedQueryRowsAreCheckedOnTheRangePath(t *testing.T) {
	t.Parallel()

	server, dataStore := setupChainTestServer(t)
	ctx := context.Background()
	suffix := "archrange"

	createTestUser(t, dataStore, "admin-"+suffix, "adminpass123", []string{store.RoleAdmin})
	token := loginUser(t, server, "admin-"+suffix, "adminpass123")

	conn := createChainedSession(t, dataStore, suffix, 1)

	queries, err := dataStore.ListQueries(ctx, store.QueryFilter{ConnectionID: &conn.UID})
	require.NoError(t, err)
	require.Len(t, queries, 1)

	query := queries[0]

	// About 13 MiB of incompressible JSON: above archive.WholeObjectLimit once
	// zstd is done with it.
	const total = 10_000

	rows := make([]store.PendingQueryRow, 0, 1000)

	for i := 1; i <= total; i++ {
		raw := make([]byte, 1024)
		_, err := rand.Read(raw)
		require.NoError(t, err)

		data := json.RawMessage(fmt.Sprintf(`{"i":%d,"blob":%q}`, i, base64.StdEncoding.EncodeToString(raw)))
		rows = append(rows, store.PendingQueryRow{
			QueryID: query.UID, RowNumber: i, RowData: data, RowSizeBytes: int64(len(data)),
		})

		if len(rows) == cap(rows) {
			require.NoError(t, dataStore.StoreQueryRows(ctx, rows))
			rows = rows[:0]
		}
	}

	dir := t.TempDir()

	a, err := archive.Open(ctx, "file://"+dir, dataStore, nil)
	require.NoError(t, err)

	t.Cleanup(func() { _ = a.Close() })

	server.SetArchive(a)

	seal, err := a.ArchiveConnection(ctx, store.ArchiveCandidate{
		UID: conn.UID, ServerUID: conn.DatabaseID, DisconnectedAt: time.Now(),
		ArchiveWindow: store.RetentionWindow{Seconds: 60, Source: "instance"},
	})
	require.NoError(t, err)
	require.Greater(t, seal.Bytes, int64(archive.WholeObjectLimit), "the object must be read over range requests")
	require.Equal(t, int64(archive.ChunkBytes), seal.ChunkBytes)
	require.Len(t, seal.ChunkSHA256, int(archive.ChunkCount(seal.Bytes, seal.ChunkBytes)))

	router := gin.New()
	router.Use(server.authMiddleware())
	router.GET("/api/v1/queries/:uid/rows", server.requireAdminOrViewer(), server.handleGetQueryRows)

	path := "/api/v1/queries/" + query.UID.String() + "/rows?limit=3"

	resp := doChainVerify(router, token, path)
	require.Equal(t, http.StatusOK, resp.Code, "response body: %s", resp.Body.String())

	var page store.QueryRowsResult
	require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &page))
	assert.Equal(t, int64(total), page.TotalRows)
	require.Len(t, page.Rows, 3)
	require.NotNil(t, page.Verified)
	assert.True(t, *page.Verified)

	// Rewrite the middle of the object after sealing.
	file := filepath.Join(dir, seal.Key)
	data, err := os.ReadFile(file)
	require.NoError(t, err)

	data[len(data)/2] ^= 0xff
	require.NoError(t, os.WriteFile(file, data, 0o600))

	// Page through to the altered chunk: the read fails there.
	cursor := ""
	failed := false

	for range total {
		p := "/api/v1/queries/" + query.UID.String() + "/rows?limit=1000"
		if cursor != "" {
			p += "&cursor=" + url.QueryEscape(cursor)
		}

		resp := doChainVerify(router, token, p)
		if resp.Code == http.StatusBadGateway {
			assert.Contains(t, resp.Body.String(), "archive object corrupted")

			failed = true

			break
		}

		require.Equal(t, http.StatusOK, resp.Code, "response body: %s", resp.Body.String())

		var pg store.QueryRowsResult
		require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &pg))

		if !pg.HasMore {
			break
		}

		cursor = pg.NextCursor
	}

	assert.True(t, failed, "an altered chunk is never served")
}
