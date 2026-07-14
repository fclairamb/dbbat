package shared

import (
	"errors"
	"regexp"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"

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

// Mongo-specific validation errors (contract §7 surfaces these as the errmsg
// of an Unauthorized (13) reply).
var (
	ErrMongoReadOnly        = errors.New("dbbat: grant is read-only")
	ErrMongoDDLBlocked      = errors.New("dbbat: grant blocks DDL operations")
	ErrMongoCommandBlocked  = errors.New("dbbat: command not permitted through dbbat")
	ErrMongoUnknownCommand  = errors.New("dbbat: command not on the proxy allowlist")
	ErrMongoDatabaseBlocked = errors.New("dbbat: access to this database is not permitted")
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

// mongoCmdClass classifies a MongoDB command by the enforcement it needs.
type mongoCmdClass int

const (
	classUnknown mongoCmdClass = iota
	classRead
	classWrite
	classDDL
	classDiagnostic
	classBlocked
	classListDatabases
)

// Read commands (contract §2 command classification). aggregate is
// re-classified as a write when its pipeline contains $out or $merge.
var mongoReadCommands = map[string]bool{
	"find": true, "aggregate": true, "count": true, "distinct": true,
	"getMore": true, "killCursors": true, "listCollections": true,
	"listIndexes": true, "explain": true, "dbStats": true, "collStats": true,
}

// Write commands.
var mongoWriteCommands = map[string]bool{
	"insert": true, "update": true, "delete": true,
	"findAndModify": true, "findandmodify": true, "bulkWrite": true,
}

// DDL commands.
var mongoDDLCommands = map[string]bool{
	"create": true, "drop": true, "dropDatabase": true, "createIndexes": true,
	"dropIndexes": true, "collMod": true, "renameCollection": true, "convertToCapped": true,
}

// Diagnostics — always allowed post-auth. Includes the set a real mongosh
// session emits on connect.
var mongoDiagnosticCommands = map[string]bool{
	"hello": true, "isMaster": true, "ismaster": true, "ping": true,
	"buildInfo": true, "buildinfo": true, "whatsmyuri": true,
	"connectionStatus": true, "getParameter": true, "getLog": true,
	"hostInfo": true, "atlasVersion": true, "endSessions": true,
	"saslStart": true, "saslContinue": true,
}

// Always-blocked commands (user/role management, cluster admin, RCE surface).
var mongoBlockedCommands = map[string]bool{
	"createUser": true, "updateUser": true, "dropUser": true,
	"dropAllUsersFromDatabase": true, "grantRolesToUser": true,
	"revokeRolesFromUser": true, "createRole": true, "updateRole": true,
	"dropRole": true, "shutdown": true, "replSetReconfig": true,
	"replSetStepDown": true, "setParameter": true,
	"setFeatureCompatibilityVersion": true, "eval": true, "fsync": true,
	"compact": true,
}

// classifyMongoCommand maps a command name (+ body, for aggregate) to a class.
func classifyMongoCommand(cmd string, body bson.Raw) mongoCmdClass {
	switch {
	case mongoBlockedCommands[cmd]:
		return classBlocked
	case cmd == "listDatabases":
		return classListDatabases
	case mongoDiagnosticCommands[cmd]:
		return classDiagnostic
	case mongoDDLCommands[cmd]:
		return classDDL
	case mongoWriteCommands[cmd]:
		return classWrite
	case mongoReadCommands[cmd]:
		if cmd == "aggregate" && aggregatePipelineWrites(body) {
			return classWrite
		}

		return classRead
	default:
		return classUnknown
	}
}

// aggregatePipelineWrites reports whether an aggregate command's pipeline ends
// in a $out or $merge stage (which writes and must be grant-checked as a write).
func aggregatePipelineWrites(body bson.Raw) bool {
	pipeline, ok := body.Lookup("pipeline").ArrayOK()
	if !ok {
		return false
	}

	stages, err := pipeline.Values()
	if err != nil {
		return false
	}

	for _, stageVal := range stages {
		stage, ok := stageVal.DocumentOK()
		if !ok {
			continue
		}

		elems, err := stage.Elements()
		if err != nil {
			continue
		}

		for _, e := range elems {
			if e.Key() == "$out" || e.Key() == "$merge" {
				return true
			}
		}
	}

	return false
}

// ValidateMongoCommand enforces grant controls and the $db policy on a MongoDB
// command (contract §2). It operates on the command name and the kind-0 body.
// db is the session's resolved target database; grant carries the controls.
func ValidateMongoCommand(cmd, dbName string, body bson.Raw, db *store.Database, grant *store.Grant) error {
	class := classifyMongoCommand(cmd, body)

	switch class {
	case classBlocked:
		return ErrMongoCommandBlocked
	case classListDatabases:
		// Cluster-wide disclosure — denied by default.
		return ErrMongoDatabaseBlocked
	case classUnknown:
		// Default-deny so the allowlist is extended from real logs, never
		// silently punching a hole in read_only.
		return ErrMongoUnknownCommand
	case classDDL:
		if grant.IsReadOnly() {
			return ErrMongoReadOnly
		}

		if grant.ShouldBlockDDL() {
			return ErrMongoDDLBlocked
		}
	case classWrite:
		if grant.IsReadOnly() {
			return ErrMongoReadOnly
		}
	case classRead, classDiagnostic:
		// allowed; fall through to the $db check
	}

	if !mongoDatabaseAllowed(dbName, class, db) {
		return ErrMongoDatabaseBlocked
	}

	return nil
}

// mongoDatabaseAllowed enforces the $db policy (contract §2): allow the
// configured database; allow admin only for diagnostics; deny local/config and
// anything else.
func mongoDatabaseAllowed(dbName string, class mongoCmdClass, db *store.Database) bool {
	switch dbName {
	case "":
		return true
	case "admin":
		return class == classDiagnostic
	case "$external":
		return false
	case "local", "config":
		return false
	}

	if db == nil {
		return false
	}

	return dbName == db.DatabaseName || dbName == db.Name
}
