package shared

// SELECT … INTO <table> creates a table and fills it, yet it starts with SELECT,
// so the prefix-shaped classifiers in validation.go read it as a read. Both
// PostgreSQL and SQL Server accept it as plain SQL:
//
//   - PostgreSQL: `SELECT a INTO [TEMP | UNLOGGED | TABLE] t FROM x`. The
//     read_only pin refuses it there anyway; block_ddl, a text check, did not.
//   - SQL Server: `SELECT a INTO t FROM x`, and `INTO #t` for a temp table. With
//     no session pin, the text is the control, and it passed both.
//
// It is deliberately not applied to the two other SQL dialects:
//
//   - MySQL/MariaDB have no SELECT … INTO <table> (`CREATE TABLE … SELECT` is
//     their spelling, caught by CREATE). Their `SELECT … INTO` names variables
//     (`@v`, or a routine's local variable) or a file, and the file forms are
//     refused outright by mysqlBlockedPatterns.
//   - Oracle has none either: `SELECT … INTO v` is PL/SQL, assigning a
//     variable, and appears inside blocks (`BEGIN SELECT … INTO v …; END;`) and
//     `WITH FUNCTION` bodies — an INTO followed by a bare name that is not a
//     table.

// selectIntoOwners are the statements an INTO can belong to. The one most
// recently opened at the INTO's parenthesis depth owns it: `INSERT INTO t`,
// `MERGE INTO t` and a DML's `OUTPUT … INTO log` are not SELECT … INTO, and
// `UPDATE t SET a = (SELECT b FROM u) OUTPUT inserted.a INTO log` must not read
// the subquery's SELECT as the INTO's owner.
var selectIntoOwners = []string{"SELECT", "INSERT", "UPDATE", "DELETE", "MERGE"}

// selectsIntoTable reports whether a comment-normalized text contains a
// SELECT … INTO whose target is a table, i.e. anything but a variable (`@v` on
// SQL Server, `:v` as a host or bind variable).
//
// It reads the whole text rather than its first statement, so a T-SQL batch
// (`SELECT 1 SELECT a INTO t FROM x`) and a PostgreSQL multi-statement query are
// covered too. `;` resets the owners. Literals and quoted identifiers are
// stepped over; tsql additionally steps over bracketed identifiers, and the
// PostgreSQL reading over dollar-quoted bodies (`[` is an array subscript
// there). A qualified name (`t.into`) and a variable (`@into`) are not the
// keyword.
//
// It fails closed: a quoted run left open reports true.
func selectsIntoTable(s string, tsql bool) bool {
	p := &useScanner{s: s, syntax: syntaxStandard}
	owners := []string{""}

	for !p.eof() {
		skipped, ok := p.skipSelectIntoVerbatim(tsql)
		if !ok {
			return true
		}

		if skipped {
			continue
		}

		if p.selectIntoPunctuation(&owners) {
			continue
		}

		if p.i > 0 {
			if prev := p.s[p.i-1]; isUseNameByte(prev, syntaxStandard) || prev == '.' {
				p.i++

				continue
			}
		}

		if owner, ok := p.selectIntoOwner(); ok {
			owners[len(owners)-1] = owner

			continue
		}

		if p.keyword("INTO") {
			if owners[len(owners)-1] == "SELECT" && p.intoTargetIsTable() {
				return true
			}

			continue
		}

		p.i++
	}

	return false
}

// skipSelectIntoVerbatim steps over a run that is not code: a literal or quoted
// identifier, and a bracketed identifier (tsql) or a dollar-quoted body
// (PostgreSQL). ok is false when a run is left open.
func (p *useScanner) skipSelectIntoVerbatim(tsql bool) (bool, bool) {
	if skipped, ok := p.skipLiteral(); skipped || !ok {
		return skipped, ok
	}

	c := p.s[p.i]

	if c == '[' && tsql {
		end, ok := p.bracketEnd()
		if !ok {
			return false, false
		}

		p.i = end

		return true, true
	}

	if c == '$' && !tsql && (p.i == 0 || !isPGIdentByte(p.s[p.i-1])) {
		if end := skipPGDollar(p.s, p.i); end != p.i {
			p.i = end + 1

			return true, true
		}
	}

	return false, true
}

// selectIntoPunctuation consumes a parenthesis or a `;` and keeps the per-depth
// owners in step with it.
func (p *useScanner) selectIntoPunctuation(owners *[]string) bool {
	switch p.s[p.i] {
	case '(':
		*owners = append(*owners, "")
	case ')':
		if len(*owners) > 1 {
			*owners = (*owners)[:len(*owners)-1]
		}
	case ';':
		*owners = (*owners)[:1]
		(*owners)[0] = ""
	default:
		return false
	}

	p.i++

	return true
}

// selectIntoOwner consumes one of selectIntoOwners at the scanner's position.
func (p *useScanner) selectIntoOwner() (string, bool) {
	for _, kw := range selectIntoOwners {
		if p.keyword(kw) {
			return kw, true
		}
	}

	return "", false
}

// intoTargetIsTable reads what follows an INTO: anything but a variable is a
// table. An INTO with nothing behind it is not a statement the server would
// run, and is reported as a table, failing closed.
func (p *useScanner) intoTargetIsTable() bool {
	p.spaces()

	if p.eof() {
		return true
	}

	c := p.s[p.i]

	return c != '@' && c != ':'
}
