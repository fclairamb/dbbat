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
	// `ALTER SESSION SET CONTAINER = <pdb>` switches the session's pluggable
	// database. A dbbat grant is scoped to one server row, and the audit trail,
	// the quotas and the approval holds are all written against that row — so
	// after a switch every `queries` row names the wrong database. Refusing it
	// only under read_only/block_ddl (which is what the leading ALTER already
	// did) left the default "full write" grant able to step outside the database
	// it was granted on, which is the same escape with a different label. It is
	// therefore blocked outright, like ALTER SYSTEM, whatever the grant says.
	//
	// `[^;]*?` rather than `SET\s+"?CONTAINER"?` because CONTAINER need not be
	// the first parameter: `ALTER SESSION SET CURRENT_SCHEMA=X CONTAINER=PDB2`
	// is the same switch and must match too. It stops at `;` so the scan stays
	// inside one statement. `\bCONTAINER\b\s*"?\s*=` covers the bare and the
	// double-quoted spelling without matching a name that merely ends in it
	// (`FOO_CONTAINER=` has no word boundary before CONTAINER).
	regexp.MustCompile(`(?i)\bALTER\s+SESSION\s+SET\b[^;]*?\bCONTAINER\b\s*"?\s*=`),
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
	// The ALTER SESSION carve-out, ahead of the keyword scan because ALTER is in
	// both keyword sets. See IsAllowedAlterSession.
	if IsAllowedAlterSession(sql) {
		return false
	}

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
	// Same carve-out as IsWriteQuery: the reclassification is of the statement,
	// so it has to hold for both controls or a block_ddl grant would still refuse
	// what a read_only grant now allows.
	if IsAllowedAlterSession(sql) {
		return false
	}

	upper := strings.ToUpper(strings.TrimSpace(sql))
	for _, keyword := range ddlKeywords {
		if strings.HasPrefix(upper, keyword) {
			return true
		}
	}

	return false
}

// alterSessionAllowedParams enumerates the ALTER SESSION parameters that a
// read_only or block_ddl grant does *not* refuse.
//
// Why there is a carve-out at all: `ALTER SESSION SET <parameter>` changes
// nothing durable — no data, no schema, no other session, and Oracle reverts it
// at disconnect — yet ALTER is in both writeKeywords and ddlKeywords, so both
// controls refused it. DBeaver sends five of them *while the connection is
// still being established* (measured over the recorded corpus by
// internal/proxy/oracle/sql_extraction_survey_test.go), which turned a
// read-only grant into a client that cannot connect at all rather than one that
// is refused on a real write.
//
// Why it is a list of parameter names and never a prefix match on
// `ALTER SESSION SET …`: CONTAINER. `ALTER SESSION SET CONTAINER = <pdb>`
// switches the session's pluggable database, and a dbbat grant is scoped to one
// database — so a prefix match would hand a read-only session a way to step
// outside the database its grant covers. CONTAINER is therefore deliberately
// absent below, as is every parameter nobody has justified: an unenumerated
// parameter keeps the pre-carve-out behavior and is refused. CONTAINER is now
// belt *and* braces: oracleBlockedPatterns refuses it outright, whatever the
// grant says, because "absent from this list" only means "refused by read_only
// and block_ddl" and a full-write grant has neither. `ALTER SYSTEM` is untouched
// by any of this — same outright block.
//
// `ALTER SESSION` is Oracle-only syntax. IsWriteQuery/IsDDLQuery are shared by
// all five proxies, but no PostgreSQL, MySQL, MongoDB or SQL Server statement
// begins with it, so no other dialect's classification moves.
var alterSessionAllowedParams = map[string]bool{
	// --- Measured in the corpus: DBeaver's connection setup. The floor. ---

	// Changes unqualified *name resolution* only. Oracle still evaluates
	// privileges as the connected user, and a dbbat grant is scoped to a
	// database rather than to a schema, so the session reaches nothing it could
	// not already reach.
	"CURRENT_SCHEMA": true,
	// Picks which optimizer version's behavior to emulate: plan selection,
	// nothing else, for this session only.
	"OPTIMIZER_FEATURES_ENABLE": true,
	// The three hidden optimizer parameters DBeaver sends. Listed explicitly
	// even though the _OPTIMIZER_ family rule in alterSessionParamAllowed also
	// admits them, so the measured floor survives that rule being narrowed.
	"_OPTIMIZER_COST_BASED_TRANSFORMATION": true,
	"_OPTIMIZER_PUSH_PRED_COST_BASED":      true,
	"_OPTIMIZER_SQU_BOTTOMUP":              true,

	// --- Adjacent session-scoped, non-durable settings real clients set. ---
	//
	// Every NLS_* entry below is a formatting/collation preference: it decides
	// how the session renders or compares values it is already allowed to read.
	// None of them selects a database, a schema or a privilege, and each is
	// enumerated individually rather than admitted by an NLS_ wildcard — the
	// list is the security boundary, so it stays a list.
	"NLS_LANGUAGE":            true, // message and day/month name language
	"NLS_TERRITORY":           true, // default date/number/currency conventions
	"NLS_DATE_FORMAT":         true, // DATE rendering
	"NLS_DATE_LANGUAGE":       true, // day/month names in DATE rendering
	"NLS_TIMESTAMP_FORMAT":    true, // TIMESTAMP rendering
	"NLS_TIMESTAMP_TZ_FORMAT": true, // TIMESTAMP WITH TIME ZONE rendering
	"NLS_NUMERIC_CHARACTERS":  true, // decimal separator / group separator
	"NLS_CURRENCY":            true, // local currency symbol
	"NLS_ISO_CURRENCY":        true, // ISO currency symbol
	"NLS_DUAL_CURRENCY":       true, // secondary currency symbol
	"NLS_CALENDAR":            true, // calendar system used to render dates
	"NLS_SORT":                true, // linguistic collation for ORDER BY
	"NLS_COMP":                true, // whether comparisons use NLS_SORT
	// Session time zone: shifts how TIMESTAMP WITH LOCAL TIME ZONE values are
	// rendered for this session. dbbat itself sets it on the upstream leg
	// (upstream_auth_client_wide.go), so refusing it from a client would refuse
	// what the proxy already does on the client's behalf.
	"TIME_ZONE": true,
}

// IsAllowedAlterSession reports whether sql is an `ALTER SESSION SET …` in
// which *every* parameter being set is allowed — see alterSessionAllowedParams
// for the list and for why it is a list.
//
// It fails closed on everything else, deliberately, because "false" here just
// means the statement keeps the classification it had before the carve-out
// existed:
//
//   - a statement that is not `ALTER SESSION SET` (`ALTER SESSION ENABLE …`,
//     `ALTER SESSION CLOSE DATABASE LINK …`, `ALTER SYSTEM …`);
//   - a statement setting one allowed parameter and one that is not — allowing
//     it partially is not an option, so `ALTER SESSION SET CURRENT_SCHEMA=X
//     CONTAINER=Y` is refused whole;
//   - anything the scanner cannot read to the end with confidence: an
//     unterminated quote, a missing `=`, a byte outside the value charset, a
//     second statement stapled on behind a `;`.
//
// The statement is still recorded either way. This is a classification change,
// not a visibility one: an allowed ALTER SESSION appears in /queries exactly
// like any other statement.
func IsAllowedAlterSession(sql string) bool {
	scan := &alterSessionScanner{s: strings.TrimSpace(sql)}

	for _, kw := range []string{"ALTER", "SESSION", "SET"} {
		if !scan.keyword(kw) {
			return false
		}
	}

	params := 0

	for {
		scan.spaces()

		if scan.eof() {
			break
		}

		name, ok := scan.name()
		if !ok {
			return false
		}

		scan.spaces()

		if !scan.equals() {
			return false
		}

		scan.spaces()

		if !scan.value() {
			return false
		}

		if !alterSessionParamAllowed(name) {
			return false
		}

		params++
	}

	// `ALTER SESSION SET` with nothing after it is not a statement we recognize.
	return params > 0
}

// alterSessionParamAllowed answers for one upper-cased parameter name.
func alterSessionParamAllowed(name string) bool {
	if alterSessionAllowedParams[name] {
		return true
	}

	// The one family rule, and the only one. Oracle's `_optimizer_*` hidden
	// parameters are a single closed namespace of cost-based-optimizer knobs:
	// every one of them influences plan choice for the current session and
	// nothing else — none writes data, none touches schema, none changes the
	// session's identity or its container, none outlives the disconnect. That is
	// what makes this family safe when a wildcard generally is not: the danger a
	// wildcard usually carries is admitting a parameter with a different *kind*
	// of effect (CONTAINER being exactly that), and the parameters that can move
	// a session out of its grant live in other namespaces, not this one.
	//
	// It is a family rather than three enumerated names because the run of
	// `_optimizer_*` hints a client sends is version-dependent — the three in the
	// corpus are one DBeaver release's worth — so pinning what one recording
	// captured would reintroduce the connect-time refusal on the next release.
	return strings.HasPrefix(name, "_OPTIMIZER_")
}

// alterSessionScanner is a byte scanner over a single ALTER SESSION statement.
// Hand-rolled rather than a regexp because the decision is per parameter: "every
// parameter must be on the list" is not something a match over the whole
// statement can express.
type alterSessionScanner struct {
	s string
	i int
}

func (p *alterSessionScanner) eof() bool { return p.i >= len(p.s) }

func (p *alterSessionScanner) spaces() {
	for p.i < len(p.s) && isSQLSpace(p.s[p.i]) {
		p.i++
	}
}

// keyword consumes leading whitespace then kw, case-insensitively and only at a
// word boundary — `ALTER SESSION SETTINGS` must not read as `SET`.
func (p *alterSessionScanner) keyword(kw string) bool {
	p.spaces()

	end := p.i + len(kw)
	if end > len(p.s) || !strings.EqualFold(p.s[p.i:end], kw) {
		return false
	}

	if end < len(p.s) && isBareNameByte(p.s[end]) {
		return false
	}

	p.i = end

	return true
}

func (p *alterSessionScanner) equals() bool {
	if p.eof() || p.s[p.i] != '=' {
		return false
	}

	p.i++

	return true
}

// name reads a parameter name, bare or double-quoted, and upper-cases it.
//
// Oracle treats a double-quoted identifier as case-sensitive, so folding is not
// strictly faithful — but it can only ever make a *differently cased spelling of
// a listed name* match, never a name that is not on the list, and such a
// spelling is an unknown-parameter error upstream rather than anything the
// session gets to do.
func (p *alterSessionScanner) name() (string, bool) {
	if p.eof() {
		return "", false
	}

	if p.s[p.i] == '"' {
		quoted, ok := p.quoted('"')

		return strings.ToUpper(quoted), ok
	}

	start := p.i
	for p.i < len(p.s) && isBareNameByte(p.s[p.i]) {
		p.i++
	}

	if p.i == start {
		return "", false
	}

	return strings.ToUpper(p.s[start:p.i]), true
}

// value consumes a parameter value: a single-quoted literal, a double-quoted
// identifier, or a bare token. Reports false when there is no value to read or
// a quote is left open.
func (p *alterSessionScanner) value() bool {
	if p.eof() {
		return false
	}

	if q := p.s[p.i]; q == '\'' || q == '"' {
		_, ok := p.quoted(q)

		return ok
	}

	start := p.i
	for p.i < len(p.s) && isBareValueByte(p.s[p.i]) {
		p.i++
	}

	return p.i > start
}

// quoted reads a q-delimited run starting at the opening quote. `”` inside a
// single-quoted literal is an escaped quote, not a terminator. An unterminated
// run reports false, which fails the whole statement closed.
func (p *alterSessionScanner) quoted(q byte) (string, bool) {
	p.i++
	start := p.i

	for p.i < len(p.s) {
		if p.s[p.i] != q {
			p.i++

			continue
		}

		if q == '\'' && p.i+1 < len(p.s) && p.s[p.i+1] == '\'' {
			p.i += 2

			continue
		}

		out := p.s[start:p.i]
		p.i++

		return out, true
	}

	return "", false
}

func isSQLSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\v' || c == '\f'
}

// isBareNameByte covers Oracle's unquoted identifier charset.
func isBareNameByte(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
		c == '_' || c == '$' || c == '#'
}

// isBareValueByte covers unquoted values (TESTADM, FALSE, 8, ALL_ROWS, +02:00).
// It deliberately excludes `;`, quotes, parentheses, commas and slashes, so a
// second statement stapled behind the first cannot be swallowed as a value —
// the scan hits a byte it does not accept and the statement fails closed.
func isBareValueByte(c byte) bool {
	return isBareNameByte(c) || c == '.' || c == '+' || c == '-' || c == ':'
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
func ValidateMongoCommand(cmd, dbName string, body bson.Raw, db *store.Server, grant *store.Grant) error {
	class := classifyMongoCommand(cmd, body)

	switch class {
	case classBlocked:
		return ErrMongoCommandBlocked
	case classListDatabases:
		// Allowed, but the MongoDB proxy filters the reply's databases array down
		// to the grant's target database (result.go), so no cluster-wide
		// disclosure leaks. Runs against admin, so skip the $db policy below.
		return nil
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
func mongoDatabaseAllowed(dbName string, class mongoCmdClass, db *store.Server) bool {
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
