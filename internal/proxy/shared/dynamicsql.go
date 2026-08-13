package shared

// T-SQL's `EXEC(<literal>)` runs the literal's contents as a batch of its own.
// That makes "no statement begins inside a string literal" — the invariant the
// USE scan rests on, and the one every prefix-shaped validator implicitly
// assumes — false on this one construct: `EXEC('DELETE FROM t')` is a DELETE
// that no amount of looking at the outer statement's first keyword will ever
// see, so `read_only`, `block_ddl`, `block_copy` and the database-switch
// refusal all walked past it.
//
// The fix is to hand the checks the inner statement too, and it only works when
// the statement text is *decidable*: a literal, and nothing but a literal.
// `EXEC(@sql)` is built at runtime and dbbat has no way to know what it says —
// that is a documented limitation (docs/mssql.md), not something this file
// pretends to cover.
//
// Same contract as sqlcomments.go and usedb.go: the scan runs over the
// comment-normalized scratch copy, and what comes back is the inner statement
// text as the *client wrote it*, for matching only. The bytes relayed upstream
// are never any of this.

// MSSQLDynamicSQL returns the statement text of every dynamic-SQL form in a
// T-SQL batch whose text dbbat can read — `EXEC('…')`, `EXECUTE('…')` and
// `sp_executesql N'…'` — with the doubled quotes that escape a quote inside a
// literal undone, so the caller can run the same checks over it that the outer
// batch gets.
//
// `false` means the batch carries dynamic SQL dbbat cannot vouch for and the
// caller must refuse it: either a quoted run was left open, or the extracted
// text nests *another* dynamic-SQL form. The nesting case is refused rather
// than unwrapped a second time — one level is where the recursion stops, and
// stopping silently would be a hole the shape of the one this file closes.
//
// A form whose argument is *not* a literal — `EXEC(@sql)`, `EXEC('a' + @b)`,
// `EXEC dbo.some_proc` — yields no text and is deliberately **not** refused.
// The first two are undecidable; the third is an ordinary procedure call, which
// `describeRPCRequest` already fails closed on under a restrictive grant.
func MSSQLDynamicSQL(sql string) ([]string, bool) {
	texts, ok := mssqlDynamicSQL(matchableSQL(sql, syntaxStandard))
	if !ok {
		return nil, false
	}

	for _, text := range texts {
		if nestsMSSQLDynamicSQL(matchableSQL(text, syntaxStandard)) {
			return nil, false
		}
	}

	return texts, true
}

// mssqlDynamicSQL does one level of extraction over an already-normalized batch.
func mssqlDynamicSQL(sql string) ([]string, bool) {
	p := &useScanner{s: sql, syntax: syntaxStandard}

	var texts []string

	for !p.eof() {
		skipped, ok := p.skipOpaque()
		if !ok {
			return nil, false
		}

		if skipped {
			continue
		}

		kw, isDynamic := p.dynamicKeyword()
		if !isDynamic {
			p.i++

			continue
		}

		p.i += len(kw)

		if text, ok := p.dynamicArgument(kw); ok {
			texts = append(texts, text)
		}
	}

	return texts, true
}

// nestsMSSQLDynamicSQL reports whether an already-normalized statement carries
// dynamic SQL of its own — the depth-2 case, which is refused rather than
// unwrapped.
//
// `EXEC` / `EXECUTE` count only when what follows is a `(` or a string literal:
// a bare `EXEC dbo.p` inside dynamic SQL is a procedure call, not a second
// level of dynamic SQL, and refusing it would be a blanket refusal this change
// has no business making. `sp_executesql` counts on sight, because its argument
// is a statement whether it is spelled as a literal or as a variable.
func nestsMSSQLDynamicSQL(sql string) bool {
	p := &useScanner{s: sql, syntax: syntaxStandard}

	for !p.eof() {
		skipped, ok := p.skipOpaque()
		if !ok {
			return true
		}

		if skipped {
			continue
		}

		kw, isDynamic := p.dynamicKeyword()
		if !isDynamic {
			p.i++

			continue
		}

		p.i += len(kw)

		if kw == kwSPExecuteSQL {
			return true
		}

		p.spaces()

		if !p.eof() && (p.s[p.i] == '(' || p.atStringLiteral()) {
			return true
		}
	}

	return false
}

// The three spellings of "run this text as a statement". Longest first, so
// EXECUTE is never read as EXEC followed by `UTE`.
const (
	kwExecute      = "EXECUTE"
	kwExec         = "EXEC"
	kwSPExecuteSQL = "SP_EXECUTESQL"
)

// skipOpaque steps the scanner over a run whose contents cannot start a
// statement: a string literal, a quoted identifier, a bracketed identifier. It
// reports whether it moved, and false when such a run is left open.
//
// Literals are skipped here and read in dynamicArgument instead, which is the
// whole distinction this file turns on: a literal is inert *unless* it is the
// argument of one of the three keywords below.
func (p *useScanner) skipOpaque() (bool, bool) {
	if end, isRun, ok := scanVerbatimRun(p.s, p.i, p.syntax); isRun {
		if !ok {
			return false, false
		}

		p.i = end

		return true, true
	}

	if p.s[p.i] == '[' {
		end, ok := p.bracketEnd()
		if !ok {
			return false, false
		}

		p.i = end

		return true, true
	}

	return false, true
}

// dynamicKeyword reports which of the three keywords starts at the scanner's
// position, as a whole word. It does not consume.
//
// Unlike useKeywordHere it tolerates a `.` in front, because `sys.sp_executesql`
// is how the procedure is routinely qualified. That costs nothing: `EXEC` and
// `EXECUTE` are reserved words in T-SQL and cannot be a column, and a match that
// turns out not to be followed by a statement argument is simply skipped.
func (p *useScanner) dynamicKeyword() (string, bool) {
	if p.i > 0 && isUseNameByte(p.s[p.i-1], p.syntax) {
		return "", false
	}

	for _, kw := range []string{kwExecute, kwExec, kwSPExecuteSQL} {
		end := p.i + len(kw)
		if end > len(p.s) || !equalFoldASCII(p.s[p.i:end], kw) {
			continue
		}

		if end < len(p.s) && isUseNameByte(p.s[end], p.syntax) {
			continue
		}

		return kw, true
	}

	return "", false
}

// dynamicArgument reads the statement text the keyword at the scanner's
// position runs, when — and only when — that text is a literal.
//
// `EXEC` and `EXECUTE` take it parenthesized; `sp_executesql` takes it as its
// first argument, with whatever parameter list follows left alone. A leading
// `N` (national character set) is part of the literal's spelling, not of the
// text.
//
// Anything else — a variable, a concatenation, a procedure name — reports false
// with the scanner left just past the keyword, so the outer scan carries on
// from there.
func (p *useScanner) dynamicArgument(kw string) (string, bool) {
	saved := p.i

	p.spaces()

	if kw != kwSPExecuteSQL {
		if p.eof() || p.s[p.i] != '(' {
			p.i = saved

			return "", false
		}

		p.i++

		p.spaces()
	}

	text, ok := p.stringLiteral()
	if !ok {
		p.i = saved

		return "", false
	}

	if kw == kwSPExecuteSQL {
		return text, true
	}

	p.spaces()

	// A concatenation (`EXEC('a' + @b)`) lands here with a `+` rather than the
	// closing paren: the text is not the whole argument, so dbbat has not read
	// the statement and must not claim to have.
	if p.eof() || p.s[p.i] != ')' {
		p.i = saved

		return "", false
	}

	p.i++

	return text, true
}

// atStringLiteral reports whether a (possibly `N`-prefixed) string literal opens
// at the scanner's position.
func (p *useScanner) atStringLiteral() bool {
	i := p.i
	if i < len(p.s) && (p.s[i] == 'N' || p.s[i] == 'n') {
		i++
	}

	return i < len(p.s) && p.s[i] == '\''
}

// stringLiteral consumes the literal at the scanner's position and returns its
// contents with `”` doubling undone.
func (p *useScanner) stringLiteral() (string, bool) {
	if !p.atStringLiteral() {
		return "", false
	}

	if p.s[p.i] != '\'' {
		p.i++
	}

	return p.delimited('\'')
}

// equalFoldASCII is strings.EqualFold restricted to ASCII, which is all a
// keyword match needs and avoids the Unicode folding that would let a
// lookalike rune spell a keyword.
func equalFoldASCII(s, upper string) bool {
	if len(s) != len(upper) {
		return false
	}

	for i := range len(s) {
		c := s[i]
		if c >= 'a' && c <= 'z' {
			c -= 'a' - 'A'
		}

		if c != upper[i] {
			return false
		}
	}

	return true
}
