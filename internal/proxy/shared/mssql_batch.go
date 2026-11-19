package shared

import "github.com/fclairamb/dbbat/internal/store"

// mssqlBatchWriteKeywords are the writeKeywords that are reserved words in
// T-SQL. REPLACE is left out: it is MySQL's statement but SQL Server's string
// function, so `SELECT REPLACE(name, 'a', 'b')` must stay a read.
var mssqlBatchWriteKeywords = []string{
	"INSERT", "UPDATE", "DELETE", "DROP", "TRUNCATE",
	"CREATE", "ALTER", "GRANT", "REVOKE", "MERGE",
}

// mssqlBatchDDLKeywords are the ddlKeywords that are reserved words in T-SQL.
// RENAME, PURGE, FLASHBACK and COMMENT are left out: they are not T-SQL
// statements and not reserved, so `SELECT comment, rename FROM t` must stay a
// read.
var mssqlBatchDDLKeywords = []string{"CREATE", "ALTER", "DROP", "TRUNCATE"}

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
//
// Ahead of the controls, and whatever the grant, it refuses a password change
// anywhere in the batch (see mssqlBatchChangesPassword).
func ValidateMSSQLBatch(sql string, grant *store.Grant) error {
	matchable := matchableSQL(sql, syntaxStandard)

	// Every dbbat session to a server shares one upstream login: changing its
	// password locks every other user out.
	if mssqlBatchChangesPassword(matchable) {
		return ErrPasswordChangeBlocked
	}

	if !grant.IsReadOnly() && !grant.ShouldBlockDDL() {
		return nil
	}

	// SELECT … INTO <table> creates a table and fills it, with no keyword above
	// in it. See selectsIntoTable.
	selectInto := selectsIntoTable(matchable, true)

	if grant.IsReadOnly() && (selectInto || mssqlBatchHasKeyword(matchable, mssqlBatchWriteKeywords)) {
		return ErrReadOnlyViolation
	}

	if grant.ShouldBlockDDL() && (selectInto || mssqlBatchHasKeyword(matchable, mssqlBatchDDLKeywords)) {
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

// mssqlBatchChangesPassword reports whether a comment-normalized T-SQL batch
// holds a statement that sets a password: `ALTER LOGIN … WITH PASSWORD`,
// `CREATE LOGIN … WITH PASSWORD`, and the contained-database spellings
// `ALTER USER … WITH PASSWORD` / `CREATE USER … WITH PASSWORD`.
//
// It walks the batch the way mssqlBatchHasKeyword does: a bare ALTER or CREATE
// followed by LOGIN or USER arms it, a `;` or the next bare ALTER/CREATE
// re-decides, and a bare PASSWORD while armed is the finding. OLD_PASSWORD,
// `[password]`, `@password` and `'PASSWORD'` are stepped over. T-SQL needs no
// separator, so a later statement of the batch naming a bare `password` column
// behind an armed ALTER LOGIN is refused too: that fails closed. A literal or
// bracketed identifier left open while armed reports true.
func mssqlBatchChangesPassword(s string) bool {
	p := &useScanner{s: s, syntax: syntaxStandard}
	armed := false

	for !p.eof() {
		if end, isRun, ok := scanVerbatimRun(p.s, p.i, p.syntax); isRun {
			if !ok {
				return armed
			}

			p.i = end

			continue
		}

		if p.s[p.i] == '[' {
			end, ok := p.bracketEnd()
			if !ok {
				return armed
			}

			p.i = end

			continue
		}

		if p.s[p.i] == ';' {
			armed = false
			p.i++

			continue
		}

		if p.i > 0 {
			if prev := p.s[p.i-1]; isUseNameByte(prev, p.syntax) || prev == '.' {
				p.i++

				continue
			}
		}

		if p.keyword("ALTER") || p.keyword("CREATE") {
			p.spaces()
			armed = p.keyword("LOGIN") || p.keyword("USER")

			continue
		}

		if armed && p.keyword("PASSWORD") {
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
