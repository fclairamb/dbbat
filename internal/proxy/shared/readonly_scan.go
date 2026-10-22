package shared

import (
	"slices"
	"strings"
)

// The scans in this file close read_only holes the prefix-shaped classifier in
// validation.go cannot see by construction: a statement whose first keyword is
// not the one that writes. `WITH d AS (DELETE …) SELECT …` starts with WITH, and
// `BEGIN DELETE FROM t; END;` starts with BEGIN, so both used to read as reads.
//
// They look for a write keyword *anywhere* in the statement, outside literals
// and quoted identifiers, and they fail closed: a quoted run left open means
// the statement is reported as writing, because a scan that fell off the end
// vouches for nothing. The price is a false positive on a read that names a
// write keyword as a bare word, the realistic one being `SELECT … FOR UPDATE`
// inside a CTE or a block — which takes row locks and which PostgreSQL itself
// refuses in a read-only transaction, so refusing it is consistent.

// embeddedWriteKeywords are the statements that write data and can appear
// somewhere other than at the start of a statement: inside a CTE, after the CTE
// list, or inside a PL/SQL block. DDL is not here — neither dialect accepts it
// in either position (PL/SQL needs EXECUTE IMMEDIATE, which dynamicsql.go
// already unwraps) — and neither is REPLACE, which is also an ordinary string
// function on every dialect that has it.
var embeddedWriteKeywords = []string{"INSERT", "UPDATE", "DELETE", "MERGE"}

// isWritingCTE reports whether a statement led by WITH writes: either one of
// its CTEs is a data-modifying statement (PostgreSQL's writable CTEs, Oracle's
// `WITH FUNCTION` carrying an autonomous transaction), or the statement the CTE
// list introduces is one (`WITH c AS (…) DELETE FROM t`, valid on PostgreSQL,
// SQL Server and MySQL 8).
func isWritingCTE(sql string, syntax sqlCommentSyntax) bool {
	if !startsWithKeyword(sql, "WITH", syntax) {
		return false
	}

	return containsBareKeyword(sql, syntax, embeddedWriteKeywords)
}

// validateOracleReadOnlyPLSQL is the read_only control for the Oracle statements
// that run PL/SQL rather than SQL. It is only called under a read_only grant.
//
//   - an anonymous block (`BEGIN …` / `DECLARE …`) is refused when its body
//     contains a static INSERT/UPDATE/DELETE/MERGE. The dynamic form
//     (`EXECUTE IMMEDIATE '…'`) is unwrapped separately by ValidateOracleQuery.
//   - `CALL` is refused outright: what a stored procedure does lives in the
//     database, not in the statement, so no reading of the text can clear it.
//
// What stays out of reach, and is documented as such in docs/oracle.md: a block
// that calls a procedure (`BEGIN pkg.proc; END;`) and a function with an
// autonomous transaction called from a plain SELECT. Refusing every block that
// calls anything would refuse the `BEGIN DBMS_OUTPUT.ENABLE; END;` that SQL
// clients send on connect, i.e. every read_only session; Oracle offers no
// session-level read-only switch dbbat could pin instead. A read-only database
// account is the guarantee there.
func validateOracleReadOnlyPLSQL(matchable string) error {
	if startsWithKeyword(matchable, "CALL", syntaxStandard) {
		return ErrReadOnlyProcedureCall
	}

	if !isPLSQLBlock(matchable) {
		return nil
	}

	if containsBareKeyword(matchable, syntaxStandard, embeddedWriteKeywords) {
		return ErrReadOnlyViolation
	}

	return nil
}

// isPLSQLBlock reports whether an Oracle statement is an anonymous PL/SQL block:
// it opens with BEGIN or DECLARE, possibly behind a `<<label>>`.
func isPLSQLBlock(matchable string) bool {
	s := strings.TrimLeft(matchable, " \t\r\n\f")

	for strings.HasPrefix(s, "<<") {
		end := strings.Index(s, ">>")
		if end < 0 {
			return false
		}

		s = strings.TrimLeft(s[end+2:], " \t\r\n\f")
	}

	return startsWithKeyword(s, "BEGIN", syntaxStandard) || startsWithKeyword(s, "DECLARE", syntaxStandard)
}

// startsWithKeyword reports whether sql opens with kw as a whole word, after
// leading whitespace and case-insensitively. `WITHOUT` is not `WITH`.
func startsWithKeyword(sql, kw string, syntax sqlCommentSyntax) bool {
	p := &useScanner{s: sql, syntax: syntax}
	p.spaces()

	return p.keyword(kw)
}

// containsBareKeyword reports whether any of keywords occurs in sql as a whole
// word outside every literal and quoted identifier. A qualified name (`t.delete`)
// is a column, not the keyword, and does not count.
//
// It fails closed: a quoted run left open reports true.
func containsBareKeyword(sql string, syntax sqlCommentSyntax, keywords []string) bool {
	p := &useScanner{s: sql, syntax: syntax}

	for !p.eof() {
		skipped, ok := p.skipLiteral()
		if !ok {
			return true
		}

		if skipped {
			continue
		}

		if p.i > 0 {
			if prev := p.s[p.i-1]; isUseNameByte(prev, syntax) || prev == '.' {
				p.i++

				continue
			}
		}

		for _, kw := range keywords {
			if p.keyword(kw) {
				return true
			}
		}

		p.i++
	}

	return false
}

// PostgresSetConfigReaches reports whether a PostgreSQL statement calls
// set_config on one of names (lower case), or calls it in a way dbbat cannot
// read the name of.
//
// set_config(name, value, is_local) is SET as a function: everything the
// read-only bypass patterns refuse as `SET …` it does as a SELECT, so
// `SELECT set_config('default_transaction_read_only', 'off', false)` unpins a
// read-only session and `set_config('role', 'postgres', false)` is SET ROLE.
// Refusing the function outright would refuse pg_dump, which opens with
// `SELECT pg_catalog.set_config('search_path', <empty string>, false)`, so the
// name is read — and only a plain single-quoted literal is a name dbbat can
// read. Anything else in that position (a concatenation, an E-prefixed or
// dollar-quoted string, a column) reports true.
//
// set_config can also be reached from inside a string, through the functions
// that run one (`query_to_xml('select set_config(…)', …)`). The scan above skips
// literals, so a call it did not clear by reading its name is any occurrence of
// the word it did not account for: every `set_config` in the text, literal or
// not, has to be one the scan read and cleared.
func PostgresSetConfigReaches(sql string, names ...string) bool {
	matchable := matchableSQL(sql, syntaxStandard)

	total := strings.Count(strings.ToLower(matchable), "set_config")
	if total == 0 {
		return false
	}

	p := &useScanner{s: matchable, syntax: syntaxStandard}
	cleared := 0

	for !p.eof() {
		if p.setConfigNameHere() {
			name, ok := p.setConfigName()
			if !ok || slices.Contains(names, strings.ToLower(name)) {
				return true
			}

			cleared++

			continue
		}

		skipped, ok := p.skipLiteral()
		if !ok {
			return true
		}

		if !skipped {
			p.i++
		}
	}

	return cleared < total
}

// setConfigNameHere consumes `set_config` (bare or double-quoted, optionally
// schema-qualified — the qualifier has already been walked past) and the opening
// parenthesis of its call. It does not consume anything when the call is not
// there.
func (p *useScanner) setConfigNameHere() bool {
	if p.i > 0 && isUseNameByte(p.s[p.i-1], p.syntax) {
		return false
	}

	saved := p.i

	quoted := p.s[p.i] == '"'
	if quoted {
		p.i++
	}

	if !p.keyword("set_config") {
		p.i = saved

		return false
	}

	if quoted {
		if p.eof() || p.s[p.i] != '"' {
			p.i = saved

			return false
		}

		p.i++
	}

	p.spaces()

	if p.eof() || p.s[p.i] != '(' {
		p.i = saved

		return false
	}

	p.i++

	return true
}

// setConfigName reads set_config's first argument, which it accepts only as a
// plain single-quoted literal followed by the comma before the value.
func (p *useScanner) setConfigName() (string, bool) {
	p.spaces()

	if p.eof() || p.s[p.i] != '\'' {
		return "", false
	}

	end, ok := scanQuotedRun(p.s, p.i, p.syntax)
	if !ok {
		return "", false
	}

	name := strings.ReplaceAll(p.s[p.i+1:end-1], "''", "'")
	p.i = end

	p.spaces()

	if p.eof() || p.s[p.i] != ',' {
		return "", false
	}

	return name, true
}
