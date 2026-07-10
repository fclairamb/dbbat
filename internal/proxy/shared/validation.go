package shared

import (
	"errors"
	"regexp"
	"strings"

	"github.com/fclairamb/dbbat/internal/store"
)

// Validation errors shared across proxy implementations.
var (
	ErrReadOnlyViolation     = errors.New("write operations not permitted with read-only access")
	ErrDDLBlocked            = errors.New("DDL operations not permitted: your access grant blocks schema modifications")
	ErrPasswordChangeBlocked = errors.New("password modification is not allowed through the proxy")
	ErrOraclePatternBlocked  = errors.New("blocked: this Oracle operation is not permitted through the proxy")
	ErrMySQLPatternBlocked   = errors.New("blocked: this MySQL operation is not permitted through the proxy")
)

// Write keywords that should be blocked for read-only grants.
// REPLACE is MySQL's upsert and writes data; included so MySQL read-only grants
// catch it. PG/Oracle never start a statement with REPLACE so it's a no-op there.
var writeKeywords = []string{
	"INSERT", "UPDATE", "DELETE", "DROP", "TRUNCATE",
	"CREATE", "ALTER", "GRANT", "REVOKE", "MERGE", "REPLACE",
}

// DDL keywords.
var ddlKeywords = []string{"CREATE", "ALTER", "DROP", "TRUNCATE"}

// Oracle-specific blocked patterns (always blocked regardless of grant controls).
var oracleBlockedPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)ALTER\s+SYSTEM`),
	regexp.MustCompile(`(?i)CREATE\s+DATABASE\s+LINK`),
	regexp.MustCompile(`(?i)DBMS_SCHEDULER`),
	regexp.MustCompile(`(?i)DBMS_JOB`),
	regexp.MustCompile(`(?i)UTL_HTTP`),
	regexp.MustCompile(`(?i)UTL_TCP`),
	regexp.MustCompile(`(?i)UTL_FILE`),
	regexp.MustCompile(`(?i)DBMS_PIPE`),
}

// MySQL-specific blocked patterns (always blocked regardless of grant controls).
//
//   - LOAD DATA [LOCAL] INFILE: file system reads from the MySQL server, and
//     LOCAL is a client-side data exfiltration vector — the upstream server
//     can ask the client to upload arbitrary local files.
//   - SELECT ... INTO OUTFILE / DUMPFILE: writes files to the MySQL server FS.
//   - SET GLOBAL: server-wide variable changes (privilege escalation).
//   - SET PASSWORD: covered separately by IsPasswordChangeQuery for ALTER USER,
//     but the SET PASSWORD syntax also exists.
var mysqlBlockedPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bLOAD\s+DATA\s+(?:LOCAL\s+)?INFILE\b`),
	regexp.MustCompile(`(?i)\bINTO\s+(?:OUT|DUMP)FILE\b`),
	regexp.MustCompile(`(?i)\bSET\s+GLOBAL\b`),
	regexp.MustCompile(`(?i)\bSET\s+PASSWORD\b`),
}

// leadingBlockCommentRe matches a single leading block comment (/* ... */),
// including one that spans multiple lines. leadingLineCommentRe matches a single
// leading line comment (-- ... up to end of line).
var (
	leadingBlockCommentRe = regexp.MustCompile(`(?s)^\s*/\*.*?\*/`)
	leadingLineCommentRe  = regexp.MustCompile(`^\s*--[^\n]*`)
)

// stripLeadingComments removes any stacked leading SQL comments (block or line)
// and surrounding whitespace so a query like "/* x */ ALTER ROLE ..." is
// classified by its real leading keyword rather than bypassing every prefix
// check. Returns the remaining SQL, trimmed.
func stripLeadingComments(sql string) string {
	prev := ""
	out := strings.TrimSpace(sql)

	for out != prev {
		prev = out
		out = leadingBlockCommentRe.ReplaceAllString(out, "")
		out = leadingLineCommentRe.ReplaceAllString(out, "")
		out = strings.TrimSpace(out)
	}

	return out
}

// classifyStatements splits a possibly multi-statement SQL string into its
// individual statements for prefix/pattern classification. Leading comments are
// stripped from the whole string first (so a "; " inside a leading comment does
// not create bogus fragments) and again from each fragment (to catch comments
// between statements). Empty fragments are dropped.
//
// This is deliberately a naive semicolon split — it does not parse string
// literals — which matches the proxy's existing prefix-based classification.
// Its purpose is to stop a trailing "; ALTER ROLE ..." from hiding behind a
// leading "SELECT 1" in the PostgreSQL simple query protocol.
func classifyStatements(sql string) []string {
	parts := strings.Split(stripLeadingComments(sql), ";")
	out := make([]string, 0, len(parts))

	for _, part := range parts {
		if stmt := stripLeadingComments(part); stmt != "" {
			out = append(out, stmt)
		}
	}

	return out
}

// statementHasPrefix reports whether any statement in sql begins with one of the
// given (upper-case) keywords once leading comments are stripped.
func statementHasPrefix(sql string, keywords ...string) bool {
	for _, stmt := range classifyStatements(sql) {
		upper := strings.ToUpper(stmt)
		for _, keyword := range keywords {
			if strings.HasPrefix(upper, keyword) {
				return true
			}
		}
	}

	return false
}

// IsWriteQuery checks if a query is a write operation.
func IsWriteQuery(sql string) bool {
	return statementHasPrefix(sql, writeKeywords...)
}

// IsDDLQuery checks if a query is a DDL operation.
func IsDDLQuery(sql string) bool {
	return statementHasPrefix(sql, ddlKeywords...)
}

// IsPasswordChangeQuery checks if a query attempts to modify user/role passwords.
func IsPasswordChangeQuery(sql string) bool {
	for _, stmt := range classifyStatements(sql) {
		upper := strings.ToUpper(stmt)
		if (strings.HasPrefix(upper, "ALTER USER") || strings.HasPrefix(upper, "ALTER ROLE")) &&
			strings.Contains(upper, "PASSWORD") {
			return true
		}
	}

	return false
}

// ValidateQuery checks SQL against grant controls. Used by both PG and Oracle proxies.
func ValidateQuery(sql string, grant *store.Grant) error {
	if IsPasswordChangeQuery(sql) {
		return ErrPasswordChangeBlocked
	}

	if grant.IsReadOnly() && IsWriteQuery(sql) {
		return ErrReadOnlyViolation
	}

	if grant.ShouldBlockDDL() && IsDDLQuery(sql) {
		return ErrDDLBlocked
	}

	return nil
}

// ValidateOracleQuery runs shared validation plus Oracle-specific blocked patterns.
func ValidateOracleQuery(sql string, grant *store.Grant) error {
	if err := ValidateQuery(sql, grant); err != nil {
		return err
	}

	for _, pattern := range oracleBlockedPatterns {
		if pattern.MatchString(sql) {
			return ErrOraclePatternBlocked
		}
	}

	return nil
}

// ValidateMySQLQuery runs shared validation plus MySQL-specific blocked patterns.
func ValidateMySQLQuery(sql string, grant *store.Grant) error {
	if err := ValidateQuery(sql, grant); err != nil {
		return err
	}

	for _, pattern := range mysqlBlockedPatterns {
		if pattern.MatchString(sql) {
			return ErrMySQLPatternBlocked
		}
	}

	return nil
}
