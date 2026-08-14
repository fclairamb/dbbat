package shared

import "strings"

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

		text, outcome := p.dynamicArgument(kw)

		switch outcome {
		case dynamicUnreadable:
			// The keyword *is* one of the dynamic-SQL forms and the statement
			// argument is somewhere dbbat is not looking. Swallowing that and
			// carrying on is how a bypass stays silent, so it fails closed.
			return nil, false

		case dynamicReadable:
			texts = append(texts, text)

		case dynamicNotDynamic, dynamicUndecidable:
			// Not dynamic SQL (a bare `EXEC dbo.p`), or dynamic SQL whose text
			// the server builds at run time. Neither is refusable — see the
			// limitation note in docs/mssql.md.
		}
	}

	return texts, true
}

// dynamicOutcome is what one dynamic-SQL keyword turned out to be. The two
// middle cases look alike from the outside and are emphatically not: "there is
// nothing here to check" must never be how "there is something here I could not
// find" is reported.
type dynamicOutcome int

const (
	// dynamicNotDynamic — the keyword is not introducing dynamic SQL at all,
	// e.g. `EXEC dbo.some_proc`. Ordinary text; keep scanning.
	dynamicNotDynamic dynamicOutcome = iota
	// dynamicUndecidable — the statement argument was located and holds
	// something dbbat cannot read statically: a variable, a concatenation.
	// Allowed, and documented as a limitation rather than refused, because a
	// blanket refusal of runtime-built dynamic SQL breaks ordinary code.
	dynamicUndecidable
	// dynamicReadable — the statement text was extracted.
	dynamicReadable
	// dynamicUnreadable — the form is dynamic SQL and dbbat could not locate
	// its statement argument at all. Fail closed.
	dynamicUnreadable
)

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

// mssqlStatementParamNames are the names SQL Server accepts for the statement
// parameter of the SQL-carrying system procedures. The empty name is included
// because that is what every driver sends over RPC: those are positional calls.
//
// It lives in shared rather than in the mssql package because **two** paths ask
// the question — the RPC path, from a parsed parameter name, and the batch-text
// scanner in this file, from `@stmt = …` written out in a T-SQL batch. A second
// copy of this list is exactly the drift that becomes an authorization bug: a
// client would only have to pick the spelling one copy had not heard of.
var mssqlStatementParamNames = map[string]bool{
	"": true, "@stmt": true, "@statement": true, "@tsql": true, "@rpccall": true,
}

// IsMSSQLStatementParamName reports whether a parameter name could be the
// statement of a SQL-carrying system procedure.
func IsMSSQLStatementParamName(name string) bool {
	return mssqlStatementParamNames[strings.ToLower(name)]
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
// position runs, when — and only when — that text is a literal. A leading `N`
// (national character set) is part of the literal's spelling, not of the text.
func (p *useScanner) dynamicArgument(kw string) (string, dynamicOutcome) {
	if kw == kwSPExecuteSQL {
		return p.spExecuteSQLStatement()
	}

	return p.execParenStatement()
}

// execParenStatement reads the argument of `EXEC(…)` / `EXECUTE(…)`.
//
// No parenthesis means this was not dynamic SQL at all but an ordinary
// procedure call (`EXEC dbo.some_proc`), which keeps falling through as text.
func (p *useScanner) execParenStatement() (string, dynamicOutcome) {
	saved := p.i

	p.spaces()

	if p.eof() || p.s[p.i] != '(' {
		p.i = saved

		return "", dynamicNotDynamic
	}

	p.i++

	p.spaces()

	if !p.atStringLiteral() {
		// `EXEC(@sql)`: the statement is where it should be and is built at run
		// time. Undecidable, documented, not refused.
		p.i = saved

		return "", dynamicUndecidable
	}

	text, ok := p.stringLiteral()
	if !ok {
		return "", dynamicUnreadable
	}

	p.spaces()

	// A concatenation (`EXEC('a' + @b)`) lands here with a `+` rather than the
	// closing paren: the text is not the whole argument, so dbbat has not read
	// the statement and must not claim to have.
	if p.eof() || p.s[p.i] != ')' {
		p.i = saved

		return "", dynamicUndecidable
	}

	p.i++

	return text, dynamicReadable
}

// spExecuteSQLStatement locates `sp_executesql`'s statement argument in its
// argument list and reads it.
//
// T-SQL lets any procedure's arguments be passed **by name, in any order**, so
// the statement is not necessarily the token right after the keyword:
// `EXEC sp_executesql @params = N'@x int', @stmt = N'DROP TABLE Foo'` is the
// same call as the positional one. Recognizing only the positional form let
// every named spelling walk past as an inert literal, clearing read_only,
// block_ddl, block_copy and the database-switch refusal in one go.
//
// Which parameter names can be the statement is not decided here: it is
// IsMSSQLStatementParamName, the same set the RPC path enforces on. Two lists of
// "what names the statement" is precisely the drift that becomes an
// authorization bug — the client would only have to pick the spelling dbbat's
// copy had not heard of.
//
// Failing to find it at all is dynamicUnreadable, not "nothing to check": the
// keyword says a statement is being run, so not finding it means dbbat is
// looking in the wrong place, never that there is nothing there.
func (p *useScanner) spExecuteSQLStatement() (string, dynamicOutcome) {
	first := true

	for {
		p.spaces()

		if p.eof() || p.s[p.i] == ';' {
			break
		}

		name, named := p.argumentName()

		// The named statement slot, or — for a positional call, which is what
		// every driver sends — the first argument.
		if (named && IsMSSQLStatementParamName(name)) || (!named && first) {
			return p.statementArgumentValue()
		}

		first = false

		if !p.skipArgumentValue() {
			return "", dynamicUnreadable
		}

		if p.eof() || p.s[p.i] != ',' {
			break
		}

		p.i++
	}

	return "", dynamicUnreadable
}

// statementArgumentValue reads whatever sits in the statement slot.
func (p *useScanner) statementArgumentValue() (string, dynamicOutcome) {
	if !p.atStringLiteral() {
		// `EXEC sp_executesql @sql` and `@stmt = @sql`: located, and built at
		// run time. Undecidable, documented, not refused — this is the shape
		// half the DBA scripts in the world are written in.
		return "", dynamicUndecidable
	}

	text, ok := p.stringLiteral()
	if !ok {
		return "", dynamicUnreadable
	}

	p.spaces()

	// The literal has to be the *whole* value. `@stmt = N'USE ' + @db` lands
	// here with a `+`: dbbat holds part of the statement, not the statement, and
	// must not claim to have read it — the same call the parenthesized form
	// makes on `EXEC('a' + @b)`.
	//
	// `+` specifically, and not "anything that is not a comma", because the
	// argument list of an unparenthesized `EXEC` ends without punctuation:
	// `EXEC sp_executesql N'DROP TABLE Foo'` followed by a newline and the next
	// statement of the batch is a complete call, and treating that as
	// undecidable would hand back the bypass this function exists to close.
	// Concatenation is the only way a string value continues in T-SQL.
	if !p.eof() && p.s[p.i] == '+' {
		return "", dynamicUndecidable
	}

	return text, dynamicReadable
}

// argumentName reads a `@name =` at the scanner's position, consuming the `=`
// and the whitespace around it. It reports false — and consumes nothing — when
// the argument is positional, which is what tells `EXEC sp_executesql @sql`
// (a variable in the first slot) apart from `EXEC sp_executesql @stmt = …`.
func (p *useScanner) argumentName() (string, bool) {
	saved := p.i

	if p.eof() || p.s[p.i] != '@' {
		return "", false
	}

	start := p.i
	p.i++

	for p.i < len(p.s) && isUseNameByte(p.s[p.i], p.syntax) {
		p.i++
	}

	name := p.s[start:p.i]

	p.spaces()

	if p.eof() || p.s[p.i] != '=' {
		p.i = saved

		return "", false
	}

	p.i++

	p.spaces()

	return name, true
}

// skipArgumentValue advances past one argument's value, stopping at the comma
// that separates it from the next or at the end of the argument list. Literals,
// quoted identifiers and nested parentheses are stepped over whole, so a comma
// inside `CONCAT(a, b)` or inside a string does not end the argument early.
func (p *useScanner) skipArgumentValue() bool {
	depth := 0

	for !p.eof() {
		skipped, ok := p.skipOpaque()
		if !ok {
			return false
		}

		if skipped {
			continue
		}

		switch p.s[p.i] {
		case '(':
			depth++
		case ')':
			if depth == 0 {
				return true
			}

			depth--
		case ',', ';':
			if depth == 0 {
				return true
			}
		}

		p.i++
	}

	return true
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

// MySQL's `PREPARE <name> FROM '<statement>'` is the same construct wearing
// different syntax: the literal is parsed and, on the next `EXECUTE <name>`,
// run. `PREPARE s FROM 'USE otherdb'` therefore performs the very switch this
// package refuses, one statement later, through text every check above steps
// over — `PREPARE` matches no write or DDL keyword and no blocked pattern.
//
// Only the *decidable* form is readable, exactly as on the T-SQL side:
// `PREPARE s FROM @sql` and `PREPARE s FROM CONCAT('USE ', @db)` are assembled
// by the server from values dbbat never sees. See docs/mysql.md.

// MySQLPreparedText returns the statement text a `PREPARE <name> FROM
// '<literal>'` would later execute, read from the comment-normalized scratch
// copy under the MySQL dialect.
//
// The two returns are read together:
//
//	("", true)       nothing to check — not a PREPARE, or one whose text is
//	                 built at runtime and so is not statically decidable
//	("<text>", true) the statement text, for the caller to check
//	("", false)      a PREPARE dbbat could not read all the way down — an
//	                 unterminated literal, a text that is not one single
//	                 literal, or a nested PREPARE. Fail closed: the caller
//	                 refuses.
//
// Unwrapping stops at one level, and stops loudly. Recursion has to end
// somewhere, and ending silently would leave a hole the exact shape of the one
// this closes.
func MySQLPreparedText(sql string) (string, bool) {
	text, found, ok := mysqlPreparedText(matchableSQL(sql, syntaxMySQL))
	if !ok {
		return "", false
	}

	if !found {
		return "", true
	}

	// Any nested PREPARE at all, whatever form its text takes. MySQL does not
	// allow one inside a prepared statement, so nothing legitimate is lost.
	nested := &useScanner{s: matchableSQL(text, syntaxMySQL), syntax: syntaxMySQL}
	nested.spaces()

	if nested.keyword("PREPARE") {
		return "", false
	}

	return text, true
}

// mysqlPreparedText does one level of extraction over an already-normalized
// statement. It returns the text, whether there was one to find, and whether
// the statement could be read with confidence.
func mysqlPreparedText(sql string) (string, bool, bool) {
	p := &useScanner{s: sql, syntax: syntaxMySQL}

	p.spaces()

	if !p.keyword("PREPARE") {
		return "", false, true
	}

	p.spaces()

	if _, ok := p.name(); !ok {
		return "", false, false
	}

	p.spaces()

	if !p.keyword("FROM") {
		return "", false, false
	}

	p.spaces()

	// Not a literal: a user variable, a CONCAT, anything the server builds at
	// run time. Undecidable, and deliberately not refused — a blanket refusal
	// of PREPARE would break ordinary application code.
	if p.eof() || (p.s[p.i] != '\'' && p.s[p.i] != '"') {
		return "", false, true
	}

	text, ok := p.mysqlLiteral()
	if !ok {
		return "", false, false
	}

	p.spaces()

	if !p.eof() && p.s[p.i] == ';' {
		p.i++
		p.spaces()
	}

	// MySQL concatenates adjacent string literals, so `FROM 'USE ' 'otherdb'`
	// is one statement text dbbat has only half of. Anything trailing the
	// literal other than a `;` therefore fails closed rather than being read as
	// the whole text.
	if !p.eof() {
		return "", false, false
	}

	return text, true, true
}

// mysqlLiteral consumes the string literal at the scanner's position — MySQL
// accepts both quote characters — and returns its contents.
//
// Doubling and backslash escapes both terminate correctly, which is what makes
// the end of the literal reliable. The escaped byte itself is copied rather than
// decoded (`\n` yields `n`): this text is only ever re-scanned for a database
// switch, and no `USE <name>` needs an escape sequence to be spelled.
func (p *useScanner) mysqlLiteral() (string, bool) {
	q := p.s[p.i]
	p.i++

	var b strings.Builder

	for p.i < len(p.s) {
		switch c := p.s[p.i]; {
		case c == '\\':
			if p.i+1 >= len(p.s) {
				return "", false
			}

			b.WriteByte(p.s[p.i+1])
			p.i += 2

		case c != q:
			b.WriteByte(c)
			p.i++

		case p.i+1 < len(p.s) && p.s[p.i+1] == q:
			b.WriteByte(q)
			p.i += 2

		default:
			p.i++

			return b.String(), true
		}
	}

	return "", false
}
