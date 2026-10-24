package shared

import "strings"

// SplitPostgresStatements splits a simple-query string into its statements on
// every `;` that sits outside a comment, a quoted literal or identifier, an
// E” string and a dollar-quoted body. Comments are normalized first, like
// every other check on the PostgreSQL path. Blank parts are dropped.
//
// It never fails: an unterminated literal swallows the rest of the string into
// the last part, which is what the server will choke on anyway.
func SplitPostgresStatements(sql string) []string {
	s := matchableSQL(sql, syntaxStandard)

	var (
		parts []string
		start int
	)

	flush := func(end int) {
		if part := strings.TrimSpace(s[start:end]); part != "" {
			parts = append(parts, part)
		}
	}

	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == ';':
			flush(i)

			start = i + 1
		case c == '\'':
			i = skipPGQuoted(s, i, '\'', i > 0 && (s[i-1] == 'e' || s[i-1] == 'E') && (i == 1 || !isPGIdentByte(s[i-2])))
		case c == '"':
			i = skipPGQuoted(s, i, '"', false)
		case c == '$' && (i == 0 || !isPGIdentByte(s[i-1])):
			i = skipPGDollar(s, i)
		}
	}

	flush(len(s))

	return parts
}

// skipPGQuoted returns the index of the closing quote of the literal opening at
// i (or the last byte, when unterminated). A doubled quote is an escaped one;
// backslash escapes only inside an E” string.
func skipPGQuoted(s string, i int, q byte, backslash bool) int {
	for j := i + 1; j < len(s); j++ {
		switch {
		case backslash && s[j] == '\\':
			j++
		case s[j] == q:
			if j+1 < len(s) && s[j+1] == q {
				j++

				continue
			}

			return j
		}
	}

	return len(s) - 1
}

// skipPGDollar returns the index of the last byte of the dollar-quoted body
// opening at i, or i itself when it is not a dollar quote (`$1`, a lone `$`).
func skipPGDollar(s string, i int) int {
	j := i + 1
	if j < len(s) && s[j] >= '0' && s[j] <= '9' {
		return i
	}

	for j < len(s) && s[j] != '$' && isPGIdentByte(s[j]) {
		j++
	}

	if j >= len(s) || s[j] != '$' {
		return i
	}

	tag := s[i : j+1]
	if end := strings.Index(s[j+1:], tag); end >= 0 {
		return j + 1 + end + len(tag) - 1
	}

	return len(s) - 1
}

func isPGIdentByte(c byte) bool {
	return c == '_' || c == '$' || c >= 0x80 ||
		(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// IsPostgresTxEnd reports whether one statement ends the current transaction:
// COMMIT, END, ABORT or ROLLBACK. ROLLBACK TO SAVEPOINT (and the PREPARED
// forms) leave the transaction running and do not count.
func IsPostgresTxEnd(stmt string) bool {
	fields := strings.Fields(strings.ToUpper(matchableSQL(stmt, syntaxStandard)))
	if len(fields) == 0 {
		return false
	}

	switch fields[0] {
	case "COMMIT", "END", "ABORT", "ROLLBACK":
	default:
		return false
	}

	if len(fields) > 1 && (fields[1] == "TO" || fields[1] == "PREPARED" || fields[1] == "SAVEPOINT") {
		return false
	}

	return true
}
