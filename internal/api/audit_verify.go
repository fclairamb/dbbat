package api

import (
	"context"
	"encoding/hex"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/fclairamb/dbbat/internal/store"
)

// Chain verification over the API.
//
// `dbbat audit verify` remains the authoritative way to check the chains: it
// runs where the key lives and can be run by someone who does not trust the
// serving process. This endpoint exists so an auditor's evidence script can
// collect the same numbers over REST like every other artifact on
// website/docs/compliance.md — with the standing caveat, documented there and
// in docs/audit-chain.md, that a compromised dbbat can answer
// {"verified": true} without walking anything — and that the caching below
// makes an answer up to chainVerifyTTL old, so a chain broken moments ago keeps
// reporting verified until the cached walk expires. Monitoring signal, not a
// point-in-time attestation.
//
// Two rules shape what comes back:
//
//   - The response carries counts, positions, the head MAC and the
//     human-readable break reason. It never carries the chain key (which never
//     leaves the process, not even to the CLI's output) and never carries a
//     row's content — no sql_text, no parameters, no audit details. A MAC is
//     safe to publish: it is the value an operator is meant to record outside
//     the database.
//   - A walk is O(rows). See chainVerifyCache for how that is bounded.

// chainVerifyTTL is how long a completed walk answers later callers. An audit
// chain does not change meaningfully in a minute, and an admin refreshing a
// dashboard must not turn into one table scan per refresh.
const chainVerifyTTL = 60 * time.Second

// chainVerifyCacheMax caps the number of remembered scopes. Only three shapes
// exist ("audit", "queries" and one per connection), but the per-connection
// shape is caller-chosen, so the map needs a ceiling.
const chainVerifyCacheMax = 64

// chainVerifyCache bounds what an authenticated admin can make the store do.
//
// The spec offered two ways to bound the walk — a ?limit / ?since_seq window,
// or running it out of band and caching. The window was rejected: a partial
// walk cannot resume from a caller-supplied position without trusting that
// position's prev_mac, which is exactly the value an attacker who rewrote the
// chain controls — a windowed "verified" would be a weaker claim wearing the
// same name. So the whole chain is always walked, and the cost is bounded on
// the other side:
//
//   - the outcome of a walk is cached per scope for chainVerifyTTL, so
//     repeated calls read memory;
//   - at most one walk runs at a time process-wide (the gate), so a caller
//     cycling connection uids to miss the cache queues behind itself instead of
//     multiplying concurrent table scans.
type chainVerifyCache struct {
	mu      sync.Mutex
	entries map[string]chainVerifyEntry
	// gate admits a single walk at a time. Buffered with one slot; taking it
	// is context-aware so a client that disconnects stops waiting.
	gate chan struct{}
}

type chainVerifyEntry struct {
	body     any
	computed time.Time
}

func newChainVerifyCache() *chainVerifyCache {
	return &chainVerifyCache{
		entries: make(map[string]chainVerifyEntry),
		gate:    make(chan struct{}, 1),
	}
}

// lookup returns a cached body when it is still fresh.
func (cv *chainVerifyCache) lookup(scope string) (any, bool) {
	cv.mu.Lock()
	defer cv.mu.Unlock()

	entry, ok := cv.entries[scope]
	if !ok || time.Since(entry.computed) > chainVerifyTTL {
		return nil, false
	}

	return entry.body, true
}

// remember stores a body, evicting the oldest entry when the map is full.
func (cv *chainVerifyCache) remember(scope string, body any) {
	cv.mu.Lock()
	defer cv.mu.Unlock()

	if len(cv.entries) >= chainVerifyCacheMax {
		var (
			oldestScope string
			oldest      time.Time
		)

		for key, entry := range cv.entries {
			if oldestScope == "" || entry.computed.Before(oldest) {
				oldestScope, oldest = key, entry.computed
			}
		}

		delete(cv.entries, oldestScope)
	}

	cv.entries[scope] = chainVerifyEntry{body: body, computed: time.Now()}
}

// result serves scope from the cache, or runs walk under the single-walk gate.
// The bool reports whether the answer came from the cache.
func (cv *chainVerifyCache) result(
	ctx context.Context, scope string, walk func(context.Context) (any, error),
) (any, bool, error) {
	if body, ok := cv.lookup(scope); ok {
		return body, true, nil
	}

	select {
	case cv.gate <- struct{}{}:
	case <-ctx.Done():
		return nil, false, ctx.Err()
	}

	defer func() { <-cv.gate }()

	// Re-check under the gate: a caller that queued behind another walk of the
	// same scope should serve that walk's result rather than repeat it.
	if body, ok := cv.lookup(scope); ok {
		return body, true, nil
	}

	body, err := walk(ctx)
	if err != nil {
		return nil, false, err
	}

	cv.remember(scope, body)

	return body, false, nil
}

// chainBreakBody names the first record that failed to verify. It identifies
// the record and says what did not add up; it never echoes the record's
// content.
type chainBreakBody struct {
	UID           string `json:"uid"`
	ChainSeq      int64  `json:"chain_seq"`
	ConnectionUID string `json:"connection_uid,omitempty"`
	// QueryUID is set for captured-row-chain breaks; chain_seq then carries the
	// offending row's row_number rather than a chain position.
	QueryUID string `json:"query_uid,omitempty"`
	Reason   string `json:"reason"`
}

func newChainBreakBody(brk *store.ChainBreak) *chainBreakBody {
	if brk == nil {
		return nil
	}

	body := &chainBreakBody{
		UID:      brk.UID.String(),
		ChainSeq: brk.ChainSeq,
		Reason:   brk.Reason,
	}

	if brk.ConnectionUID != nil {
		body.ConnectionUID = brk.ConnectionUID.String()
	}

	if brk.QueryUID != nil {
		body.QueryUID = brk.QueryUID.String()
	}

	return body
}

// auditChainVerifyResponse mirrors what `dbbat audit verify` logs.
type auditChainVerifyResponse struct {
	Chain    string `json:"chain"`
	Verified bool   `json:"verified"`
	// Entries is how many chained rows were walked — up to the break, when
	// there is one, because nothing after a break means anything.
	Entries int64 `json:"entries"`
	HeadSeq int64 `json:"head_seq"`
	// HeadMAC is the chain head, hex. Record it outside the database: it is
	// what detects a chain truncated and re-sealed by someone who had the key.
	HeadMAC string `json:"head_mac"`
	// UnverifiablePreAnchorEntries counts rows written before chaining was
	// introduced. Nothing sealed them and nothing can, after the fact.
	UnverifiablePreAnchorEntries int64           `json:"unverifiable_pre_anchor_entries"`
	Break                        *chainBreakBody `json:"break,omitempty"`
	CheckedAt                    time.Time       `json:"checked_at"`
	// Cached is true when this answer is a remembered walk rather than one run
	// for this request. checked_at says how old it is.
	Cached bool `json:"cached"`
}

// queryChainVerifyResponse covers both query-chain shapes: a sweep over every
// chained connection, and a single connection (which additionally reports that
// chain's head).
type queryChainVerifyResponse struct {
	Chain    string `json:"chain"`
	Verified bool   `json:"verified"`
	// ConnectionUID is set only when the caller scoped the walk to one session.
	ConnectionUID string `json:"connection_uid,omitempty"`
	// Connections is how many sessions were walked; 1 for a scoped walk.
	Connections int64 `json:"connections"`
	// Statements is how many chained statements were checked across them.
	Statements int64 `json:"statements"`
	// ChainsWithTruncatedPrefix counts chains missing their oldest statements.
	// That is what DBB_QUERY_STORAGE_RETENTION leaves behind on a long-lived
	// session, so it is reported rather than treated as tampering.
	ChainsWithTruncatedPrefix int64 `json:"chains_with_truncated_prefix"`
	// There is deliberately no unkeyed-stamp counter. A session carrying the
	// unkeyed head stamp is a break, everywhere and with no opt-out, so there is
	// nothing tolerated to count. This endpoint is served by the process under
	// audit and answers one question — does it verify.
	//
	// HeadSeq and HeadMAC are only meaningful for a single chain, so they are
	// reported for a scoped walk and omitted for a sweep.
	HeadSeq   *int64          `json:"head_seq,omitempty"`
	HeadMAC   string          `json:"head_mac,omitempty"`
	Break     *chainBreakBody `json:"break,omitempty"`
	CheckedAt time.Time       `json:"checked_at"`
	Cached    bool            `json:"cached"`
}

// rowChainVerifyResponse covers all three captured-row shapes: a sweep over
// every capture, one connection's captures, and a single capture (which
// additionally reports that chain's head).
//
// A capture's stamp has always been a keyed MAC over (query, length, head),
// never a verbatim copy of the head the way an early revision of the
// *connection* stamp was — so there is no unkeyed-stamp population here to
// report on at all.
type rowChainVerifyResponse struct {
	Chain    string `json:"chain"`
	Verified bool   `json:"verified"`
	// ConnectionUID is set only when the caller scoped the walk to one session.
	ConnectionUID string `json:"connection_uid,omitempty"`
	// QueryUID is set only when the caller scoped the walk to one capture.
	QueryUID string `json:"query_uid,omitempty"`
	// Captures is how many captures were walked; 1 for a query-scoped walk.
	Captures int64 `json:"captures"`
	// Rows is how many chained captured rows were checked across them.
	Rows int64 `json:"rows"`
	// UnverifiablePreMigrationRows counts captured rows written before the row
	// chain migration. Nothing sealed them and nothing can, after the fact.
	UnverifiablePreMigrationRows int64 `json:"unverifiable_pre_migration_rows"`
	// HeadRowNumber and HeadMAC are only meaningful for a single capture, so
	// they are reported for a query-scoped walk and omitted otherwise.
	HeadRowNumber *int64          `json:"head_row_number,omitempty"`
	HeadMAC       string          `json:"head_mac,omitempty"`
	Break         *chainBreakBody `json:"break,omitempty"`
	CheckedAt     time.Time       `json:"checked_at"`
	Cached        bool            `json:"cached"`
}

// handleVerifyAuditChain walks the store-wide audit chain. Admin only.
func (s *Server) handleVerifyAuditChain(c *gin.Context) {
	body, cached, err := s.chainVerify.result(c.Request.Context(), "audit",
		func(ctx context.Context) (any, error) {
			result, err := s.store.VerifyAuditChain(ctx)
			if err != nil {
				return nil, err
			}

			return auditChainVerifyResponse{
				Chain:                        "audit",
				Verified:                     result.OK(),
				Entries:                      result.Verified,
				HeadSeq:                      result.HeadSeq,
				HeadMAC:                      result.HeadMACHex(),
				UnverifiablePreAnchorEntries: result.Unchained,
				Break:                        newChainBreakBody(result.Break),
				CheckedAt:                    time.Now().UTC(),
			}, nil
		})
	if err != nil {
		s.writeChainVerifyError(c, err)

		return
	}

	resp, ok := body.(auditChainVerifyResponse)
	if !ok {
		writeInternalError(c, s.logger, errChainVerifyCacheType, "audit chain verification")

		return
	}

	resp.Cached = cached

	s.logChainVerification(c, "audit", resp.Verified, cached)
	successResponse(c, resp)
}

// handleVerifyQueryChains walks the per-connection query chains, either all of
// them or one session's when ?connection= is given. Admin only.
func (s *Server) handleVerifyQueryChains(c *gin.Context) {
	var connectionUID *uuid.UUID

	scope := "queries"

	if raw := c.Query("connection"); raw != "" {
		parsed, err := uuid.Parse(raw)
		if err != nil {
			writeError(c, http.StatusBadRequest, ErrCodeValidationError, "invalid connection UID")

			return
		}

		connectionUID = &parsed
		scope = "queries:" + parsed.String()
	}

	body, cached, err := s.chainVerify.result(c.Request.Context(), scope,
		func(ctx context.Context) (any, error) {
			return s.walkQueryChains(ctx, connectionUID)
		})
	if err != nil {
		s.writeChainVerifyError(c, err)

		return
	}

	resp, ok := body.(queryChainVerifyResponse)
	if !ok {
		writeInternalError(c, s.logger, errChainVerifyCacheType, "query chain verification")

		return
	}

	resp.Cached = cached

	s.logChainVerification(c, "queries", resp.Verified, cached)
	successResponse(c, resp)
}

// walkQueryChains runs the scoped or the sweeping walk. A scoped walk uses the
// single-chain verifier, which is the only one that can report a head — an
// aggregate head over many independent chains would not mean anything.
func (s *Server) walkQueryChains(ctx context.Context, connectionUID *uuid.UUID) (any, error) {
	if connectionUID != nil {
		one, err := s.store.VerifyQueryChain(ctx, *connectionUID)
		if err != nil {
			return nil, err
		}

		headSeq := one.HeadSeq
		truncated := int64(0)

		if one.TruncatedPrefix {
			truncated = 1
		}

		return queryChainVerifyResponse{
			Chain:                     "queries",
			Verified:                  one.Break == nil,
			ConnectionUID:             connectionUID.String(),
			Connections:               1,
			Statements:                one.Verified,
			ChainsWithTruncatedPrefix: truncated,
			HeadSeq:                   &headSeq,
			HeadMAC:                   hexMAC(one.HeadMAC),
			Break:                     newChainBreakBody(one.Break),
			CheckedAt:                 time.Now().UTC(),
		}, nil
	}

	result, err := s.store.VerifyQueryChains(ctx, nil)
	if err != nil {
		return nil, err
	}

	return queryChainVerifyResponse{
		Chain:                     "queries",
		Verified:                  result.OK(),
		Connections:               result.Connections,
		Statements:                result.Verified,
		ChainsWithTruncatedPrefix: result.Truncated,
		Break:                     newChainBreakBody(result.Break),
		CheckedAt:                 time.Now().UTC(),
	}, nil
}

// handleVerifyRowChains walks the per-capture result-row chains: every capture
// with one, one session's captures when ?connection= is given, or a single
// capture when ?query= is. Admin only.
//
// The two filters are mutually exclusive — ?query= already names exactly one
// capture, so a connection alongside it could only agree or contradict, and
// silently ignoring one of them is how a caller ends up trusting an answer to a
// question it did not ask.
func (s *Server) handleVerifyRowChains(c *gin.Context) {
	rawConnection, rawQuery := c.Query("connection"), c.Query("query")

	if rawConnection != "" && rawQuery != "" {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError,
			"connection and query cannot be combined: a query already names one capture")

		return
	}

	var (
		connectionUID *uuid.UUID
		queryUID      *uuid.UUID
	)

	scope := "rows"

	switch {
	case rawConnection != "":
		parsed, err := uuid.Parse(rawConnection)
		if err != nil {
			writeError(c, http.StatusBadRequest, ErrCodeValidationError, "invalid connection UID")

			return
		}

		connectionUID = &parsed
		scope = "rows:connection:" + parsed.String()
	case rawQuery != "":
		parsed, err := uuid.Parse(rawQuery)
		if err != nil {
			writeError(c, http.StatusBadRequest, ErrCodeValidationError, "invalid query UID")

			return
		}

		queryUID = &parsed
		scope = "rows:query:" + parsed.String()
	}

	body, cached, err := s.chainVerify.result(c.Request.Context(), scope,
		func(ctx context.Context) (any, error) {
			return s.walkRowChains(ctx, connectionUID, queryUID)
		})
	if err != nil {
		s.writeChainVerifyError(c, err)

		return
	}

	resp, ok := body.(rowChainVerifyResponse)
	if !ok {
		writeInternalError(c, s.logger, errChainVerifyCacheType, "captured row chain verification")

		return
	}

	resp.Cached = cached

	s.logChainVerification(c, "rows", resp.Verified, cached)
	successResponse(c, resp)
}

// walkRowChains runs the capture-scoped or the sweeping walk. A capture-scoped
// walk uses the single-chain verifier, which is the only one that can report a
// head — an aggregate head over many independent chains would not mean
// anything.
func (s *Server) walkRowChains(ctx context.Context, connectionUID, queryUID *uuid.UUID) (any, error) {
	if queryUID != nil {
		one, err := s.store.VerifyRowChain(ctx, *queryUID)
		if err != nil {
			return nil, err
		}

		// VerifyRowChain only sees rows carrying a MAC, so the pre-migration
		// count has to be asked for separately to keep this shape reporting the
		// same number the sweep does.
		unchained, err := s.store.CountUnchainedCapturedRows(ctx, *queryUID)
		if err != nil {
			return nil, err
		}

		headRowNumber := one.HeadRowNumber

		return rowChainVerifyResponse{
			Chain:                        "rows",
			Verified:                     one.Break == nil,
			QueryUID:                     queryUID.String(),
			Captures:                     1,
			Rows:                         one.Verified,
			UnverifiablePreMigrationRows: unchained,
			HeadRowNumber:                &headRowNumber,
			HeadMAC:                      hexMAC(one.HeadMAC),
			Break:                        newChainBreakBody(one.Break),
			CheckedAt:                    time.Now().UTC(),
		}, nil
	}

	result, err := s.store.VerifyRowChains(ctx, connectionUID)
	if err != nil {
		return nil, err
	}

	resp := rowChainVerifyResponse{
		Chain:                        "rows",
		Verified:                     result.OK(),
		Captures:                     result.Captures,
		Rows:                         result.Verified,
		UnverifiablePreMigrationRows: result.Unchained,
		Break:                        newChainBreakBody(result.Break),
		CheckedAt:                    time.Now().UTC(),
	}

	if connectionUID != nil {
		resp.ConnectionUID = connectionUID.String()
	}

	return resp, nil
}

// hexMAC renders a chain head for an operator to record. Publishing a MAC is
// safe and intended; publishing the key that produced it never is, and the key
// is not reachable from here.
func hexMAC(mac []byte) string {
	return hex.EncodeToString(mac)
}

// errChainVerifyCacheType guards an impossible state: the cache is keyed so
// that a scope always yields the body type its handler expects.
var errChainVerifyCacheType = errors.New("chain verification cache returned an unexpected body type")

// writeChainVerifyError maps a failed walk onto a status.
func (s *Server) writeChainVerifyError(c *gin.Context, err error) {
	// The caller went away while queued behind another walk. Nobody is left to
	// read a body.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		c.Abort()

		return
	}

	// A serving process always resolves an encryption key (config creates
	// ~/.dbbat/key if it has to), so this is a store built by hand without one.
	if errors.Is(err, store.ErrChainKeyUnavailable) {
		writeError(c, http.StatusConflict, ErrCodeConflict,
			"this instance has no audit chain key, so nothing it stores is chained")

		return
	}

	writeInternalError(c, s.logger, err, "chain verification failed")
}

// logChainVerification records that verification was run, and its outcome. A
// broken chain is logged at error level: it is the loudest thing this service
// can say.
func (s *Server) logChainVerification(c *gin.Context, chain string, verified, cached bool) {
	user := getCurrentUser(c)
	username := ""

	if user != nil {
		username = user.Username
	}

	if !verified {
		s.logger.ErrorContext(c.Request.Context(), "CHAIN VERIFICATION REPORTED A BREAK",
			"chain", chain, "requested_by", username, "cached", cached)

		return
	}

	s.logger.InfoContext(c.Request.Context(), "Chain verified over the API",
		"chain", chain, "requested_by", username, "cached", cached)
}
