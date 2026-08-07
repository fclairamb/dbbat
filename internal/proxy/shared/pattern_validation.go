package shared

// QueryMatchResult is the outcome of testing one candidate SQL statement
// against a set of compiled approval patterns: the NormalizeSQL form the
// patterns actually run against, and which pattern (if any) matched first.
type QueryMatchResult struct {
	Query      string
	Normalized string
	Matched    bool
	Pattern    string
}

// ValidatePatternsAgainstQueries compiles the given approval-pattern sources
// and matches each query against them, applying exactly
// CompiledApprovalPatterns.Match's semantics — the same code
// NewApprovalGate/ApprovalGate.Match use on the proxy hot path.
//
// It is a pure function: no store, no network, nothing but regexp compile
// and string matching. That is what makes it safe to expose directly as the
// grant-definition pattern-authoring endpoint (POST
// /grant-definitions/validate-patterns) — an author previews what a set of
// patterns will do against a bench of sample queries before saving anything.
//
// Compile errors are returned alongside the match results, not as a hard
// failure: a pattern that fails to compile still lets the other patterns in
// the batch be tested, and it is up to the caller to decide compile errors
// block a save while non-matches don't (see docs on the API handler).
func ValidatePatternsAgainstQueries(patterns, queries []string) (*CompiledApprovalPatterns, []QueryMatchResult) {
	compiled := CompileApprovalPatterns(patterns)

	results := make([]QueryMatchResult, len(queries))

	for i, q := range queries {
		pattern, matched := compiled.Match(q)
		results[i] = QueryMatchResult{
			Query:      q,
			Normalized: NormalizeSQL(q),
			Matched:    matched,
			Pattern:    pattern,
		}
	}

	return compiled, results
}
