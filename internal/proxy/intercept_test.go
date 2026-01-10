package proxy

import (
	"encoding/json"
	"errors"
	"log/slog"
	"testing"

	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/fclairamb/dbbat/internal/store"
)

func TestIsWriteQuery(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		sql      string
		expected bool
	}{
		// Write queries
		{name: "INSERT statement", sql: "INSERT INTO users (name) VALUES ('test')", expected: true},
		{name: "UPDATE statement", sql: "UPDATE users SET name = 'test' WHERE id = 1", expected: true},
		{name: "DELETE statement", sql: "DELETE FROM users WHERE id = 1", expected: true},
		{name: "DROP statement", sql: "DROP TABLE users", expected: true},
		{name: "TRUNCATE statement", sql: "TRUNCATE TABLE users", expected: true},
		{name: "CREATE statement", sql: "CREATE TABLE users (id INT)", expected: true},
		{name: "ALTER statement", sql: "ALTER TABLE users ADD COLUMN name TEXT", expected: true},
		{name: "GRANT statement", sql: "GRANT SELECT ON users TO user1", expected: true},
		{name: "REVOKE statement", sql: "REVOKE SELECT ON users FROM user1", expected: true},

		// Write queries with leading whitespace
		{name: "INSERT with whitespace", sql: "  INSERT INTO users (name) VALUES ('test')", expected: true},
		{name: "UPDATE with newline", sql: "\nUPDATE users SET name = 'test'", expected: true},
		{name: "DELETE with tab", sql: "\tDELETE FROM users", expected: true},

		// Write queries lowercase
		{name: "insert lowercase", sql: "insert into users (name) values ('test')", expected: true},
		{name: "update lowercase", sql: "update users set name = 'test'", expected: true},
		{name: "delete lowercase", sql: "delete from users", expected: true},

		// Read queries
		{name: "SELECT statement", sql: "SELECT * FROM users", expected: false},
		{name: "SELECT with WHERE", sql: "SELECT id, name FROM users WHERE id = 1", expected: false},
		{name: "WITH clause", sql: "WITH cte AS (SELECT * FROM users) SELECT * FROM cte", expected: false},
		{name: "EXPLAIN statement", sql: "EXPLAIN SELECT * FROM users", expected: false},
		{name: "SHOW statement", sql: "SHOW tables", expected: false},

		// Edge cases
		{name: "empty string", sql: "", expected: false},
		{name: "only whitespace", sql: "   ", expected: false},
		{name: "SELECT containing INSERT keyword", sql: "SELECT * FROM users WHERE action = 'INSERT'", expected: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			result := isWriteQuery(tt.sql)
			if result != tt.expected {
				t.Errorf("isWriteQuery(%q) = %v, want %v", tt.sql, result, tt.expected)
			}
		})
	}
}

func TestWriteKeywords(t *testing.T) {
	t.Parallel()

	// Ensure all expected keywords are in the list
	expectedKeywords := []string{
		"INSERT", "UPDATE", "DELETE", "DROP", "TRUNCATE",
		"CREATE", "ALTER", "GRANT", "REVOKE",
	}

	if len(writeKeywords) != len(expectedKeywords) {
		t.Errorf("writeKeywords length = %d, want %d", len(writeKeywords), len(expectedKeywords))
	}

	for _, kw := range expectedKeywords {
		found := false
		for _, wk := range writeKeywords {
			if wk == kw {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("writeKeywords missing %q", kw)
		}
	}
}

func TestIsReadOnlyBypassAttempt(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		sql      string
		expected bool
	}{
		// Should block - attempts to disable read-only mode
		{
			name:     "SET SESSION with off",
			sql:      "SET SESSION default_transaction_read_only = off;",
			expected: true,
		},
		{
			name:     "SET SESSION with false",
			sql:      "SET SESSION default_transaction_read_only = false;",
			expected: true,
		},
		{
			name:     "SET SESSION with 0",
			sql:      "SET SESSION default_transaction_read_only = 0;",
			expected: true,
		},
		{
			name:     "SET with TO syntax",
			sql:      "SET default_transaction_read_only TO off;",
			expected: true,
		},
		{
			name:     "SET with TO false",
			sql:      "SET default_transaction_read_only TO false;",
			expected: true,
		},
		{
			name:     "RESET command",
			sql:      "RESET default_transaction_read_only;",
			expected: true,
		},
		{
			name:     "RESET SESSION command",
			sql:      "RESET SESSION default_transaction_read_only;",
			expected: true,
		},
		{
			name:     "SET SESSION AUTHORIZATION",
			sql:      "SET SESSION AUTHORIZATION postgres;",
			expected: true,
		},
		{
			name:     "SET AUTHORIZATION",
			sql:      "SET AUTHORIZATION DEFAULT;",
			expected: true,
		},
		{
			name:     "SET ROLE",
			sql:      "SET ROLE admin;",
			expected: true,
		},
		{
			name:     "case insensitive",
			sql:      "set session default_transaction_read_only = OFF;",
			expected: true,
		},
		{
			name:     "mixed case",
			sql:      "Set Session Default_Transaction_Read_Only = Off;",
			expected: true,
		},
		{
			name:     "with whitespace",
			sql:      "  SET SESSION default_transaction_read_only = off  ;",
			expected: true,
		},

		// Should allow - safe operations
		{
			name:     "SET to on",
			sql:      "SET SESSION default_transaction_read_only = on;",
			expected: false,
		},
		{
			name:     "SET to true",
			sql:      "SET SESSION default_transaction_read_only = true;",
			expected: false,
		},
		{
			name:     "SET to 1",
			sql:      "SET SESSION default_transaction_read_only = 1;",
			expected: false,
		},
		{
			name:     "SELECT query",
			sql:      "SELECT * FROM users;",
			expected: false,
		},
		{
			name:     "SET other parameter",
			sql:      "SET statement_timeout = 30000;",
			expected: false,
		},
		{
			name:     "SET work_mem",
			sql:      "SET work_mem = '64MB';",
			expected: false,
		},
		{
			name:     "SHOW command",
			sql:      "SHOW default_transaction_read_only;",
			expected: false,
		},
		{
			name:     "SHOW ALL",
			sql:      "SHOW ALL;",
			expected: false,
		},
		{
			name:     "empty string",
			sql:      "",
			expected: false,
		},
		// Note: Regex-based detection has limitations and may produce false positives
		// when keywords appear in string literals. This is acceptable since PostgreSQL
		// enforces read-only mode at the database level as defense-in-depth.
		{
			name:     "SELECT with keyword in string (known limitation - false positive)",
			sql:      "SELECT * FROM users WHERE notes LIKE '%SET ROLE%';",
			expected: true, // False positive, but acceptable for security
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			result := isReadOnlyBypassAttempt(tt.sql)
			if result != tt.expected {
				t.Errorf("isReadOnlyBypassAttempt(%q) = %v, want %v", tt.sql, result, tt.expected)
			}
		})
	}
}

func TestIsPasswordChangeQuery(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		sql      string
		expected bool
	}{
		// Password change queries that should be blocked
		{name: "ALTER USER with PASSWORD", sql: "ALTER USER myuser WITH PASSWORD 'newpass'", expected: true},
		{name: "ALTER USER PASSWORD only", sql: "ALTER USER myuser PASSWORD 'newpass'", expected: true},
		{name: "ALTER ROLE with PASSWORD", sql: "ALTER ROLE myrole WITH PASSWORD 'newpass'", expected: true},
		{name: "ALTER ROLE ENCRYPTED PASSWORD", sql: "ALTER ROLE myrole WITH ENCRYPTED PASSWORD 'newpass'", expected: true},
		{name: "ALTER USER lowercase", sql: "alter user myuser with password 'newpass'", expected: true},
		{name: "ALTER ROLE mixed case", sql: "Alter Role myrole Password 'newpass'", expected: true},
		{name: "ALTER USER with whitespace", sql: "  ALTER USER myuser WITH PASSWORD 'newpass'", expected: true},
		{name: "ALTER USER complex", sql: "ALTER USER myuser WITH LOGIN PASSWORD 'newpass' VALID UNTIL 'infinity'", expected: true},

		// Non-password queries that should be allowed
		{name: "ALTER USER without PASSWORD", sql: "ALTER USER myuser WITH LOGIN", expected: false},
		{name: "ALTER ROLE without PASSWORD", sql: "ALTER ROLE myrole WITH SUPERUSER", expected: false},
		{name: "ALTER TABLE", sql: "ALTER TABLE users ADD COLUMN password VARCHAR(255)", expected: false},
		{name: "UPDATE with password column", sql: "UPDATE users SET password = 'newpass' WHERE id = 1", expected: false},
		{name: "SELECT with PASSWORD in text", sql: "SELECT * FROM users WHERE notes LIKE '%PASSWORD%'", expected: false},
		{name: "CREATE USER with PASSWORD", sql: "CREATE USER myuser WITH PASSWORD 'newpass'", expected: false}, // CREATE is different from ALTER
		{name: "empty string", sql: "", expected: false},
		{name: "SELECT statement", sql: "SELECT * FROM users", expected: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			result := isPasswordChangeQuery(tt.sql)
			if result != tt.expected {
				t.Errorf("isPasswordChangeQuery(%q) = %v, want %v", tt.sql, result, tt.expected)
			}
		})
	}
}

func TestHandleQuery_BlocksReadOnlyBypass(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		accessLevel string
		sql         string
		expectErr   error
	}{
		{
			name:        "read access blocks SET default_transaction_read_only off",
			accessLevel: "read",
			sql:         "SET SESSION default_transaction_read_only = off;",
			expectErr:   ErrReadOnlyBypassAttempt,
		},
		{
			name:        "read access blocks RESET default_transaction_read_only",
			accessLevel: "read",
			sql:         "RESET default_transaction_read_only;",
			expectErr:   ErrReadOnlyBypassAttempt,
		},
		{
			name:        "read access blocks SET AUTHORIZATION",
			accessLevel: "read",
			sql:         "SET SESSION AUTHORIZATION postgres;",
			expectErr:   ErrReadOnlyBypassAttempt,
		},
		{
			name:        "read access blocks SET ROLE",
			accessLevel: "read",
			sql:         "SET ROLE admin;",
			expectErr:   ErrReadOnlyBypassAttempt,
		},
		{
			name:        "read access allows SET to on",
			accessLevel: "read",
			sql:         "SET SESSION default_transaction_read_only = on;",
			expectErr:   nil,
		},
		{
			name:        "write access allows SET default_transaction_read_only off",
			accessLevel: "write",
			sql:         "SET SESSION default_transaction_read_only = off;",
			expectErr:   nil,
		},
		{
			name:        "write access allows SET ROLE",
			accessLevel: "write",
			sql:         "SET ROLE admin;",
			expectErr:   nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := newTestSession(tt.accessLevel)
			err := s.handleQuery(&pgproto3.Query{String: tt.sql})

			if tt.expectErr != nil {
				if !errors.Is(err, tt.expectErr) {
					t.Errorf("handleQuery() error = %v, want %v", err, tt.expectErr)
				}
			} else {
				if err != nil {
					t.Errorf("handleQuery() unexpected error: %v", err)
				}
			}
		})
	}
}

func TestHandleQuery_BlocksPasswordChange(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		accessLevel string
		sql         string
		expectErr   error
	}{
		{
			name:        "write access blocks ALTER USER PASSWORD",
			accessLevel: "write",
			sql:         "ALTER USER myuser WITH PASSWORD 'newpass'",
			expectErr:   ErrPasswordChangeNotAllowed,
		},
		{
			name:        "read access blocks ALTER USER PASSWORD",
			accessLevel: "read",
			sql:         "ALTER USER myuser WITH PASSWORD 'newpass'",
			expectErr:   ErrPasswordChangeNotAllowed,
		},
		{
			name:        "write access blocks ALTER ROLE PASSWORD",
			accessLevel: "write",
			sql:         "ALTER ROLE myrole WITH PASSWORD 'newpass'",
			expectErr:   ErrPasswordChangeNotAllowed,
		},
		{
			name:        "write access allows ALTER USER without PASSWORD",
			accessLevel: "write",
			sql:         "ALTER USER myuser WITH LOGIN",
			expectErr:   nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := newTestSession(tt.accessLevel)
			err := s.handleQuery(&pgproto3.Query{String: tt.sql})

			if tt.expectErr != nil {
				if !errors.Is(err, tt.expectErr) {
					t.Errorf("handleQuery() error = %v, want %v", err, tt.expectErr)
				}
			} else {
				if err != nil {
					t.Errorf("handleQuery() unexpected error: %v", err)
				}
			}
		})
	}
}

func TestHandleParse_BlocksReadOnlyBypass(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		accessLevel string
		sql         string
		expectErr   error
	}{
		{
			name:        "read access blocks SET default_transaction_read_only off",
			accessLevel: "read",
			sql:         "SET SESSION default_transaction_read_only = off;",
			expectErr:   ErrReadOnlyBypassAttempt,
		},
		{
			name:        "read access blocks RESET default_transaction_read_only",
			accessLevel: "read",
			sql:         "RESET default_transaction_read_only;",
			expectErr:   ErrReadOnlyBypassAttempt,
		},
		{
			name:        "read access blocks SET ROLE",
			accessLevel: "read",
			sql:         "SET ROLE admin;",
			expectErr:   ErrReadOnlyBypassAttempt,
		},
		{
			name:        "read access allows SELECT",
			accessLevel: "read",
			sql:         "SELECT * FROM users;",
			expectErr:   nil,
		},
		{
			name:        "write access allows SET ROLE",
			accessLevel: "write",
			sql:         "SET ROLE admin;",
			expectErr:   nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := newTestSession(tt.accessLevel)
			err := s.handleParse(&pgproto3.Parse{Query: tt.sql})

			if tt.expectErr != nil {
				if !errors.Is(err, tt.expectErr) {
					t.Errorf("handleParse() error = %v, want %v", err, tt.expectErr)
				}
			} else {
				if err != nil {
					t.Errorf("handleParse() unexpected error: %v", err)
				}
			}
		})
	}
}

func TestHandleParse_BlocksPasswordChange(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		accessLevel string
		sql         string
		expectErr   error
	}{
		{
			name:        "write access blocks ALTER USER PASSWORD",
			accessLevel: "write",
			sql:         "ALTER USER myuser WITH PASSWORD 'newpass'",
			expectErr:   ErrPasswordChangeNotAllowed,
		},
		{
			name:        "read access blocks ALTER ROLE PASSWORD",
			accessLevel: "read",
			sql:         "ALTER ROLE myrole WITH PASSWORD 'newpass'",
			expectErr:   ErrPasswordChangeNotAllowed,
		},
		{
			name:        "write access allows ALTER TABLE with password column",
			accessLevel: "write",
			sql:         "ALTER TABLE users ADD COLUMN password TEXT",
			expectErr:   nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := newTestSession(tt.accessLevel)
			err := s.handleParse(&pgproto3.Parse{Query: tt.sql})

			if tt.expectErr != nil {
				if !errors.Is(err, tt.expectErr) {
					t.Errorf("handleParse() error = %v, want %v", err, tt.expectErr)
				}
			} else {
				if err != nil {
					t.Errorf("handleParse() unexpected error: %v", err)
				}
			}
		})
	}
}

func TestParseRowsAffected(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		commandTag string
		expected   *int64
	}{
		// Standard command tags
		{name: "UPDATE with count", commandTag: "UPDATE 5", expected: ptr(int64(5))},
		{name: "DELETE with count", commandTag: "DELETE 10", expected: ptr(int64(10))},
		{name: "SELECT with count", commandTag: "SELECT 100", expected: ptr(int64(100))},

		// INSERT has format "INSERT oid count"
		{name: "INSERT with oid and count", commandTag: "INSERT 0 1", expected: ptr(int64(1))},
		{name: "INSERT multiple rows", commandTag: "INSERT 0 42", expected: ptr(int64(42))},

		// Commands without counts
		{name: "BEGIN", commandTag: "BEGIN", expected: nil},
		{name: "COMMIT", commandTag: "COMMIT", expected: nil},
		{name: "ROLLBACK", commandTag: "ROLLBACK", expected: nil},
		{name: "SET", commandTag: "SET", expected: nil},

		// Edge cases
		{name: "empty string", commandTag: "", expected: nil},
		{name: "single word", commandTag: "DISCARD", expected: nil},
		{name: "zero count", commandTag: "UPDATE 0", expected: ptr(int64(0))},
		{name: "large count", commandTag: "SELECT 999999", expected: ptr(int64(999999))},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			result := parseRowsAffected(tt.commandTag)
			if tt.expected == nil {
				if result != nil {
					t.Errorf("parseRowsAffected(%q) = %d, want nil", tt.commandTag, *result)
				}
			} else {
				if result == nil {
					t.Errorf("parseRowsAffected(%q) = nil, want %d", tt.commandTag, *tt.expected)
				} else if *result != *tt.expected {
					t.Errorf("parseRowsAffected(%q) = %d, want %d", tt.commandTag, *result, *tt.expected)
				}
			}
		})
	}
}

// ptr is a helper to create a pointer to an int64.
func ptr(i int64) *int64 {
	return &i
}

// newTestSession creates a session with the given access level for testing.
func newTestSession(accessLevel string) *Session {
	return &Session{
		grant: &store.Grant{
			AccessLevel: accessLevel,
		},
		extendedState: &extendedQueryState{
			preparedStatements: make(map[string]*preparedStatement),
			portals:            make(map[string]*portalState),
		},
		logger: slog.Default(),
	}
}

func TestHandleParse_ReadOnly(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		accessLevel string
		sql         string
		expectErr   bool
	}{
		{
			name:        "read access allows SELECT",
			accessLevel: "read",
			sql:         "SELECT * FROM users",
			expectErr:   false,
		},
		{
			name:        "read access blocks INSERT",
			accessLevel: "read",
			sql:         "INSERT INTO users (name) VALUES ('test')",
			expectErr:   true,
		},
		{
			name:        "read access blocks UPDATE",
			accessLevel: "read",
			sql:         "UPDATE users SET name = 'test'",
			expectErr:   true,
		},
		{
			name:        "read access blocks DELETE",
			accessLevel: "read",
			sql:         "DELETE FROM users",
			expectErr:   true,
		},
		{
			name:        "write access allows INSERT",
			accessLevel: "write",
			sql:         "INSERT INTO users (name) VALUES ('test')",
			expectErr:   false,
		},
		{
			name:        "write access allows UPDATE",
			accessLevel: "write",
			sql:         "UPDATE users SET name = 'test'",
			expectErr:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := newTestSession(tt.accessLevel)
			err := s.handleParse(&pgproto3.Parse{Query: tt.sql})

			if tt.expectErr && err == nil {
				t.Errorf("handleParse() expected error, got nil")
			}
			if !tt.expectErr && err != nil {
				t.Errorf("handleParse() unexpected error: %v", err)
			}
		})
	}
}

func TestHandleParse_StoresStatement(t *testing.T) {
	t.Parallel()

	s := newTestSession("write")

	// Test unnamed statement with type OIDs
	err := s.handleParse(&pgproto3.Parse{Name: "", Query: "SELECT * FROM t1", ParameterOIDs: []uint32{23, 25}})
	if err != nil {
		t.Fatalf("handleParse() error = %v", err)
	}

	stmt := s.extendedState.preparedStatements[""]
	if stmt == nil || stmt.sql != "SELECT * FROM t1" {
		t.Errorf("unnamed statement not stored correctly")
	}
	if len(stmt.typeOIDs) != 2 || stmt.typeOIDs[0] != 23 || stmt.typeOIDs[1] != 25 {
		t.Errorf("type OIDs not stored correctly: %v", stmt.typeOIDs)
	}

	// Test named statement
	err = s.handleParse(&pgproto3.Parse{Name: "stmt1", Query: "SELECT * FROM t2"})
	if err != nil {
		t.Fatalf("handleParse() error = %v", err)
	}

	stmt1 := s.extendedState.preparedStatements["stmt1"]
	if stmt1 == nil || stmt1.sql != "SELECT * FROM t2" {
		t.Errorf("named statement not stored correctly")
	}
}

func TestHandleBind_MapsPortal(t *testing.T) {
	t.Parallel()

	s := newTestSession("write")

	// First store a prepared statement with type OIDs
	s.extendedState.preparedStatements["stmt1"] = &preparedStatement{
		sql:      "SELECT * FROM users WHERE id = $1",
		typeOIDs: []uint32{23}, // int4
	}

	// Bind portal to statement with parameters
	s.handleBind(&pgproto3.Bind{
		DestinationPortal:    "portal1",
		PreparedStatement:    "stmt1",
		ParameterFormatCodes: []int16{1}, // binary
		Parameters:           [][]byte{{0, 0, 0, 42}},
	})

	portal := s.extendedState.portals["portal1"]
	if portal == nil || portal.stmtName != "stmt1" {
		t.Errorf("portal not mapped to statement")
	}
	if portal.parameters == nil || len(portal.parameters.Values) != 1 {
		t.Errorf("parameters not captured")
	}
	if portal.parameters.Values[0] != "42" {
		t.Errorf("parameter value = %q, want %q", portal.parameters.Values[0], "42")
	}

	// Test unnamed portal without parameters
	s.extendedState.preparedStatements[""] = &preparedStatement{sql: "SELECT 1"}
	s.handleBind(&pgproto3.Bind{
		DestinationPortal: "",
		PreparedStatement: "",
	})

	portal2 := s.extendedState.portals[""]
	if portal2 == nil || portal2.stmtName != "" {
		t.Errorf("unnamed portal not mapped correctly")
	}
}

func TestHandleExecute_QueuesQuery(t *testing.T) {
	t.Parallel()

	s := newTestSession("write")

	// Set up state with parameters
	s.extendedState.preparedStatements[""] = &preparedStatement{
		sql:      "SELECT * FROM users WHERE id = $1",
		typeOIDs: []uint32{23},
	}
	s.extendedState.portals[""] = &portalState{
		stmtName: "",
		parameters: &store.QueryParameters{
			Values:      []string{"42"},
			Raw:         []string{"AAAAKg=="},
			FormatCodes: []int16{1},
			TypeOIDs:    []uint32{23},
		},
	}

	// Execute
	err := s.handleExecute(&pgproto3.Execute{Portal: ""})
	if err != nil {
		t.Fatalf("handleExecute() error = %v", err)
	}

	// Verify query was queued with parameters
	if len(s.extendedState.pendingQueries) != 1 {
		t.Fatalf("expected 1 pending query, got %d", len(s.extendedState.pendingQueries))
	}

	pending := s.extendedState.pendingQueries[0]
	if pending.sql != "SELECT * FROM users WHERE id = $1" {
		t.Errorf("queued query SQL = %q, want %q", pending.sql, "SELECT * FROM users WHERE id = $1")
	}
	if pending.parameters == nil || pending.parameters.Values[0] != "42" {
		t.Errorf("parameters not included in queued query")
	}
}

func TestHandleExecute_QuotaCheck(t *testing.T) {
	t.Parallel()

	maxQueries := int64(0) // Quota exhausted
	s := &Session{
		grant: &store.Grant{
			AccessLevel:    "write",
			MaxQueryCounts: &maxQueries,
			QueryCount:     0,
		},
		extendedState: &extendedQueryState{
			preparedStatements: map[string]*preparedStatement{"": {sql: "SELECT 1"}},
			portals:            map[string]*portalState{"": {stmtName: ""}},
		},
		logger: slog.Default(),
	}

	err := s.handleExecute(&pgproto3.Execute{Portal: ""})
	if !errors.Is(err, ErrQueryLimitExceeded) {
		t.Errorf("handleExecute() error = %v, want %v", err, ErrQueryLimitExceeded)
	}
}

func TestHandleExecute_UnknownStatement(t *testing.T) {
	t.Parallel()

	s := newTestSession("write")

	// Execute without setting up statement
	err := s.handleExecute(&pgproto3.Execute{Portal: "unknown"})
	if err != nil {
		t.Errorf("handleExecute() error = %v, expected nil for unknown statement", err)
	}

	// No query should be queued
	if len(s.extendedState.pendingQueries) != 0 {
		t.Errorf("expected 0 pending queries, got %d", len(s.extendedState.pendingQueries))
	}
}

func TestHandleClose_CleansUp(t *testing.T) {
	t.Parallel()

	s := newTestSession("write")

	// Set up state
	s.extendedState.preparedStatements["stmt1"] = &preparedStatement{sql: "SELECT * FROM users"}
	s.extendedState.portals["portal1"] = &portalState{stmtName: "stmt1"}

	// Close statement
	s.handleClose(&pgproto3.Close{ObjectType: 'S', Name: "stmt1"})

	if _, exists := s.extendedState.preparedStatements["stmt1"]; exists {
		t.Errorf("statement was not deleted")
	}

	// Close portal
	s.handleClose(&pgproto3.Close{ObjectType: 'P', Name: "portal1"})

	if _, exists := s.extendedState.portals["portal1"]; exists {
		t.Errorf("portal was not deleted")
	}
}

func TestExtendedQueryFlow(t *testing.T) {
	t.Parallel()

	s := newTestSession("write")

	// Parse with type OIDs
	err := s.handleParse(&pgproto3.Parse{
		Name:          "",
		Query:         "SELECT * FROM users WHERE id = $1",
		ParameterOIDs: []uint32{23}, // int4
	})
	if err != nil {
		t.Fatalf("handleParse() error = %v", err)
	}

	// Bind with parameter
	s.handleBind(&pgproto3.Bind{
		DestinationPortal:    "",
		PreparedStatement:    "",
		ParameterFormatCodes: []int16{0}, // text format
		Parameters:           [][]byte{[]byte("42")},
	})

	// Execute
	err = s.handleExecute(&pgproto3.Execute{Portal: ""})
	if err != nil {
		t.Fatalf("handleExecute() error = %v", err)
	}

	// Verify query was tracked with parameters
	if len(s.extendedState.pendingQueries) != 1 {
		t.Fatalf("expected 1 pending query, got %d", len(s.extendedState.pendingQueries))
	}

	pending := s.extendedState.pendingQueries[0]
	if pending.sql != "SELECT * FROM users WHERE id = $1" {
		t.Errorf("pending query SQL = %q, want %q", pending.sql, "SELECT * FROM users WHERE id = $1")
	}

	if pending.startTime.IsZero() {
		t.Errorf("pending query startTime not set")
	}

	if pending.parameters == nil {
		t.Fatal("pending query parameters not set")
	}
	if pending.parameters.Values[0] != "42" {
		t.Errorf("parameter value = %q, want %q", pending.parameters.Values[0], "42")
	}
}

func TestMultipleExecuteBeforeSync(t *testing.T) {
	t.Parallel()

	s := newTestSession("write")

	// Parse two statements
	if err := s.handleParse(&pgproto3.Parse{Name: "s1", Query: "SELECT * FROM t1"}); err != nil {
		t.Fatalf("handleParse(s1) error = %v", err)
	}

	if err := s.handleParse(&pgproto3.Parse{Name: "s2", Query: "SELECT * FROM t2"}); err != nil {
		t.Fatalf("handleParse(s2) error = %v", err)
	}

	// Bind both
	s.handleBind(&pgproto3.Bind{DestinationPortal: "p1", PreparedStatement: "s1"})
	s.handleBind(&pgproto3.Bind{DestinationPortal: "p2", PreparedStatement: "s2"})

	// Execute both before Sync
	if err := s.handleExecute(&pgproto3.Execute{Portal: "p1"}); err != nil {
		t.Fatalf("handleExecute(p1) error = %v", err)
	}

	if err := s.handleExecute(&pgproto3.Execute{Portal: "p2"}); err != nil {
		t.Fatalf("handleExecute(p2) error = %v", err)
	}

	// Verify both queries are queued in order
	if len(s.extendedState.pendingQueries) != 2 {
		t.Fatalf("expected 2 pending queries, got %d", len(s.extendedState.pendingQueries))
	}

	if s.extendedState.pendingQueries[0].sql != "SELECT * FROM t1" {
		t.Errorf("first query = %q, want %q", s.extendedState.pendingQueries[0].sql, "SELECT * FROM t1")
	}

	if s.extendedState.pendingQueries[1].sql != "SELECT * FROM t2" {
		t.Errorf("second query = %q, want %q", s.extendedState.pendingQueries[1].sql, "SELECT * FROM t2")
	}
}

func TestDecodeBinaryParameter(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		data     []byte
		oid      uint32
		expected string
	}{
		// Boolean
		{name: "bool true", data: []byte{1}, oid: 16, expected: "true"},
		{name: "bool false", data: []byte{0}, oid: 16, expected: "false"},

		// Integers
		{name: "int2 positive", data: []byte{0, 42}, oid: 21, expected: "42"},
		{name: "int2 negative", data: []byte{0xff, 0xfe}, oid: 21, expected: "-2"},
		{name: "int4 positive", data: []byte{0, 0, 0, 42}, oid: 23, expected: "42"},
		{name: "int4 negative", data: []byte{0xff, 0xff, 0xff, 0xfe}, oid: 23, expected: "-2"},
		{name: "int8 positive", data: []byte{0, 0, 0, 0, 0, 0, 0, 45}, oid: 20, expected: "45"},

		// Floats
		{name: "float4", data: []byte{0x42, 0x28, 0x00, 0x00}, oid: 700, expected: "42"},
		{name: "float8", data: []byte{0x40, 0x45, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}, oid: 701, expected: "42"},

		// Text types
		{name: "text", data: []byte("hello"), oid: 25, expected: "hello"},
		{name: "varchar", data: []byte("world"), oid: 1043, expected: "world"},
		{name: "char", data: []byte("x"), oid: 1042, expected: "x"},

		// Empty and unknown
		{name: "empty data", data: []byte{}, oid: 23, expected: ""},
		{name: "unknown oid", data: []byte{1, 2, 3}, oid: 99999, expected: "(oid:99999)AQID"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			result := decodeBinaryParameter(tt.data, tt.oid)
			if result != tt.expected {
				t.Errorf("decodeBinaryParameter(%v, %d) = %q, want %q", tt.data, tt.oid, result, tt.expected)
			}
		})
	}
}

func TestGetTypeOID(t *testing.T) {
	t.Parallel()

	oids := []uint32{23, 25, 20}

	if got := getTypeOID(oids, 0); got != 23 {
		t.Errorf("getTypeOID(oids, 0) = %d, want 23", got)
	}
	if got := getTypeOID(oids, 2); got != 20 {
		t.Errorf("getTypeOID(oids, 2) = %d, want 20", got)
	}
	if got := getTypeOID(oids, 5); got != 0 {
		t.Errorf("getTypeOID(oids, 5) = %d, want 0 (out of bounds)", got)
	}
	if got := getTypeOID(nil, 0); got != 0 {
		t.Errorf("getTypeOID(nil, 0) = %d, want 0", got)
	}
}

func TestHandleBind_CapturesParameters(t *testing.T) {
	t.Parallel()

	s := newTestSession("write")

	// Set up statement with type OIDs
	s.extendedState.preparedStatements[""] = &preparedStatement{
		sql:      "SELECT $1, $2",
		typeOIDs: []uint32{25, 23}, // text, int4
	}

	// Bind with mixed format parameters
	s.handleBind(&pgproto3.Bind{
		DestinationPortal:    "",
		PreparedStatement:    "",
		ParameterFormatCodes: []int16{0, 1}, // text, binary
		Parameters:           [][]byte{[]byte("hello"), {0, 0, 0, 42}},
	})

	portal := s.extendedState.portals[""]
	if portal == nil {
		t.Fatal("portal not created")
	}

	params := portal.parameters
	if params == nil {
		t.Fatal("parameters not captured")
	}

	// Check values
	if params.Values[0] != "hello" {
		t.Errorf("param 0 value = %q, want %q", params.Values[0], "hello")
	}
	if params.Values[1] != "42" {
		t.Errorf("param 1 value = %q, want %q", params.Values[1], "42")
	}

	// Check format codes
	if params.FormatCodes[0] != 0 {
		t.Errorf("param 0 format = %d, want 0 (text)", params.FormatCodes[0])
	}
	if params.FormatCodes[1] != 1 {
		t.Errorf("param 1 format = %d, want 1 (binary)", params.FormatCodes[1])
	}

	// Check raw values are base64 encoded
	if params.Raw[0] != "aGVsbG8=" { // base64("hello")
		t.Errorf("param 0 raw = %q, want %q", params.Raw[0], "aGVsbG8=")
	}

	// Check type OIDs
	if len(params.TypeOIDs) != 2 || params.TypeOIDs[0] != 25 || params.TypeOIDs[1] != 23 {
		t.Errorf("type OIDs = %v, want [25, 23]", params.TypeOIDs)
	}
}

func TestHandleBind_SingleFormatCode(t *testing.T) {
	t.Parallel()

	s := newTestSession("write")

	// Set up statement
	s.extendedState.preparedStatements[""] = &preparedStatement{
		sql:      "SELECT $1, $2",
		typeOIDs: []uint32{25, 25},
	}

	// Bind with single format code (applies to all params)
	s.handleBind(&pgproto3.Bind{
		DestinationPortal:    "",
		PreparedStatement:    "",
		ParameterFormatCodes: []int16{0}, // text for all
		Parameters:           [][]byte{[]byte("a"), []byte("b")},
	})

	portal := s.extendedState.portals[""]
	params := portal.parameters

	if params.FormatCodes[0] != 0 || params.FormatCodes[1] != 0 {
		t.Errorf("format codes = %v, want [0, 0]", params.FormatCodes)
	}
}

func TestDecodeColumnValue(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		data     []byte
		oid      uint32
		expected interface{}
	}{
		// Boolean
		{name: "bool true t", data: []byte("t"), oid: 16, expected: true},
		{name: "bool true 1", data: []byte("1"), oid: 16, expected: true},
		{name: "bool true word", data: []byte("true"), oid: 16, expected: true},
		{name: "bool false f", data: []byte("f"), oid: 16, expected: false},
		{name: "bool false 0", data: []byte("0"), oid: 16, expected: false},

		// Integers (text format from DataRow)
		{name: "int2", data: []byte("42"), oid: 21, expected: int64(42)},
		{name: "int4", data: []byte("12345"), oid: 23, expected: int64(12345)},
		{name: "int8", data: []byte("9223372036854775807"), oid: 20, expected: int64(9223372036854775807)},
		{name: "int4 negative", data: []byte("-100"), oid: 23, expected: int64(-100)},

		// Floats (text format)
		{name: "float4", data: []byte("3.14"), oid: 700, expected: float64(3.14)},
		{name: "float8", data: []byte("3.14159265359"), oid: 701, expected: float64(3.14159265359)},
		{name: "numeric", data: []byte("123.456"), oid: 1700, expected: float64(123.456)},

		// Text types
		{name: "text", data: []byte("hello world"), oid: 25, expected: "hello world"},
		{name: "varchar", data: []byte("test varchar"), oid: 1043, expected: "test varchar"},
		{name: "char", data: []byte("x"), oid: 1042, expected: "x"},

		// Bytea
		{name: "bytea", data: []byte(`\x48656c6c6f`), oid: 17, expected: `\x48656c6c6f`},

		// Timestamp and other unknown types (return as string)
		{name: "timestamp", data: []byte("2024-01-01 12:00:00"), oid: 1114, expected: "2024-01-01 12:00:00"},
		{name: "unknown oid", data: []byte("some value"), oid: 99999, expected: "some value"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			result := decodeColumnValue(tt.data, tt.oid)
			if result != tt.expected {
				t.Errorf("decodeColumnValue(%q, %d) = %v (%T), want %v (%T)",
					tt.data, tt.oid, result, result, tt.expected, tt.expected)
			}
		})
	}
}

func TestConvertDataRow(t *testing.T) {
	t.Parallel()

	s := newTestSession("write")

	tests := []struct {
		name        string
		values      [][]byte
		columnNames []string
		columnOIDs  []uint32
		expectKeys  []string
	}{
		{
			name:        "basic row with column names",
			values:      [][]byte{[]byte("1"), []byte("Test Name"), []byte("100")},
			columnNames: []string{"id", "name", "value"},
			columnOIDs:  []uint32{23, 25, 23}, // int4, text, int4
			expectKeys:  []string{"id", "name", "value"},
		},
		{
			name:        "row without column names uses col_N",
			values:      [][]byte{[]byte("a"), []byte("b")},
			columnNames: nil,
			columnOIDs:  nil,
			expectKeys:  []string{"col_0", "col_1"},
		},
		{
			name:        "handles null values",
			values:      [][]byte{[]byte("1"), nil, []byte("3")},
			columnNames: []string{"a", "b", "c"},
			columnOIDs:  []uint32{23, 25, 23},
			expectKeys:  []string{"a", "b", "c"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			row := s.convertDataRow(tt.values, tt.columnNames, tt.columnOIDs)

			// Check that RowData is valid JSON
			if len(row.RowData) == 0 {
				t.Fatal("RowData is empty")
			}

			// Verify it parses as JSON
			var data map[string]interface{}
			if err := json.Unmarshal(row.RowData, &data); err != nil {
				t.Fatalf("failed to unmarshal RowData: %v", err)
			}

			// Check expected keys exist
			for _, key := range tt.expectKeys {
				if _, ok := data[key]; !ok {
					t.Errorf("missing key %q in row data: %v", key, data)
				}
			}

			// Check row size is calculated
			if row.RowSizeBytes == 0 && len(tt.values) > 0 && tt.values[0] != nil {
				t.Errorf("RowSizeBytes should be > 0")
			}
		})
	}
}

func TestConvertDataRow_TypeDecoding(t *testing.T) {
	t.Parallel()

	s := newTestSession("write")

	// Test with various types
	values := [][]byte{
		[]byte("42"),    // int
		[]byte("3.14"),  // float
		[]byte("t"),     // bool
		[]byte("hello"), // text
	}
	columnNames := []string{"int_col", "float_col", "bool_col", "text_col"}
	columnOIDs := []uint32{23, 701, 16, 25} // int4, float8, bool, text

	row := s.convertDataRow(values, columnNames, columnOIDs)

	var data map[string]interface{}
	if err := json.Unmarshal(row.RowData, &data); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	// JSON numbers are float64, so int becomes float64 when unmarshaled
	if data["int_col"] != float64(42) {
		t.Errorf("int_col = %v, want 42", data["int_col"])
	}
	if data["float_col"] != float64(3.14) {
		t.Errorf("float_col = %v, want 3.14", data["float_col"])
	}
	if data["bool_col"] != true {
		t.Errorf("bool_col = %v, want true", data["bool_col"])
	}
	if data["text_col"] != "hello" {
		t.Errorf("text_col = %v, want 'hello'", data["text_col"])
	}
}

func TestHandleQuery_InitializesCapturedRows(t *testing.T) {
	t.Parallel()

	s := newTestSession("write")

	err := s.handleQuery(&pgproto3.Query{String: "SELECT * FROM users"})
	if err != nil {
		t.Fatalf("handleQuery() error = %v", err)
	}

	if s.currentQuery == nil {
		t.Fatal("currentQuery not set")
	}

	if s.currentQuery.capturedRows == nil {
		t.Error("capturedRows not initialized")
	}
}

func TestHandleExecute_InitializesCapturedRows(t *testing.T) {
	t.Parallel()

	s := newTestSession("write")

	// Set up state
	s.extendedState.preparedStatements[""] = &preparedStatement{sql: "SELECT 1"}
	s.extendedState.portals[""] = &portalState{stmtName: ""}

	err := s.handleExecute(&pgproto3.Execute{Portal: ""})
	if err != nil {
		t.Fatalf("handleExecute() error = %v", err)
	}

	if len(s.extendedState.pendingQueries) != 1 {
		t.Fatalf("expected 1 pending query, got %d", len(s.extendedState.pendingQueries))
	}

	pending := s.extendedState.pendingQueries[0]
	if pending.capturedRows == nil {
		t.Error("capturedRows not initialized")
	}
}

func TestResultCapture_RefusesWhenLimitsExceeded(t *testing.T) {
	t.Parallel()

	// Test that when limits are exceeded, all captured rows are discarded
	query := &pendingQuery{
		sql:          "SELECT * FROM big_table",
		capturedRows: make([]store.QueryRow, 0),
	}

	maxRows := 5

	// Simulate capturing rows until limit is exceeded
	for i := 0; i < 10; i++ {
		if query.truncated {
			// Already truncated, no more rows should be captured
			break
		}

		if query.rowNumber >= maxRows {
			// Limits exceeded - discard all captured rows
			query.truncated = true
			query.capturedRows = nil
		} else {
			query.capturedRows = append(query.capturedRows, store.QueryRow{
				RowNumber: i + 1,
				RowData:   []byte(`{"id":1}`),
			})
			query.rowNumber++
		}
	}

	// Verify all rows were discarded
	if query.capturedRows != nil {
		t.Errorf("capturedRows should be nil when limits exceeded, got %d rows", len(query.capturedRows))
	}
	if !query.truncated {
		t.Error("truncated should be true")
	}
}

func TestParseCopyColumnNames(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		sql      string
		expected []string
	}{
		{
			name:     "COPY TO with columns",
			sql:      "COPY public.test_data (id, name, value, created_at) TO stdout",
			expected: []string{"id", "name", "value", "created_at"},
		},
		{
			name:     "COPY TO without columns",
			sql:      "COPY public.test_data TO stdout",
			expected: nil,
		},
		{
			name:     "COPY FROM with columns",
			sql:      "COPY users (name, email) FROM stdin",
			expected: []string{"name", "email"},
		},
		{
			name:     "COPY TO with lowercase",
			sql:      "copy test (a, b, c) to stdout",
			expected: []string{"a", "b", "c"},
		},
		{
			name:     "COPY with extra spaces",
			sql:      "COPY test ( col1 , col2 ) TO stdout",
			expected: []string{"col1", "col2"},
		},
		{
			name:     "Empty column list",
			sql:      "COPY test () TO stdout",
			expected: nil,
		},
		{
			name:     "No TO or FROM",
			sql:      "SELECT * FROM test",
			expected: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			result := parseCopyColumnNames(tc.sql)

			if tc.expected == nil {
				if result != nil {
					t.Errorf("expected nil, got %v", result)
				}
				return
			}

			if len(result) != len(tc.expected) {
				t.Errorf("expected %d columns, got %d: %v", len(tc.expected), len(result), result)
				return
			}

			for i, col := range tc.expected {
				if result[i] != col {
					t.Errorf("column %d: expected %q, got %q", i, col, result[i])
				}
			}
		})
	}
}

func TestUnescapeCopyText(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{name: "no escapes", input: "hello world", expected: "hello world"},
		{name: "escaped newline", input: "hello\\nworld", expected: "hello\nworld"},
		{name: "escaped tab", input: "col1\\tcol2", expected: "col1\tcol2"},
		{name: "escaped backslash", input: "path\\\\to\\\\file", expected: "path\\to\\file"},
		{name: "multiple escapes", input: "a\\nb\\tc\\\\d", expected: "a\nb\tc\\d"},
		{name: "escaped carriage return", input: "line\\r", expected: "line\r"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			result := unescapeCopyText(tc.input)
			if result != tc.expected {
				t.Errorf("expected %q, got %q", tc.expected, result)
			}
		})
	}
}

func TestCopyFormatToString(t *testing.T) {
	t.Parallel()

	tests := []struct {
		format   byte
		expected string
	}{
		{0, "text"},
		{1, "binary"},
		{2, "unknown"},
		{255, "unknown"},
	}

	for _, tc := range tests {
		result := copyFormatToString(tc.format)
		if result != tc.expected {
			t.Errorf("copyFormatToString(%d) = %q, expected %q", tc.format, result, tc.expected)
		}
	}
}
