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
)

// Write keywords that should be blocked for read-only grants.
var writeKeywords = []string{
	"INSERT", "UPDATE", "DELETE", "DROP", "TRUNCATE",
	"CREATE", "ALTER", "GRANT", "REVOKE", "MERGE",
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

// IsWriteQuery checks if a query is a write operation.
func IsWriteQuery(sql string) bool {
	upper := strings.ToUpper(strings.TrimSpace(sql))
	for _, keyword := range writeKeywords {
		if strings.HasPrefix(upper, keyword) {
			return true
		}
	}

	return false
}

// IsDDLQuery checks if a query is a DDL operation.
func IsDDLQuery(sql string) bool {
	upper := strings.ToUpper(strings.TrimSpace(sql))
	for _, keyword := range ddlKeywords {
		if strings.HasPrefix(upper, keyword) {
			return true
		}
	}

	return false
}

// IsPasswordChangeQuery checks if a query attempts to modify user/role passwords.
func IsPasswordChangeQuery(sql string) bool {
	upper := strings.ToUpper(strings.TrimSpace(sql))

	if (strings.HasPrefix(upper, "ALTER USER") || strings.HasPrefix(upper, "ALTER ROLE")) &&
		strings.Contains(upper, "PASSWORD") {
		return true
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
