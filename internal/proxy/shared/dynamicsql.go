package shared

import "strings"

// Dynamic SQL — a statement whose real payload is a string the server parses as
// a statement of its own — breaks the invariant every prefix-shaped validator in
// this package rests on: that a statement's first keyword says what it does, and
// that no statement begins inside a string literal. `EXEC('DELETE FROM t')`,
// `PREPARE s FROM 'DELETE FROM t'` and `BEGIN EXECUTE IMMEDIATE 'DELETE FROM t';
// END;` are all writes that no amount of looking at the outer statement's first
// keyword will ever see, so `read_only`, `block_ddl`, `block_copy` and the
// database-switch refusal walked straight past them.
//
// This file is the one static reader of those forms, for all three protocols
// that have them. What it hands back the caller runs the **same** classification
// over that the outer statement got — a second, laxer rule set for dynamic SQL
// would turn every control into a suggestion.
//
// Same contract as sqlcomments.go and usedb.go: the scan runs over the
// comment-normalized scratch copy, and what comes back is the inner statement
// text as the *client wrote it*, for matching only. The bytes relayed upstream
// are never any of this.

// DynamicSQL is what one static read of a statement found out about the dynamic
// SQL it carries. The two fields are emphatically different answers and are
// never collapsed into one:
//
//   - Payloads is what dbbat *read*: inner statement text, to be checked.
//   - Opaque says a dynamic-SQL form is there whose payload dbbat could not read
//     statically — `EXEC(@sql)`, `PREPARE … FROM @v`, `EXECUTE IMMEDIATE
//     v_stmt`. "There is nothing here to check" must never be how "there is
//     something here I could not read" is reported, which is why it is its own
//     field and not an empty Payloads.
//
// The boolean the extractors return alongside it is the third answer: false
// means a dynamic-SQL form was found that dbbat could not read *at all*, or that
// nests a second level. That is the fail-closed path and the caller refuses.
type DynamicSQL struct {
	// Payloads are the statically decidable inner statement texts, in the order
	// they appear.
	Payloads []string
	// Opaque reports that the statement carries a dynamic-SQL form whose payload
	// is built at run time. Whether that is refused is a grant question, not a
	// parsing one — see ValidateDynamicSQL.
	Opaque bool
}

// MSSQLDynamicSQL reads every dynamic-SQL form in a T-SQL batch whose text dbbat
// can see — `EXEC('…')`, `EXECUTE('…')`, an all-literal concatenation
// (`EXEC('DELETE ' + 'FROM t')`), and the SQL-carrying system procedures sent as
// batch text (`sp_executesql`, `sp_prepare`, `sp_prepexec`, `sp_cursorprepare`,
// `sp_cursoropen`, `sp_cursorprepexec`, `sp_prepexecrpc`) — with the doubled
// quotes that escape a quote inside a literal undone.
//
// `false` means the batch carries dynamic SQL dbbat cannot vouch for and the
// caller must refuse it: either a quoted run was left open, or the extracted
// text nests *another* dynamic-SQL form. The nesting case is refused rather than
// unwrapped a second time — one level is where the recursion stops, and stopping
// silently would be a hole the shape of the one this file closes.
//
// A form whose argument is not a literal — `EXEC(@sql)`, `EXEC('a' + @b)` —
// comes back as Opaque rather than as an error: whether that is refused depends
// on the grant (ValidateDynamicSQL), not on the parse. `EXEC dbo.some_proc` is
// neither: it is an ordinary procedure call, which `describeRPCRequest` already
// fails closed on under a restrictive grant.
func MSSQLDynamicSQL(sql string) (DynamicSQL, bool) {
	return mssqlDynamicSQLAt(matchableSQL(sql, syntaxStandard))
}

// mssqlDynamicSQLAt is MSSQLDynamicSQL over an already-normalized batch.
func mssqlDynamicSQLAt(matchable string) (DynamicSQL, bool) {
	dyn, ok := mssqlDynamicSQL(matchable)
	if !ok {
		return DynamicSQL{}, false
	}

	for _, text := range dyn.Payloads {
		if nestsMSSQLDynamicSQL(matchableSQL(text, syntaxStandard)) {
			return DynamicSQL{}, false
		}
	}

	return dyn, true
}

// mssqlDynamicSQL does one level of extraction over an already-normalized batch.
func mssqlDynamicSQL(sql string) (DynamicSQL, bool) {
	p := &useScanner{s: sql, syntax: syntaxStandard}

	var dyn DynamicSQL

	for !p.eof() {
		skipped, ok := p.skipOpaque()
		if !ok {
			return DynamicSQL{}, false
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
			return DynamicSQL{}, false

		case dynamicReadable:
			dyn.Payloads = append(dyn.Payloads, text)

		case dynamicUndecidable:
			// Dynamic SQL whose text the server builds at run time. Recorded, not
			// refused here: the refusal is the grant's call.
			dyn.Opaque = true

		case dynamicNotDynamic:
			// Not dynamic SQL at all (a bare `EXEC dbo.p`). Ordinary text.
		}
	}

	return dyn, true
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
	// something dbbat cannot read statically: a variable, a concatenation with a
	// non-literal operand. Reported as DynamicSQL.Opaque.
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
// has no business making. A SQL-carrying system procedure counts on sight,
// because its argument is a statement whether it is spelled as a literal or as
// a variable.
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

		if _, isProc := MSSQLStatementParamIndex(kw); isProc {
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

// mssqlStatementProcs is where each SQL-carrying system procedure puts its
// statement when the call is positional — the documented parameter orders of
// MS-TDS 3.2.5.4 and the sp_* reference.
//
// It lives here, next to the parameter-*name* list, for the same reason that
// list does: **two** paths need it. The RPC path reads it off a decoded
// parameter list (internal/proxy/mssql/rpc.go), and the batch-text scanner in
// this file reads it off `EXEC sp_prepare @h OUT, NULL, N'DROP TABLE Foo', 1`.
// For a while only the RPC path implemented positional indexing at all, so that
// exact batch went unchecked while the RPC spelling of it was refused — the same
// drift the shared name list was introduced to end, one column over.
var mssqlStatementProcs = map[string]int{
	"sp_executesql":     0, // @stmt, @params, values…
	"sp_prepare":        2, // @handle OUT, @params, @stmt, @options
	"sp_prepexec":       2, // @handle OUT, @params, @stmt, values…
	"sp_cursorprepare":  2, // @handle OUT, @params, @stmt, @options, …
	"sp_cursoropen":     1, // @cursor OUT, @stmt, @scrollopt, …
	"sp_cursorprepexec": 3, // @handle OUT, @cursor OUT, @params, @stmt, …
	"sp_prepexecrpc":    1, // @handle OUT, @rpccall
}

// MSSQLStatementParamIndex returns the position of a SQL-carrying system
// procedure's statement argument, by procedure name, and whether the name is
// one of them at all.
func MSSQLStatementParamIndex(proc string) (int, bool) {
	idx, ok := mssqlStatementProcs[strings.ToLower(proc)]

	return idx, ok
}

// The spellings of "run this text as a statement" the batch scanner looks for.
// Longest first within each family, so EXECUTE is never read as EXEC followed by
// `UTE` and `sp_prepexecrpc` never as `sp_prepexec` followed by `rpc` — the
// trailing word-boundary check makes that belt and braces rather than the only
// guard.
const (
	kwExecute = "EXECUTE"
	kwExec    = "EXEC"
)

var mssqlDynamicKeywords = []string{
	kwExecute, kwExec,
	"SP_CURSORPREPEXEC", "SP_CURSORPREPARE", "SP_CURSOROPEN",
	"SP_PREPEXECRPC", "SP_PREPEXEC", "SP_PREPARE",
	"SP_EXECUTESQL",
}

// skipOpaque steps the scanner over a run whose contents cannot start a
// statement: a string literal, a quoted identifier, a bracketed identifier. It
// reports whether it moved, and false when such a run is left open.
//
// Literals are skipped here and read in dynamicArgument instead, which is the
// whole distinction this file turns on: a literal is inert *unless* it is the
// argument of one of the keywords above.
func (p *useScanner) skipOpaque() (bool, bool) {
	if skipped, ok := p.skipLiteral(); !ok || skipped {
		return skipped, ok
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

// skipLiteral is skipOpaque without the bracketed-identifier rule, for the
// dialects where `[` is not a delimiter. Skipping to a `]` that is not a closing
// delimiter could step the scan over a dynamic-SQL keyword, so the rule is
// applied only where it is real.
func (p *useScanner) skipLiteral() (bool, bool) {
	if end, isRun, ok := scanVerbatimRun(p.s, p.i, p.syntax); isRun {
		if !ok {
			return false, false
		}

		p.i = end

		return true, true
	}

	return false, true
}

// dynamicKeyword reports which of the keywords above starts at the scanner's
// position, as a whole word. It does not consume.
//
// Unlike useKeywordHere it tolerates a `.` in front, because `sys.sp_executesql`
// is how these procedures are routinely qualified. That costs nothing: `EXEC` and
// `EXECUTE` are reserved words in T-SQL and cannot be a column, and a match that
// turns out not to be followed by a statement argument is simply skipped.
func (p *useScanner) dynamicKeyword() (string, bool) {
	// Cheap gate: every keyword above starts with E or S, and this runs at every
	// byte of every batch on the protocol.
	if c := p.s[p.i]; c != 'E' && c != 'e' && c != 'S' && c != 's' {
		return "", false
	}

	if p.i > 0 && isUseNameByte(p.s[p.i-1], p.syntax) {
		return "", false
	}

	for _, kw := range mssqlDynamicKeywords {
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
// position runs, when — and only when — that text is a literal (or a
// concatenation of nothing but literals). A leading `N` (national character set)
// is part of the literal's spelling, not of the text.
func (p *useScanner) dynamicArgument(kw string) (string, dynamicOutcome) {
	if idx, isProc := MSSQLStatementParamIndex(kw); isProc {
		return p.procStatement(idx)
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
		// time. Undecidable, which is a grant question rather than a parse error.
		p.i = saved

		return "", dynamicUndecidable
	}

	text, ok := p.stringLiteral()
	if !ok {
		return "", dynamicUnreadable
	}

	text, outcome := p.foldLiteralConcatenation(text, '+', p.atStringLiteral, p.stringLiteral)
	if outcome != dynamicReadable {
		p.i = saved

		return "", outcome
	}

	p.spaces()

	// Anything other than the closing paren here means the argument is not the
	// text dbbat just read — so dbbat has not read the statement and must not
	// claim to have.
	if p.eof() || p.s[p.i] != ')' {
		p.i = saved

		return "", dynamicUndecidable
	}

	p.i++

	return text, dynamicReadable
}

// procStatement locates a SQL-carrying system procedure's statement argument in
// its argument list and reads it. target is the position the statement occupies
// in a positional call — MSSQLStatementParamIndex, the same table the RPC path
// reads.
//
// T-SQL lets any procedure's arguments be passed **by name, in any order**, so
// the statement is not necessarily at that position:
// `EXEC sp_executesql @params = N'@x int', @stmt = N'DROP TABLE Foo'` is the
// same call as the positional one. Recognizing only the positional form let
// every named spelling walk past as an inert literal, clearing read_only,
// block_ddl, block_copy and the database-switch refusal in one go; recognizing
// only position 0 did the same to every procedure whose statement is not first.
//
// Which parameter names can be the statement is not decided here either: it is
// IsMSSQLStatementParamName. Two lists of "what names the statement" is
// precisely the drift that becomes an authorization bug — the client would only
// have to pick the spelling dbbat's copy had not heard of.
//
// Failing to find it at all is dynamicUnreadable, not "nothing to check": the
// keyword says a statement is being run, so not finding it means dbbat is
// looking in the wrong place, never that there is nothing there.
func (p *useScanner) procStatement(target int) (string, dynamicOutcome) {
	pos := 0

	for {
		p.spaces()

		if p.eof() || p.s[p.i] == ';' {
			break
		}

		name, named := p.argumentName()

		// The named statement slot, or — for a positional call, which is what
		// every driver sends — the documented position.
		if (named && IsMSSQLStatementParamName(name)) || (!named && pos == target) {
			return p.statementArgumentValue()
		}

		pos++

		if !p.skipArgumentValue(p.skipOpaque) {
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
		// run time. Undecidable — this is the shape half the DBA scripts in the
		// world are written in, so whether it is refused is the grant's call.
		return "", dynamicUndecidable
	}

	text, ok := p.stringLiteral()
	if !ok {
		return "", dynamicUnreadable
	}

	// The literal has to be the *whole* value. `@stmt = N'USE ' + @db` continues
	// with a `+`: dbbat holds part of the statement, not the statement. A `+`
	// whose other operands are all literals is folded and *is* the statement.
	//
	// `+` specifically, and not "anything that is not a comma", because the
	// argument list of an unparenthesized `EXEC` ends without punctuation:
	// `EXEC sp_executesql N'DROP TABLE Foo'` followed by a newline and the next
	// statement of the batch is a complete call, and treating that as
	// undecidable would hand back the bypass this function exists to close.
	// Concatenation is the only way a string value continues in T-SQL.
	return p.foldLiteralConcatenation(text, '+', p.atStringLiteral, p.stringLiteral)
}

// foldLiteralConcatenation appends every operand of a concatenation chain that
// is itself a literal, so `'DELETE ' + 'FROM t'` reads as the one statement the
// server runs. op is the concatenation operator's first byte; at and read are
// the dialect's literal predicate and reader.
//
// A chain with one non-literal operand is dynamicUndecidable — dbbat holds part
// of a statement, which is not a statement — and a literal left open is
// dynamicUnreadable.
func (p *useScanner) foldLiteralConcatenation(
	text string,
	op byte,
	at func() bool,
	read func() (string, bool),
) (string, dynamicOutcome) {
	for {
		saved := p.i

		p.spaces()

		if p.eof() || p.s[p.i] != op {
			p.i = saved

			return text, dynamicReadable
		}

		if !p.concatOperator(op) {
			return "", dynamicUnreadable
		}

		p.spaces()

		if !at() {
			return "", dynamicUndecidable
		}

		next, ok := read()
		if !ok {
			return "", dynamicUnreadable
		}

		text += next
	}
}

// concatOperator consumes the concatenation operator at the scanner's position:
// one byte in T-SQL (`+`), two in Oracle (`||`). A lone `|` is not an operator
// this file knows, which is the fail-closed direction.
func (p *useScanner) concatOperator(op byte) bool {
	if op != '|' {
		p.i++

		return true
	}

	if p.i+1 >= len(p.s) || p.s[p.i+1] != '|' {
		return false
	}

	p.i += 2

	return true
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
//
// skip is the dialect's opaque-run stepper: brackets are identifier delimiters
// in T-SQL and are not in PL/SQL.
func (p *useScanner) skipArgumentValue(skip func() (bool, bool)) bool {
	depth := 0

	for !p.eof() {
		skipped, ok := skip()
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

// --- Oracle -----------------------------------------------------------------

// The two PL/SQL forms that run a string as a statement. `EXECUTE IMMEDIATE` is
// the common one and needs no PL/SQL block of its own to be dangerous — a client
// wraps it in `BEGIN … END;` and sends it as one ordinary statement, which is
// exactly why the scan below is not anchored at the start of the text.
const (
	oracleExecuteImmediate = "EXECUTE IMMEDIATE"
	oracleDBMSSQLParse     = "DBMS_SQL.PARSE"
)

// dbmsSQLParseStatementIndex is where DBMS_SQL.PARSE takes the statement:
// PARSE(c, statement, language_flag).
const dbmsSQLParseStatementIndex = 1

// OracleDynamicSQL reads every dynamic-SQL form in an Oracle statement whose
// text dbbat can see: `EXECUTE IMMEDIATE '<literal>'` — at the top level or
// inside a `BEGIN … END;` block, alone or concatenated from nothing but
// literals — and `DBMS_SQL.PARSE(c, '<literal>', …)`.
//
// Same three answers as the T-SQL reader: extracted payloads, an Opaque flag for
// `EXECUTE IMMEDIATE v_stmt`, and `false` for a form dbbat could not read at all
// or one that nests a second level.
func OracleDynamicSQL(sql string) (DynamicSQL, bool) {
	return oracleDynamicSQLAt(matchableSQL(sql, syntaxStandard))
}

// oracleDynamicSQLAt is OracleDynamicSQL over an already-normalized statement.
func oracleDynamicSQLAt(matchable string) (DynamicSQL, bool) {
	dyn, ok := oracleDynamicSQL(matchable)
	if !ok {
		return DynamicSQL{}, false
	}

	for _, text := range dyn.Payloads {
		if nestsOracleDynamicSQL(matchableSQL(text, syntaxStandard)) {
			return DynamicSQL{}, false
		}
	}

	return dyn, true
}

// oracleDynamicSQL does one level of extraction over an already-normalized
// statement.
func oracleDynamicSQL(sql string) (DynamicSQL, bool) {
	p := &useScanner{s: sql, syntax: syntaxStandard}

	var dyn DynamicSQL

	for !p.eof() {
		skipped, ok := p.skipLiteral()
		if !ok {
			return DynamicSQL{}, false
		}

		if skipped {
			continue
		}

		kw, isDynamic := p.oracleDynamicKeyword()
		if !isDynamic {
			p.i++

			continue
		}

		text, outcome := p.oracleDynamicArgument(kw)

		switch outcome {
		case dynamicUnreadable:
			return DynamicSQL{}, false
		case dynamicReadable:
			dyn.Payloads = append(dyn.Payloads, text)
		case dynamicUndecidable:
			dyn.Opaque = true
		case dynamicNotDynamic:
		}
	}

	return dyn, true
}

// nestsOracleDynamicSQL reports whether an already-normalized statement carries
// dynamic SQL of its own — the depth-2 case, refused rather than unwrapped.
func nestsOracleDynamicSQL(sql string) bool {
	p := &useScanner{s: sql, syntax: syntaxStandard}

	for !p.eof() {
		skipped, ok := p.skipLiteral()
		if !ok {
			return true
		}

		if skipped {
			continue
		}

		if _, isDynamic := p.oracleDynamicKeyword(); isDynamic {
			return true
		}

		p.i++
	}

	return false
}

// oracleDynamicKeyword consumes the dynamic-SQL keyword at the scanner's
// position and reports which one it was. It consumes nothing when there is none.
//
// A `.` in front is tolerated for DBMS_SQL, because `SYS.DBMS_SQL.PARSE` is how
// the package is routinely qualified.
func (p *useScanner) oracleDynamicKeyword() (string, bool) {
	// Cheap gate: both forms start with E or D, and this runs at every byte of
	// every statement on the protocol.
	if c := p.s[p.i]; c != 'E' && c != 'e' && c != 'D' && c != 'd' {
		return "", false
	}

	if p.i > 0 && isUseNameByte(p.s[p.i-1], p.syntax) {
		return "", false
	}

	saved := p.i

	if p.keyword("EXECUTE") {
		p.spaces()

		if p.keyword("IMMEDIATE") {
			return oracleExecuteImmediate, true
		}

		p.i = saved

		return "", false
	}

	if p.keyword("DBMS_SQL") {
		p.spaces()

		if !p.eof() && p.s[p.i] == '.' {
			p.i++

			p.spaces()

			if p.keyword("PARSE") {
				return oracleDBMSSQLParse, true
			}
		}

		p.i = saved

		return "", false
	}

	return "", false
}

// oracleDynamicArgument reads the statement text the keyword just consumed runs.
func (p *useScanner) oracleDynamicArgument(kw string) (string, dynamicOutcome) {
	if kw == oracleDBMSSQLParse {
		return p.dbmsSQLParseStatement()
	}

	return p.oracleStatementValue()
}

// oracleStatementValue reads a PL/SQL statement expression: a literal, or a
// concatenation of nothing but literals. Anything else is built at run time.
func (p *useScanner) oracleStatementValue() (string, dynamicOutcome) {
	p.spaces()

	if !p.atOracleLiteral() {
		// `EXECUTE IMMEDIATE v_stmt`: the statement is where it should be and is
		// built at run time. Undecidable — the grant decides.
		return "", dynamicUndecidable
	}

	text, ok := p.oracleLiteral()
	if !ok {
		return "", dynamicUnreadable
	}

	return p.foldLiteralConcatenation(text, '|', p.atOracleLiteral, p.oracleLiteral)
}

// dbmsSQLParseStatement locates DBMS_SQL.PARSE's statement argument and reads
// it. PL/SQL takes arguments positionally or by `name => value`, so both are
// recognized — the named spelling walking past unread would be the same bypass
// the T-SQL named spelling was.
func (p *useScanner) dbmsSQLParseStatement() (string, dynamicOutcome) {
	p.spaces()

	if p.eof() || p.s[p.i] != '(' {
		return "", dynamicUnreadable
	}

	p.i++

	pos := 0

	for {
		p.spaces()

		if p.eof() || p.s[p.i] == ')' {
			return "", dynamicUnreadable
		}

		name, named := p.plsqlArgumentName()

		if (named && name == "STATEMENT") || (!named && pos == dbmsSQLParseStatementIndex) {
			return p.oracleStatementValue()
		}

		pos++

		if !p.skipArgumentValue(p.skipLiteral) {
			return "", dynamicUnreadable
		}

		if p.eof() || p.s[p.i] != ',' {
			return "", dynamicUnreadable
		}

		p.i++
	}
}

// plsqlArgumentName reads a `name =>` at the scanner's position, upper-cased,
// consuming the arrow and the whitespace around it. It reports false — and
// consumes nothing — for a positional argument.
func (p *useScanner) plsqlArgumentName() (string, bool) {
	saved := p.i

	start := p.i
	for p.i < len(p.s) && isUseNameByte(p.s[p.i], p.syntax) {
		p.i++
	}

	if p.i == start {
		return "", false
	}

	name := p.s[start:p.i]

	p.spaces()

	if p.i+1 >= len(p.s) || p.s[p.i] != '=' || p.s[p.i+1] != '>' {
		p.i = saved

		return "", false
	}

	p.i += 2

	p.spaces()

	return strings.ToUpper(name), true
}

// atOracleLiteral reports whether a string literal opens at the scanner's
// position — plain, `N`-prefixed, or the `q'[…]'` quote operator (which is the
// spelling a statement containing quotes is most naturally written in).
func (p *useScanner) atOracleLiteral() bool {
	i := p.i
	if i < len(p.s) && (p.s[i] == 'N' || p.s[i] == 'n') {
		i++
	}

	if i+1 < len(p.s) && (p.s[i] == 'q' || p.s[i] == 'Q') && p.s[i+1] == '\'' {
		return true
	}

	return i < len(p.s) && p.s[i] == '\''
}

// oracleLiteral consumes the literal at the scanner's position and returns its
// contents, with `”` doubling undone in the ordinary form and taken verbatim in
// the quote-operator form (where nothing needs escaping).
func (p *useScanner) oracleLiteral() (string, bool) {
	i := p.i
	if p.s[i] == 'N' || p.s[i] == 'n' {
		i++
	}

	if i+1 < len(p.s) && (p.s[i] == 'q' || p.s[i] == 'Q') && p.s[i+1] == '\'' {
		end, ok := scanQuoteOperator(p.s, i)
		if !ok {
			return "", false
		}

		text := p.s[i+3 : end-2]
		p.i = end

		return text, true
	}

	p.i = i

	if p.eof() || p.s[p.i] != '\'' {
		return "", false
	}

	return p.delimited('\'')
}

// --- MySQL / MariaDB --------------------------------------------------------

// MySQL's `PREPARE <name> FROM '<statement>'` is the same construct wearing
// different syntax: the literal is parsed and, on the next `EXECUTE <name>`,
// run. MariaDB's `EXECUTE IMMEDIATE '<statement>'` collapses the pair into one
// statement. Either way the payload is text every prefix-shaped check steps
// over — `PREPARE` matches no write or DDL keyword and no blocked pattern.
//
// Only the *decidable* form is readable, exactly as on the other two protocols:
// `PREPARE s FROM @sql` and `PREPARE s FROM CONCAT('USE ', @db)` are assembled
// by the server from values dbbat never sees. See docs/mysql.md.

// MySQLPrepareKind distinguishes the two halves of MySQL's text-protocol
// prepared-statement pair.
type MySQLPrepareKind int

const (
	// MySQLPrepareNone — the statement is neither half of the pair. It may still
	// carry dynamic SQL: MariaDB's `EXECUTE IMMEDIATE` names nothing.
	MySQLPrepareNone MySQLPrepareKind = iota
	// MySQLPrepareDefine — `PREPARE <name> FROM …`.
	MySQLPrepareDefine
	// MySQLPrepareExecute — `EXECUTE <name> [USING …]`.
	MySQLPrepareExecute
)

// MySQLPrepare is one statement of MySQL's text-protocol prepared-statement
// pair, read statically.
//
// Name and Kind exist because the two halves are one decision spread over two
// statements: `EXECUTE <name>` carries no text at all, so whether it may run can
// only be answered from what the matching `PREPARE` said — which is session
// state, and lives in the MySQL proxy rather than here.
type MySQLPrepare struct {
	Kind    MySQLPrepareKind
	Name    string
	Dynamic DynamicSQL
}

// MySQLPreparedStatement reads sql as one half of the prepared-statement pair,
// or as an `EXECUTE IMMEDIATE`.
//
// `false` is the fail-closed path: a `PREPARE` dbbat could not read all the way
// down — an unterminated literal, a text that is not one single literal (MySQL
// concatenates adjacent ones), or a nested `PREPARE`/`EXECUTE`. Unwrapping stops
// at one level, and stops loudly: ending silently would leave a hole the exact
// shape of the one this closes.
func MySQLPreparedStatement(sql string) (MySQLPrepare, bool) {
	return mysqlPreparedStatement(matchableSQL(sql, syntaxMySQL))
}

// MySQLDynamicSQL is MySQLPreparedStatement's dynamic-SQL half, for the callers
// that only need the payloads.
func MySQLDynamicSQL(sql string) (DynamicSQL, bool) {
	prepared, ok := MySQLPreparedStatement(sql)

	return prepared.Dynamic, ok
}

// mysqlPreparedStatement works over an already-normalized statement.
func mysqlPreparedStatement(matchable string) (MySQLPrepare, bool) {
	prepared, ok := mysqlPrepared(matchable)
	if !ok {
		return MySQLPrepare{}, false
	}

	for _, text := range prepared.Dynamic.Payloads {
		if nestsMySQLDynamicSQL(matchableSQL(text, syntaxMySQL)) {
			return MySQLPrepare{}, false
		}
	}

	return prepared, true
}

// nestsMySQLDynamicSQL reports whether a prepared statement's text is itself one
// half of the pair. MySQL does not allow either inside a prepared statement, so
// refusing loses nothing legitimate and refuses to guess.
func nestsMySQLDynamicSQL(sql string) bool {
	p := &useScanner{s: sql, syntax: syntaxMySQL}

	p.spaces()

	return p.keyword("PREPARE") || p.keyword("EXECUTE")
}

// mysqlPrepared does one level of extraction over an already-normalized
// statement.
//
// The match is anchored at the start, which is sound on this protocol rather
// than merely convenient: the client leg does not negotiate
// CLIENT_MULTI_STATEMENTS, so one COM_QUERY carries one statement.
func mysqlPrepared(sql string) (MySQLPrepare, bool) {
	p := &useScanner{s: sql, syntax: syntaxMySQL}

	p.spaces()

	if p.keyword("EXECUTE") {
		return p.mysqlExecute()
	}

	if !p.keyword("PREPARE") {
		return MySQLPrepare{}, true
	}

	p.spaces()

	name, ok := p.name()
	if !ok {
		return MySQLPrepare{}, false
	}

	p.spaces()

	if !p.keyword("FROM") {
		return MySQLPrepare{}, false
	}

	prepared := MySQLPrepare{Kind: MySQLPrepareDefine, Name: name}

	dyn, ok := p.mysqlStatementValue(false)
	if !ok {
		return MySQLPrepare{}, false
	}

	prepared.Dynamic = dyn

	return prepared, true
}

// mysqlExecute reads what follows `EXECUTE`: MariaDB's `IMMEDIATE '<literal>'`,
// or the name of a statement prepared earlier.
func (p *useScanner) mysqlExecute() (MySQLPrepare, bool) {
	p.spaces()

	saved := p.i

	if p.keyword("IMMEDIATE") {
		dyn, ok := p.mysqlStatementValue(true)
		if !ok {
			return MySQLPrepare{}, false
		}

		return MySQLPrepare{Dynamic: dyn}, true
	}

	p.i = saved

	name, ok := p.name()
	if !ok {
		return MySQLPrepare{}, false
	}

	return MySQLPrepare{Kind: MySQLPrepareExecute, Name: name}, true
}

// mysqlStatementValue reads the statement expression at the scanner's position.
//
// allowUsing admits the `USING <values>` clause MariaDB's `EXECUTE IMMEDIATE`
// takes: those are bind values, not statement text, so stopping there yields the
// whole statement. Everything else trailing the literal fails closed —
// especially a second literal, because MySQL concatenates adjacent ones and
// dbbat would be holding half a statement.
func (p *useScanner) mysqlStatementValue(allowUsing bool) (DynamicSQL, bool) {
	p.spaces()

	// Not a literal: a user variable, a CONCAT, anything the server builds at
	// run time. Opaque rather than unreadable — a blanket refusal of PREPARE is
	// the grant's call to make, not the parser's.
	if p.eof() || (p.s[p.i] != '\'' && p.s[p.i] != '"') {
		return DynamicSQL{Opaque: true}, true
	}

	text, ok := p.mysqlLiteral()
	if !ok {
		return DynamicSQL{}, false
	}

	p.spaces()

	if allowUsing && p.keyword("USING") {
		return DynamicSQL{Payloads: []string{text}}, true
	}

	if !p.eof() && p.s[p.i] == ';' {
		p.i++
		p.spaces()
	}

	if !p.eof() {
		return DynamicSQL{}, false
	}

	return DynamicSQL{Payloads: []string{text}}, true
}

// mysqlLiteral consumes the string literal at the scanner's position — MySQL
// accepts both quote characters — and returns its contents.
//
// Doubling and backslash escapes both terminate correctly, which is what makes
// the end of the literal reliable. The escaped byte itself is copied rather than
// decoded (`\n` yields `n`): the text is only ever re-scanned for keywords and
// blocked patterns, and none of those needs an escape sequence to be spelled.
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
