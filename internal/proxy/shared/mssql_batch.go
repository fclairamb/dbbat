package shared

import "github.com/fclairamb/dbbat/internal/store"

// mssqlBatchWriteKeywords are the writeKeywords that are reserved words in
// T-SQL. REPLACE is left out: it is MySQL's statement but SQL Server's string
// function, so `SELECT REPLACE(name, 'a', 'b')` must stay a read.
var mssqlBatchWriteKeywords = []string{
	"INSERT", "UPDATE", "DELETE", "DROP", "TRUNCATE",
	"CREATE", "ALTER", "GRANT", "REVOKE", "MERGE",
}

// ValidateMSSQLBatch applies read_only and block_ddl to every statement of a
// T-SQL batch, not only to its first one.
//
// ValidateQuery reads the leading keyword, but a batch is genuinely
// multi-statement and needs no separator: `SELECT 1` ⏎ `DELETE FROM t` is one
// ordinary SQLBatch, and SQL Server has no session read-only mode dbbat could
// pin behind the text. So the whole batch is scanned for the keywords, the way
// MSSQLUseTargets scans it for USE. That is only sound because every keyword
// here is reserved in T-SQL and cannot be a bare identifier; literals, quoted
// and bracketed identifiers, variables (`@delete`), temp objects (`#update`)
// and qualified names (`t.delete`) are stepped over. A quoted run left open
// fails closed.
func ValidateMSSQLBatch(sql string, grant *store.Grant) error {
	if !grant.IsReadOnly() && !grant.ShouldBlockDDL() {
		return nil
	}

	matchable := matchableSQL(sql, syntaxStandard)

	if grant.IsReadOnly() && mssqlBatchHasKeyword(matchable, mssqlBatchWriteKeywords) {
		return ErrReadOnlyViolation
	}

	if grant.ShouldBlockDDL() && mssqlBatchHasKeyword(matchable, ddlKeywords) {
		return ErrDDLBlocked
	}

	return nil
}

// mssqlBatchHasKeyword reports whether any of keywords occurs in a
// comment-normalized T-SQL batch as a bare word. It fails closed: a literal or
// bracketed identifier left open reports true.
func mssqlBatchHasKeyword(s string, keywords []string) bool {
	p := &useScanner{s: s, syntax: syntaxStandard}

	for !p.eof() {
		if end, isRun, ok := scanVerbatimRun(p.s, p.i, p.syntax); isRun {
			if !ok {
				return true
			}

			p.i = end

			continue
		}

		if p.s[p.i] == '[' {
			end, ok := p.bracketEnd()
			if !ok {
				return true
			}

			p.i = end

			continue
		}

		if p.i > 0 {
			if prev := p.s[p.i-1]; isUseNameByte(prev, p.syntax) || prev == '.' {
				p.i++

				continue
			}
		}

		for _, kw := range keywords {
			if !p.keyword(kw) {
				continue
			}

			if kw == "MERGE" && p.mergeIsJoinHint() {
				break
			}

			return true
		}

		p.i++
	}

	return false
}

// mergeIsJoinHint reports whether the MERGE just consumed is a join or union
// hint (`INNER MERGE JOIN`, `OPTION (MERGE JOIN)`, `OPTION (MERGE UNION)`)
// rather than the MERGE statement. It leaves the scanner after MERGE.
func (p *useScanner) mergeIsJoinHint() bool {
	saved := p.i
	defer func() { p.i = saved }()

	p.spaces()

	return p.keyword("JOIN") || p.keyword("UNION")
}
