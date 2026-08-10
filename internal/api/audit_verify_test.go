package api

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/store"
)

// chainTestKey is the HMAC subkey the chain-verification tests seal their
// rows with. Knowing it is what lets the leak test assert it never appears in
// a response.
var chainTestKey = bytes.Repeat([]byte{0x2b}, 32)

// setupChainTestServer builds a server whose store seals what it writes. The
// shared API test store is built without an encryption key, so chaining is off
// until the key is installed here — and every row written afterwards is
// chained.
func setupChainTestServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()

	server, dataStore := setupTestServer(t)
	dataStore.SetChainKey(chainTestKey)

	return server, dataStore
}

// newChainVerifyTestRouter mounts the verification routes with the same
// middleware chain as production (see server.go: both behind requireAdmin).
func newChainVerifyTestRouter(server *Server) *gin.Engine {
	router := gin.New()
	router.Use(server.authMiddleware())
	router.GET("/api/v1/audit/verify", server.requireAdmin(), server.handleVerifyAuditChain)
	router.GET("/api/v1/audit/verify/queries", server.requireAdmin(), server.handleVerifyQueryChains)

	return router
}

func doChainVerify(router *gin.Engine, token, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, http.NoBody)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	return w
}

// logChainedAuditEvents writes n chained audit events carrying a marker in
// their details, and returns the chain position of the last one.
func logChainedAuditEvents(t *testing.T, dataStore *store.Store, n int, marker string) int64 {
	t.Helper()

	ctx := context.Background()

	for i := range n {
		err := dataStore.LogAuditEvent(ctx, &store.AuditEvent{
			EventType: "chain_api_test",
			Details:   json.RawMessage(fmt.Sprintf(`{"i":%d,"marker":%q}`, i, marker)),
		})
		require.NoError(t, err)
	}

	var head int64

	row := dataStore.DB().QueryRowContext(ctx, "SELECT COALESCE(MAX(chain_seq), 0) FROM audit_log")
	require.NoError(t, row.Scan(&head))

	return head
}

// TestVerifyAuditChain_RequiresAdmin pins the role gate: verification is
// evidence about the audit trail itself and costs a full walk, so it is
// narrower than the audit list a viewer may read.
func TestVerifyAuditChain_RequiresAdmin(t *testing.T) {
	t.Parallel()

	server, dataStore := setupChainTestServer(t)
	suffix := "cvrole"

	createTestUser(t, dataStore, "viewer-"+suffix, "viewerpass123", []string{store.RoleViewer})
	createTestUser(t, dataStore, "connector-"+suffix, "connpass123", []string{store.RoleConnector})
	createTestUser(t, dataStore, "admin-"+suffix, "adminpass123", []string{store.RoleAdmin})

	router := newChainVerifyTestRouter(server)

	for _, path := range []string{"/api/v1/audit/verify", "/api/v1/audit/verify/queries"} {
		viewer := doChainVerify(router, loginUser(t, server, "viewer-"+suffix, "viewerpass123"), path)
		require.Equal(t, http.StatusForbidden, viewer.Code,
			"viewer must not verify %s: %s", path, viewer.Body.String())

		connector := doChainVerify(router, loginUser(t, server, "connector-"+suffix, "connpass123"), path)
		require.Equal(t, http.StatusForbidden, connector.Code,
			"connector must not verify %s: %s", path, connector.Body.String())

		admin := doChainVerify(router, loginUser(t, server, "admin-"+suffix, "adminpass123"), path)
		require.Equal(t, http.StatusOK, admin.Code,
			"admin must verify %s: %s", path, admin.Body.String())
	}
}

// TestVerifyAuditChain_CleanChain covers the happy path, including the two
// fields the spec singles out: the head MAC and the pre-anchor count.
func TestVerifyAuditChain_CleanChain(t *testing.T) {
	t.Parallel()

	server, dataStore := setupChainTestServer(t)
	suffix := "cvclean"

	createTestUser(t, dataStore, "admin-"+suffix, "adminpass123", []string{store.RoleAdmin})
	token := loginUser(t, server, "admin-"+suffix, "adminpass123")

	head := logChainedAuditEvents(t, dataStore, 5, "clean")

	router := newChainVerifyTestRouter(server)
	w := doChainVerify(router, token, "/api/v1/audit/verify")
	require.Equal(t, http.StatusOK, w.Code, "response body: %s", w.Body.String())

	var resp auditChainVerifyResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	require.Equal(t, "audit", resp.Chain)
	require.True(t, resp.Verified, "a chain nobody touched must verify: %s", w.Body.String())
	require.Nil(t, resp.Break)
	require.Equal(t, head, resp.HeadSeq)
	require.GreaterOrEqual(t, resp.Entries, int64(5))
	require.Equal(t, resp.Entries, resp.HeadSeq, "the walk ends on the last entry it verified")

	// The head MAC is a full HMAC-SHA256, hex.
	require.Len(t, resp.HeadMAC, 64)
	_, err := hex.DecodeString(resp.HeadMAC)
	require.NoError(t, err, "head_mac must be hex")

	// Everything in this database was written after the key was installed, so
	// nothing predates the anchor.
	require.Equal(t, int64(0), resp.UnverifiablePreAnchorEntries)
	require.False(t, resp.CheckedAt.IsZero())
	require.False(t, resp.Cached, "the first walk is not a cached answer")
}

// TestVerifyAuditChain_ReportsBreak is the headline case: someone with direct
// PostgreSQL access rewrote an entry, and the endpoint says where.
func TestVerifyAuditChain_ReportsBreak(t *testing.T) {
	t.Parallel()

	server, dataStore := setupChainTestServer(t)
	suffix := "cvbreak"

	createTestUser(t, dataStore, "admin-"+suffix, "adminpass123", []string{store.RoleAdmin})
	token := loginUser(t, server, "admin-"+suffix, "adminpass123")

	head := logChainedAuditEvents(t, dataStore, 4, "tampered-with")

	var tamperedUID string

	row := dataStore.DB().QueryRowContext(context.Background(),
		"UPDATE audit_log SET event_type = 'innocuous' WHERE chain_seq = ? RETURNING uid", head)
	require.NoError(t, row.Scan(&tamperedUID))

	router := newChainVerifyTestRouter(server)
	w := doChainVerify(router, token, "/api/v1/audit/verify")

	// A broken chain is a successful answer to the question asked — the error
	// is in the data, not in the request.
	require.Equal(t, http.StatusOK, w.Code, "response body: %s", w.Body.String())

	var resp auditChainVerifyResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	require.False(t, resp.Verified, "a rewritten entry must break the chain")
	require.NotNil(t, resp.Break)
	require.Equal(t, head, resp.Break.ChainSeq, "the break is reported at the rewritten entry")
	require.Equal(t, tamperedUID, resp.Break.UID)
	require.Contains(t, resp.Break.Reason, "modified")
	// The walk stops at the break, so it verified everything before it.
	require.Equal(t, head-1, resp.Entries)
}

// TestVerifyAuditChain_LeaksNothing is the reason verification was CLI-only
// until now. It asserts two things a future refactor must not undo: the
// response carries no chain key and no record content, and its field set is
// exactly what it claims to be.
func TestVerifyAuditChain_LeaksNothing(t *testing.T) {
	t.Parallel()

	server, dataStore := setupChainTestServer(t)
	suffix := "cvleak"

	createTestUser(t, dataStore, "admin-"+suffix, "adminpass123", []string{store.RoleAdmin})
	token := loginUser(t, server, "admin-"+suffix, "adminpass123")

	const marker = "d0-not-leak-this-audit-payload"

	head := logChainedAuditEvents(t, dataStore, 3, marker)

	// Break the chain too: the break path is the one carrying a free-text
	// reason, and is where a leak would most plausibly creep in.
	_, err := dataStore.DB().ExecContext(context.Background(),
		"UPDATE audit_log SET details = '{\"i\":999}'::jsonb WHERE chain_seq = ?", head)
	require.NoError(t, err)

	router := newChainVerifyTestRouter(server)
	w := doChainVerify(router, token, "/api/v1/audit/verify")
	require.Equal(t, http.StatusOK, w.Code)

	body := w.Body.String()

	require.NotContains(t, body, marker, "an audited record's content must never reach the response")
	require.NotContains(t, body, hex.EncodeToString(chainTestKey),
		"the chain key must never reach the response")
	require.NotContains(t, body, "chain_key")
	require.NotContains(t, body, "details")
	require.NotContains(t, body, "sql_text")

	// Field-set assertion: a widened response fails here rather than shipping.
	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &fields))

	expected := []string{
		"chain", "verified", "entries", "head_seq", "head_mac",
		"unverifiable_pre_anchor_entries", "break", "checked_at", "cached",
	}
	require.ElementsMatch(t, expected, keysOf(fields),
		"the audit verification response must expose exactly these fields")

	var brk map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(fields["break"], &brk))
	require.ElementsMatch(t, []string{"uid", "chain_seq", "reason"}, keysOf(brk),
		"a break must expose exactly these fields")
}

// withFields returns base plus extra, without writing into base's backing
// array — two expectations built from the same base must not clobber each
// other.
func withFields(base []string, extra ...string) []string {
	out := make([]string, 0, len(base)+len(extra))
	out = append(out, base...)

	return append(out, extra...)
}

// jsonFields decodes one JSON object into its raw fields.
func jsonFields(t *testing.T, body []byte) map[string]json.RawMessage {
	t.Helper()

	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(body, &fields))

	return fields
}

// jsonKeys is jsonFields for the common case of only caring about the field set.
func jsonKeys(t *testing.T, body []byte) []string {
	t.Helper()

	return keysOf(jsonFields(t, body))
}

func keysOf(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}

	return keys
}

// TestVerifyAuditChain_CachesTheWalk pins the bounding strategy: a walk is
// O(rows), so a second call inside the TTL must reuse the first one's outcome
// instead of scanning the table again.
func TestVerifyAuditChain_CachesTheWalk(t *testing.T) {
	t.Parallel()

	server, dataStore := setupChainTestServer(t)
	suffix := "cvcache"

	createTestUser(t, dataStore, "admin-"+suffix, "adminpass123", []string{store.RoleAdmin})
	token := loginUser(t, server, "admin-"+suffix, "adminpass123")

	logChainedAuditEvents(t, dataStore, 2, "cached")

	router := newChainVerifyTestRouter(server)

	first := doChainVerify(router, token, "/api/v1/audit/verify")
	require.Equal(t, http.StatusOK, first.Code)

	var firstResp auditChainVerifyResponse
	require.NoError(t, json.Unmarshal(first.Body.Bytes(), &firstResp))
	require.False(t, firstResp.Cached)

	// More entries land, but the cached answer is what comes back — which is
	// exactly the trade: freshness bounded to chainVerifyTTL, cost bounded to
	// one walk per scope per TTL.
	logChainedAuditEvents(t, dataStore, 2, "cached")

	second := doChainVerify(router, token, "/api/v1/audit/verify")
	require.Equal(t, http.StatusOK, second.Code)

	var secondResp auditChainVerifyResponse
	require.NoError(t, json.Unmarshal(second.Body.Bytes(), &secondResp))
	require.True(t, secondResp.Cached, "a repeat call inside the TTL must not walk again")
	require.Equal(t, firstResp.Entries, secondResp.Entries)
	require.Equal(t, firstResp.CheckedAt.UnixNano(), secondResp.CheckedAt.UnixNano())
}

// createChainedSession writes a connection with n chained statements and
// returns the connection.
func createChainedSession(t *testing.T, dataStore *store.Store, suffix string, n int) *store.Connection {
	t.Helper()

	ctx := context.Background()

	owner := createTestUser(t, dataStore, "owner-"+suffix, "ownerpass123", []string{store.RoleConnector})
	db := createTestDBEntry(t, dataStore, "db-"+suffix, true)

	conn, err := dataStore.CreateConnection(ctx, owner.UID, db.UID, "10.9.9.9")
	require.NoError(t, err)

	for i := range n {
		_, err := dataStore.CreateQuery(ctx, &store.Query{
			ConnectionID: conn.UID,
			SQLText:      fmt.Sprintf("SELECT %d FROM secrets_table_%s", i, suffix),
			ExecutedAt:   time.Now(),
		})
		require.NoError(t, err)
	}

	return conn
}

// TestVerifyQueryChains_SweepAndScope covers both shapes of the query-chain
// endpoint, and the head MAC a scoped walk reports.
func TestVerifyQueryChains_SweepAndScope(t *testing.T) {
	t.Parallel()

	server, dataStore := setupChainTestServer(t)
	suffix := "qcok"

	createTestUser(t, dataStore, "admin-"+suffix, "adminpass123", []string{store.RoleAdmin})
	token := loginUser(t, server, "admin-"+suffix, "adminpass123")

	conn := createChainedSession(t, dataStore, suffix, 3)

	router := newChainVerifyTestRouter(server)

	sweep := doChainVerify(router, token, "/api/v1/audit/verify/queries")
	require.Equal(t, http.StatusOK, sweep.Code, "response body: %s", sweep.Body.String())

	var sweepResp queryChainVerifyResponse
	require.NoError(t, json.Unmarshal(sweep.Body.Bytes(), &sweepResp))
	require.Equal(t, "queries", sweepResp.Chain)
	require.True(t, sweepResp.Verified, "response body: %s", sweep.Body.String())
	require.Equal(t, int64(1), sweepResp.Connections)
	require.Equal(t, int64(3), sweepResp.Statements)
	require.Equal(t, int64(0), sweepResp.ChainsWithTruncatedPrefix)
	require.Empty(t, sweepResp.ConnectionUID, "a sweep is not scoped to a session")
	require.Nil(t, sweepResp.HeadSeq, "an aggregate head over independent chains means nothing")

	scoped := doChainVerify(router, token, "/api/v1/audit/verify/queries?connection="+conn.UID.String())
	require.Equal(t, http.StatusOK, scoped.Code, "response body: %s", scoped.Body.String())

	var scopedResp queryChainVerifyResponse
	require.NoError(t, json.Unmarshal(scoped.Body.Bytes(), &scopedResp))
	require.True(t, scopedResp.Verified)
	require.Equal(t, conn.UID.String(), scopedResp.ConnectionUID)
	require.Equal(t, int64(3), scopedResp.Statements)
	require.NotNil(t, scopedResp.HeadSeq)
	require.Equal(t, int64(3), *scopedResp.HeadSeq)
	require.Len(t, scopedResp.HeadMAC, 64)

	// No statement text, and no chain key, anywhere in either answer.
	for _, body := range []string{sweep.Body.String(), scoped.Body.String()} {
		require.NotContains(t, body, "secrets_table_")
		require.NotContains(t, body, hex.EncodeToString(chainTestKey))
	}
}

// TestVerifyQueryChains_ReportsBreak tampers with a logged statement and
// asserts the break comes back with its position and its session.
func TestVerifyQueryChains_ReportsBreak(t *testing.T) {
	t.Parallel()

	server, dataStore := setupChainTestServer(t)
	suffix := "qcbreak"

	createTestUser(t, dataStore, "admin-"+suffix, "adminpass123", []string{store.RoleAdmin})
	token := loginUser(t, server, "admin-"+suffix, "adminpass123")

	conn := createChainedSession(t, dataStore, suffix, 4)

	_, err := dataStore.DB().ExecContext(context.Background(),
		"UPDATE queries SET sql_text = 'SELECT 1' WHERE connection_id = ? AND chain_seq = 3", conn.UID)
	require.NoError(t, err)

	router := newChainVerifyTestRouter(server)
	w := doChainVerify(router, token, "/api/v1/audit/verify/queries?connection="+conn.UID.String())
	require.Equal(t, http.StatusOK, w.Code, "response body: %s", w.Body.String())

	var resp queryChainVerifyResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	require.False(t, resp.Verified, "a rewritten statement must break the chain")
	require.NotNil(t, resp.Break)
	require.Equal(t, int64(3), resp.Break.ChainSeq)
	require.Equal(t, conn.UID.String(), resp.Break.ConnectionUID)
	require.Contains(t, resp.Break.Reason, "modified")
	require.Equal(t, int64(2), resp.Statements, "the walk stops at the break")

	// The rewritten text is not echoed back either.
	require.NotContains(t, w.Body.String(), "SELECT 1")
}

// TestVerifyQueryChains_LeaksNothing is the query-side twin of
// TestVerifyAuditChain_LeaksNothing, and matters more than it: a query row
// carries SQL text and bind parameters, so this is the response where a
// widening mistake would hurt most. Substring checks only find what you thought
// to look for, so all three shapes — sweep, scoped, and scoped-with-a-break —
// have their exact field set pinned.
func TestVerifyQueryChains_LeaksNothing(t *testing.T) {
	t.Parallel()

	server, dataStore := setupChainTestServer(t)
	suffix := "qcleak"

	createTestUser(t, dataStore, "admin-"+suffix, "adminpass123", []string{store.RoleAdmin})
	token := loginUser(t, server, "admin-"+suffix, "adminpass123")

	clean := createChainedSession(t, dataStore, suffix+"a", 3)
	tampered := createChainedSession(t, dataStore, suffix+"b", 4)

	router := newChainVerifyTestRouter(server)

	sweepFields := []string{
		"chain", "verified", "connections", "statements",
		"chains_with_truncated_prefix", "checked_at", "cached",
	}

	sweep := doChainVerify(router, token, "/api/v1/audit/verify/queries")
	require.Equal(t, http.StatusOK, sweep.Code, "response body: %s", sweep.Body.String())
	require.ElementsMatch(t, sweepFields, jsonKeys(t, sweep.Body.Bytes()),
		"a query-chain sweep must expose exactly these fields")

	scoped := doChainVerify(router, token, "/api/v1/audit/verify/queries?connection="+clean.UID.String())
	require.Equal(t, http.StatusOK, scoped.Code, "response body: %s", scoped.Body.String())
	require.ElementsMatch(t, withFields(sweepFields, "connection_uid", "head_seq", "head_mac"),
		jsonKeys(t, scoped.Body.Bytes()),
		"a scoped query-chain walk must expose exactly these fields")

	// Now the break path — the one carrying a free-text reason, and the one a
	// leak would most plausibly creep into. The tampered session has its own
	// cache scope, so this walk is a real one.
	_, err := dataStore.DB().ExecContext(context.Background(),
		"UPDATE queries SET sql_text = 'SELECT leaked_column' WHERE connection_id = ? AND chain_seq = 2",
		tampered.UID)
	require.NoError(t, err)

	broken := doChainVerify(router, token, "/api/v1/audit/verify/queries?connection="+tampered.UID.String())
	require.Equal(t, http.StatusOK, broken.Code, "response body: %s", broken.Body.String())

	var brokenResp queryChainVerifyResponse
	require.NoError(t, json.Unmarshal(broken.Body.Bytes(), &brokenResp))
	require.False(t, brokenResp.Verified, "response body: %s", broken.Body.String())

	fields := jsonFields(t, broken.Body.Bytes())
	require.ElementsMatch(t,
		withFields(sweepFields, "connection_uid", "head_seq", "head_mac", "break"),
		keysOf(fields),
		"a broken query-chain walk must expose exactly these fields")

	var brk map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(fields["break"], &brk))
	require.ElementsMatch(t, []string{"uid", "chain_seq", "connection_uid", "reason"}, keysOf(brk),
		"a query-chain break must expose exactly these fields")

	// And the belt-and-braces substring checks, on every shape.
	for _, body := range []string{sweep.Body.String(), scoped.Body.String(), broken.Body.String()} {
		require.NotContains(t, body, "secrets_table_", "statement text must never reach the response")
		require.NotContains(t, body, "leaked_column")
		require.NotContains(t, body, hex.EncodeToString(chainTestKey), "the chain key must never reach the response")
		require.NotContains(t, body, "parameters")
		require.NotContains(t, body, "sql_text")
	}
}

// TestVerifyQueryChains_RejectsBadConnectionUID keeps the 400 path honest.
func TestVerifyQueryChains_RejectsBadConnectionUID(t *testing.T) {
	t.Parallel()

	server, dataStore := setupChainTestServer(t)
	suffix := "qcbaduid"

	createTestUser(t, dataStore, "admin-"+suffix, "adminpass123", []string{store.RoleAdmin})
	token := loginUser(t, server, "admin-"+suffix, "adminpass123")

	router := newChainVerifyTestRouter(server)
	w := doChainVerify(router, token, "/api/v1/audit/verify/queries?connection=not-a-uuid")
	require.Equal(t, http.StatusBadRequest, w.Code, "response body: %s", w.Body.String())
}

// TestVerifyChains_WithoutChainKey covers the store that seals nothing. A
// serving process cannot land there — config always resolves a key — but
// answering 500 would read as a broken server rather than a configuration
// that never chained anything.
func TestVerifyChains_WithoutChainKey(t *testing.T) {
	t.Parallel()

	// Deliberately not setupChainTestServer: this store has no chain key.
	server, dataStore := setupTestServer(t)
	suffix := "cvnokey"

	createTestUser(t, dataStore, "admin-"+suffix, "adminpass123", []string{store.RoleAdmin})
	token := loginUser(t, server, "admin-"+suffix, "adminpass123")

	router := newChainVerifyTestRouter(server)

	for _, path := range []string{"/api/v1/audit/verify", "/api/v1/audit/verify/queries"} {
		w := doChainVerify(router, token, path)
		require.Equal(t, http.StatusConflict, w.Code, "%s response body: %s", path, w.Body.String())

		var body ErrorBody
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
		require.Equal(t, ErrCodeConflict, body.Code)
	}
}
