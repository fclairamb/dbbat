package api

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/fclairamb/dbbat/internal/proxy/shared"
	"github.com/fclairamb/dbbat/internal/store"
)

// ValidatePatternsRequest is the JSON body for
// POST /grant-definitions/validate-patterns.
type ValidatePatternsRequest struct {
	// Patterns are the RE2 approval-pattern sources to test — typically a
	// grant definition's approval_patterns field, saved or still being
	// edited in the dialog.
	Patterns []string `json:"patterns"`
	// Queries are sample SQL statements to test the patterns against —
	// typically a definition's sample_queries field.
	Queries []string `json:"queries"`
}

// PatternValidationResult reports whether one pattern (by its position in
// the request's patterns array) compiled.
type PatternValidationResult struct {
	Pattern string `json:"pattern"`
	// Error is the compile error message, present only when the pattern
	// failed to compile. This is exactly the error NewApprovalGate silently
	// swallows (with only a server-side warning log) when building the live
	// per-session gate — see approval.go — surfaced here instead of staying
	// invisible until a live statement fails to hold.
	Error string `json:"error,omitempty"`
}

// QueryValidationResult reports how one sample query fares against the
// patterns: the NormalizeSQL form the patterns actually run against, and
// which pattern matched it first, if any — same semantics as
// ApprovalGate.Match on the proxy hot path.
type QueryValidationResult struct {
	Query      string `json:"query"`
	Normalized string `json:"normalized"`
	Matched    bool   `json:"matched"`
	// MatchedPattern is the source text of the first pattern that matched,
	// present only when Matched is true.
	MatchedPattern string `json:"matched_pattern,omitempty"`
}

// handleValidateGrantDefinitionPatterns is a pure function over
// internal/proxy/shared: no store access, nothing persisted, no grant
// definition needs to exist. It lets an admin authoring approval_patterns
// see, before saving anything:
//
//   - which patterns fail to compile — the exact error NewApprovalGate
//     otherwise swallows with only a warning log;
//   - which of a bench of sample queries each pattern would hold, using the
//     same NormalizeSQL + first-match-wins semantics ApprovalGate.Match
//     applies at runtime, so the preview can never lie about proxy
//     behavior.
//
// A pattern that fails to compile is deliberately the *only* thing this
// endpoint treats as noteworthy for the caller to act on before saving — see
// validateDefinitionRequest, which blocks a save on a compile error via
// store.ValidateApprovalPatterns. A sample that simply does not match a
// pattern is not an error at all: this endpoint reports it exactly the same
// way as a match, leaving the "should this block the save" decision to the
// caller (it must not — see the resolved open questions in
// specs/todos/2026-08-06-06-sql-control-pattern-query-templates.md).
func (s *Server) handleValidateGrantDefinitionPatterns(c *gin.Context) {
	var req ValidatePatternsRequest

	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, "invalid request: "+err.Error())

		return
	}

	if len(req.Patterns) > store.MaxApprovalPatterns {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, "too many patterns")

		return
	}

	if len(req.Queries) > store.MaxSampleQueries {
		writeError(c, http.StatusBadRequest, ErrCodeValidationError, "too many queries")

		return
	}

	for _, p := range req.Patterns {
		if len(p) > store.MaxApprovalPatternLength {
			writeError(c, http.StatusBadRequest, ErrCodeValidationError, "pattern too long")

			return
		}
	}

	for _, q := range req.Queries {
		if len(q) > store.MaxSampleQueryLength {
			writeError(c, http.StatusBadRequest, ErrCodeValidationError, "query too long")

			return
		}
	}

	compiled, matches := shared.ValidatePatternsAgainstQueries(req.Patterns, req.Queries)

	patternResults := make([]PatternValidationResult, len(req.Patterns))

	for i, e := range compiled.Entries() {
		patternResults[i] = PatternValidationResult{Pattern: e.Pattern}
		if e.Err != nil {
			patternResults[i].Error = e.Err.Error()
		}
	}

	queryResults := make([]QueryValidationResult, len(matches))

	for i, m := range matches {
		// m.Pattern is already "" when m.Matched is false — Match returns
		// "", false on a miss — so this is a direct field copy, not a
		// conditional.
		queryResults[i] = QueryValidationResult{
			Query:          m.Query,
			Normalized:     m.Normalized,
			Matched:        m.Matched,
			MatchedPattern: m.Pattern,
		}
	}

	successResponse(c, gin.H{
		"patterns": patternResults,
		"queries":  queryResults,
	})
}
