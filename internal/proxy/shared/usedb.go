package shared

import (
	"strings"
)

// A `USE <db>` is not a grant-control question, it is a scope question: a dbbat
// grant is issued on one server row, and the audit trail, the quotas and the
// approval holds are all written against that row. `queries` carries no database
// column of its own — only `connection_id`, and `connections.database_id` is
// pinned at auth — so a session that moves elsewhere is not merely permitted to
// read another database, every statement it then runs is *attributed* to the
// granted one. A full-write grant on one database is still not a grant on
// another, which is why the refusal below is independent of read_only,
// block_ddl and block_copy.
//
// Both helpers here follow sqlcomments.go's contract: they match against the
// comment-normalized scratch copy and hand back only an extracted identifier
// and a boolean. The normalized string never escapes, so nothing a proxy relays
// upstream can come from this file.

// MySQLUseTarget reports whether sql is a MySQL `USE <database>` statement and,
// when it is, the database it names.
//
// The two return values are read together: `isUse` true with an empty target
// means "this statement switches database and dbbat could not read where to",
// which the caller must refuse rather than forward — an unreadable switch is
// still a switch.
//
// The match is anchored at the start of the statement, which is sound on this
// protocol rather than merely convenient: `USE` can only ever begin a statement,
// and the client leg does not negotiate `CLIENT_MULTI_STATEMENTS` (go-mysql's
// server capabilities offer `CLIENT_MULTI_RESULTS` only), so one COM_QUERY
// carries one statement. Anything trailing the target other than a `;` is
// therefore refused rather than parsed: `USE otherdb; SELECT 1` is not a shape
// dbbat can vouch for.
func MySQLUseTarget(sql string) (string, bool) {
	p := &useScanner{s: matchableSQL(sql, syntaxMySQL), syntax: syntaxMySQL}

	p.spaces()

	if !p.keyword("USE") {
		return "", false
	}

	p.spaces()

	name, ok := p.name()
	if !ok {
		return "", true
	}

	p.spaces()

	if !p.eof() && p.s[p.i] == ';' {
		p.i++
		p.spaces()
	}

	if !p.eof() {
		return "", true
	}

	return name, true
}

// MSSQLUseTargets returns every database a T-SQL batch would switch to, and
// whether the batch could be read with confidence. `false` means a `USE` was
// found whose target dbbat could not parse (or that a quoted run was left open)
// — fail closed, the caller refuses.
//
// It scans the whole batch rather than only its first statement, unlike
// MySQLUseTarget above, because a TDS SQLBatch is genuinely multi-statement and
// T-SQL needs no separator at all: `SELECT 1` followed by a newline and
// `USE otherdb` is one ordinary batch. An anchored check here would be a fig
// leaf.
//
// Scanning for a keyword mid-statement is only safe because the scan skips
// string literals and quoted identifiers outright — `INSERT INTO t VALUES ('USE
// otherdb')` names no database — and because `USE` is a reserved word in T-SQL,
// so it cannot appear as a bare column or alias. The two constructs that do
// spell it without switching anything are the query hints `OPTION (USE PLAN …)`
// and `OPTION (USE HINT (…))`, which are skipped by name.
func MSSQLUseTargets(sql string) ([]string, bool) {
	p := &useScanner{s: matchableSQL(sql, syntaxStandard), syntax: syntaxStandard}

	var targets []string

	for !p.eof() {
		// A literal or a quoted identifier is stepped over whole: no statement
		// begins inside one. The quoted form of a switch's *own* target
		// (`USE "db"`, `USE [db]`) is read below by name(), never here.
		if end, isRun, ok := scanVerbatimRun(p.s, p.i, p.syntax); isRun {
			if !ok {
				return nil, false
			}

			p.i = end

			continue
		}

		if p.s[p.i] == '[' {
			end, ok := p.bracketEnd()
			if !ok {
				return nil, false
			}

			p.i = end

			continue
		}

		if !p.useKeywordHere() {
			p.i++

			continue
		}

		p.i += len("USE")
		p.spaces()

		name, ok := p.name()
		if !ok {
			return nil, false
		}

		// Query hints, not database switches. Both live inside an OPTION clause
		// and neither can name a database.
		if strings.EqualFold(name, "PLAN") || strings.EqualFold(name, "HINT") {
			continue
		}

		targets = append(targets, name)
	}

	return targets, true
}

// useScanner is a byte scanner over a comment-normalized statement, used for
// nothing but answering "does this switch database, and to what".
type useScanner struct {
	s      string
	i      int
	syntax sqlCommentSyntax
}

func (p *useScanner) eof() bool { return p.i >= len(p.s) }

func (p *useScanner) spaces() {
	for p.i < len(p.s) && isSQLSpace(p.s[p.i]) {
		p.i++
	}
}

// keyword consumes kw at the scanner's position, case-insensitively and only
// when what follows is not more identifier — `USEFUL` must not read as `USE`.
func (p *useScanner) keyword(kw string) bool {
	end := p.i + len(kw)
	if end > len(p.s) || !strings.EqualFold(p.s[p.i:end], kw) {
		return false
	}

	if end < len(p.s) && isUseNameByte(p.s[end], p.syntax) {
		return false
	}

	p.i = end

	return true
}

// useKeywordHere reports whether `USE` starts at the scanner's position as a
// whole word. Unlike keyword() it also checks the byte *behind* it, because this
// one is used mid-statement where `t.use` and `@use` are what must not match.
func (p *useScanner) useKeywordHere() bool {
	if p.i > 0 {
		if prev := p.s[p.i-1]; isUseNameByte(prev, p.syntax) || prev == '.' {
			return false
		}
	}

	end := p.i + len("USE")
	if end > len(p.s) || !strings.EqualFold(p.s[p.i:end], "USE") {
		return false
	}

	return end >= len(p.s) || !isUseNameByte(p.s[end], p.syntax)
}

// name reads the identifier at the scanner's position: bare, or delimited by
// backticks (MySQL), double quotes, or brackets (SQL Server). Doubling the
// closing delimiter escapes it in every one of those forms.
//
// It reports false when there is no identifier to read or a delimited one is
// left open — the fail-closed path, since the caller cannot compare a name it
// never got.
func (p *useScanner) name() (string, bool) {
	if p.eof() {
		return "", false
	}

	switch c := p.s[p.i]; {
	case c == '`' && p.syntax == syntaxMySQL:
		return p.delimited('`')
	case c == '"':
		return p.delimited('"')
	case c == '[' && p.syntax == syntaxStandard:
		return p.delimited(']')
	}

	start := p.i
	for p.i < len(p.s) && isUseNameByte(p.s[p.i], p.syntax) {
		p.i++
	}

	if p.i == start {
		return "", false
	}

	return p.s[start:p.i], true
}

// delimited reads a quoted identifier whose opening delimiter is at the
// scanner's position and whose closing one is closer.
func (p *useScanner) delimited(closer byte) (string, bool) {
	p.i++

	var b strings.Builder

	for p.i < len(p.s) {
		if p.s[p.i] != closer {
			b.WriteByte(p.s[p.i])
			p.i++

			continue
		}

		if p.i+1 < len(p.s) && p.s[p.i+1] == closer {
			b.WriteByte(closer)
			p.i += 2

			continue
		}

		p.i++

		return b.String(), true
	}

	return "", false
}

// bracketEnd returns the index just past the `]` closing the bracketed
// identifier at the scanner's position, honoring `]]` as an escaped bracket. It
// exists so the mid-statement scan steps over `[a column named USE otherdb]`
// instead of reading a switch out of it.
func (p *useScanner) bracketEnd() (int, bool) {
	saved := p.i

	_, ok := p.delimited(']')
	end := p.i
	p.i = saved

	return end, ok
}

// isUseNameByte covers the unquoted identifier charset of both dialects.
func isUseNameByte(c byte, syntax sqlCommentSyntax) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	case c == '_', c == '$':
		return true
	case c >= 0x80:
		// Both servers admit non-ASCII in an unquoted identifier. Byte-wise is
		// enough: every byte of a multi-byte UTF-8 rune is >= 0x80.
		return true
	case c == '#', c == '@':
		// T-SQL's temp-object and variable sigils. MySQL spells a comment with
		// `#`, which the normalizer has already removed by the time this runs,
		// so admitting it there would only ever be wrong.
		return syntax == syntaxStandard
	default:
		return false
	}
}
