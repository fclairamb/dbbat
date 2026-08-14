package shared

import "strings"

// SQL comments are not a formatting detail for the validators: every check in
// validation.go is regex- or prefix-shaped, and the database ignores a comment
// wherever whitespace is allowed. `ALTER/**/SESSION SET CONTAINER=PDB2` is the
// same statement to Oracle as `ALTER SESSION SET CONTAINER=PDB2`, so an
// outright block that only matches the second is not an outright block.
//
// The fix is a **scratch copy**: matchableSQL returns a normalised string that
// only ever reaches the matchers. The statement relayed upstream stays
// byte-identical — optimizer hints (`/*+ INDEX(t i) */`) are comments too, and
// rewriting them would silently change query plans on real workloads. Nothing
// here returns anything a proxy sends to a database.

// sqlCommentSyntax selects which comment and literal forms the normaliser
// recognises.
type sqlCommentSyntax int

const (
	// syntaxStandard covers Oracle, PostgreSQL and SQL Server: `/* … */` and
	// `-- …` comments, `'…'` literals with `''` escaping, `"…"` quoted
	// identifiers, and Oracle's `q'[…]'` quote operator.
	syntaxStandard sqlCommentSyntax = iota
	// syntaxMySQL is the MySQL/MariaDB dialect: `# …` is also a comment, `--`
	// only introduces one when followed by whitespace, `\` escapes inside a
	// quoted run, backticks quote identifiers, and `/*! … */` is an *executable*
	// comment whose body must stay visible to the matchers.
	syntaxMySQL
)

// commentIntroducers are the only bytes that can start a comment in either
// dialect. The overwhelmingly common statement contains none of them, so a
// single ContainsAny pass buys the whole normalisation away — this code runs on
// every statement on all five protocols.
const commentIntroducers = "/-#"

// matchableSQL returns the scratch copy the validators match against: every
// comment replaced by a single space, every literal preserved verbatim.
//
// A single space rather than the empty string, because `ALTER/**/SESSION` must
// normalise to `ALTER SESSION` (which matches) and not to `ALTERSESSION` (which
// would escape again).
//
// It fails **closed**: when the scanner cannot make sense of the input — an
// unterminated literal, say — it hands back the raw statement so the matchers
// run against something rather than against a statement nothing checked.
func matchableSQL(sql string, syntax sqlCommentSyntax) string {
	if !strings.ContainsAny(sql, commentIntroducers) {
		return sql
	}

	stripped, ok := stripSQLComments(sql, syntax)
	if !ok {
		return sql
	}

	return stripped
}

// stripSQLComments does the work behind matchableSQL. It reports false when a
// quoted run is left open, which is the caller's cue to fall back to the raw
// text.
//
// Mis-parses are one-directional by construction. Treating real SQL as if it
// were inside a literal only means a comment goes un-stripped — the behaviour
// before this file existed. Bytes are ever only *copied* verbatim or replaced
// by a space, never re-ordered or fused, so no normalisation can manufacture a
// keyword that the client did not write.
func stripSQLComments(sql string, syntax sqlCommentSyntax) (string, bool) {
	var b strings.Builder

	b.Grow(len(sql))

	changed := false

	for i := 0; i < len(sql); {
		c := sql[i]

		switch {
		case c == '\'' || c == '"' || (c == '`' && syntax == syntaxMySQL):
			end, ok := scanQuotedRun(sql, i, syntax)
			if !ok {
				return "", false
			}

			b.WriteString(sql[i:end])
			i = end

		case isQuoteOperator(sql, i, syntax):
			end, ok := scanQuoteOperator(sql, i)
			if !ok {
				return "", false
			}

			b.WriteString(sql[i:end])
			i = end

		case isExecutableComment(sql, i, syntax):
			// MySQL *runs* the body of `/*! … */` (and MariaDB's `/*M! … */`), so
			// hiding it from the matchers would be the very evasion this file
			// closes. Drop the introducer, keep scanning the body as code; the
			// stray `*/` that ends it is inert noise in a scratch copy.
			b.WriteByte(' ')

			i += executableCommentPrefixLen(sql, i)
			changed = true

		case c == '/' && i+1 < len(sql) && sql[i+1] == '*':
			i = skipBlockComment(sql, i)

			b.WriteByte(' ')
			changed = true

		case isLineComment(sql, i, syntax):
			i = skipLineComment(sql, i)

			b.WriteByte(' ')
			changed = true

		default:
			b.WriteByte(c)
			i++
		}
	}

	if !changed {
		return sql, true
	}

	return b.String(), true
}

// scanQuotedRun consumes the quoted run opening at start and returns the index
// just past its closing delimiter. Doubling (`''`, `""`, ` `` `) escapes the
// delimiter in every dialect; MySQL additionally honours backslash escapes in
// string literals (but not in backtick identifiers).
//
// An unterminated run reports false — that is the fail-closed path.
func scanQuotedRun(sql string, start int, syntax sqlCommentSyntax) (int, bool) {
	q := sql[start]
	backslashEscapes := syntax == syntaxMySQL && q != '`'

	for i := start + 1; i < len(sql); {
		switch {
		case backslashEscapes && sql[i] == '\\':
			i += 2
		case sql[i] != q:
			i++
		case i+1 < len(sql) && sql[i+1] == q:
			i += 2
		default:
			return i + 1, true
		}
	}

	return 0, false
}

// isQuoteOperator reports whether Oracle's `q'…'` quote operator opens at i.
//
// The word-boundary check keeps a column whose name merely ends in `q` from
// being read as the operator. `nq'…'` (national character set) is the one
// prefix Oracle allows in front, so it is admitted explicitly.
func isQuoteOperator(sql string, i int, syntax sqlCommentSyntax) bool {
	if syntax != syntaxStandard {
		return false
	}

	if sql[i] != 'q' && sql[i] != 'Q' {
		return false
	}

	if i+1 >= len(sql) || sql[i+1] != '\'' {
		return false
	}

	if i == 0 || !isBareNameByte(sql[i-1]) {
		return true
	}

	if sql[i-1] != 'n' && sql[i-1] != 'N' {
		return false
	}

	return i-1 == 0 || !isBareNameByte(sql[i-2])
}

// scanQuoteOperator consumes `q'<delim> … <closer>'` and returns the index just
// past the trailing quote. The four bracketing delimiters mirror; every other
// delimiter closes with itself.
func scanQuoteOperator(sql string, start int) (int, bool) {
	if start+2 >= len(sql) {
		return 0, false
	}

	closer := sql[start+2]

	switch closer {
	case '[':
		closer = ']'
	case '{':
		closer = '}'
	case '(':
		closer = ')'
	case '<':
		closer = '>'
	}

	for i := start + 3; i+1 < len(sql); i++ {
		if sql[i] == closer && sql[i+1] == '\'' {
			return i + 2, true
		}
	}

	return 0, false
}

// isExecutableComment reports whether a MySQL/MariaDB version-gated comment
// (`/*!…*/`, `/*M!…*/`) opens at i — the one comment form whose body is SQL the
// server actually executes.
func isExecutableComment(sql string, i int, syntax sqlCommentSyntax) bool {
	return syntax == syntaxMySQL && executableCommentPrefixLen(sql, i) > 0
}

// executableCommentPrefixLen returns the length of the executable-comment
// introducer at i, or 0 when there is none.
func executableCommentPrefixLen(sql string, i int) int {
	if i+2 >= len(sql) || sql[i] != '/' || sql[i+1] != '*' {
		return 0
	}

	if sql[i+2] == '!' {
		return 3
	}

	if i+3 < len(sql) && (sql[i+2] == 'M' || sql[i+2] == 'm') && sql[i+3] == '!' {
		return 4
	}

	return 0
}

// skipBlockComment returns the index just past the `*/` closing the block
// comment at i, or len(sql) when it is unterminated — an unterminated `/*` runs
// to the end of the input, which is what the servers do too.
//
// Block comments are treated as non-nesting. PostgreSQL does nest them, so a
// nested comment leaves its tail visible to the matchers; that direction is a
// false positive (a block PostgreSQL would have ignored), never a bypass.
func skipBlockComment(sql string, i int) int {
	if end := strings.Index(sql[i+2:], "*/"); end >= 0 {
		return i + 2 + end + 2
	}

	return len(sql)
}

// isLineComment reports whether a to-end-of-line comment opens at i. MySQL
// requires whitespace (or the end of the statement) after `--` and additionally
// accepts `#`.
func isLineComment(sql string, i int, syntax sqlCommentSyntax) bool {
	if sql[i] == '#' {
		return syntax == syntaxMySQL
	}

	if sql[i] != '-' || i+1 >= len(sql) || sql[i+1] != '-' {
		return false
	}

	if syntax != syntaxMySQL {
		return true
	}

	return i+2 >= len(sql) || isSQLSpace(sql[i+2])
}

// skipLineComment returns the index of the newline ending the comment at i (the
// newline itself is not part of the comment and is kept), or len(sql).
func skipLineComment(sql string, i int) int {
	if end := strings.IndexByte(sql[i:], '\n'); end >= 0 {
		return i + end
	}

	return len(sql)
}
