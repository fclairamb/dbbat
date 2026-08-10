package mcp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fclairamb/dbbat/internal/store"
)

// Tool result statuses. Every non-final status names the next action, which is
// the whole contract with the agent: a held query must never look like a hang
// or a failure.
const (
	// StatusOK means the statement ran and the rows (if any) are here.
	StatusOK = "ok"
	// StatusApprovalPending means the statement is parked on a human. The
	// result carries the execution id to poll and the query uid the approver
	// is looking at.
	StatusApprovalPending = "approval_pending"
	// StatusStillRunning means the statement has not returned yet and no hold
	// was observed — a slow query, not a gated one. Same next action.
	StatusStillRunning = "still_running"
)

// ErrSQLRequired is returned for an empty statement.
var ErrSQLRequired = errors.New("sql is required")

// registerTools installs the tool surface for one caller.
//
// The surface is deliberately small. Every tool an agent can call is another
// thing an operator has to reason about, and everything below can be expressed
// as "the things a psql session through dbbat could do".
func (s *Server) registerTools(srv *mcpsdk.Server, caller *Caller) {
	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:  "list_databases",
		Title: "List databases you can reach",
		Description: "List the databases the API key's owner currently holds an access grant on, " +
			"with the grant's expiry, controls (read-only, DDL/COPY blocks) and quota usage. " +
			"Call this before query: the name it returns is what query and describe expect.",
		Annotations: &mcpsdk.ToolAnnotations{ReadOnlyHint: true},
	}, s.toolListDatabases(caller))

	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:  "query",
		Title: "Run a SQL statement",
		Description: "Run one SQL statement against a granted database. The statement goes through " +
			"dbbat exactly as a psql or mysql client would: the grant's read-only/DDL controls, " +
			"quotas, query logging and approval holds all apply. Rows are capped server-side. " +
			"If the statement is suspended awaiting human approval the result has status " +
			"'approval_pending' and an execution_id — call await_approval with it rather than retrying.",
	}, s.toolQuery(caller))

	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:  "describe",
		Title: "Describe tables and columns",
		Description: "List the tables of a granted database, or the columns of one table. " +
			"Introspection runs through the same governed path as query.",
		Annotations: &mcpsdk.ToolAnnotations{ReadOnlyHint: true},
	}, s.toolDescribe(caller))

	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:  "await_approval",
		Title: "Wait for a suspended statement",
		Description: "Wait for a statement that query left in 'approval_pending' (or 'still_running'). " +
			"Blocks for up to timeout_seconds and then answers: either the final result, or the same " +
			"pending status again so you can call back. It never fails silently — keep calling until " +
			"the status is no longer pending.",
		Annotations: &mcpsdk.ToolAnnotations{ReadOnlyHint: true},
	}, s.toolAwaitApproval(caller))
}

// -- list_databases ---------------------------------------------------------

// ListDatabasesInput takes no arguments: the caller's grants are the scope.
type ListDatabasesInput struct{}

// DatabaseInfo is one database the caller can reach, with everything an agent
// needs to plan: when access ends, what it may not do, and how much of its
// quota is left.
type DatabaseInfo struct {
	Name             string `json:"name" jsonschema:"the name to pass to query and describe"`
	Description      string `json:"description,omitempty"`
	Protocol         string `json:"protocol"`
	Supported        bool   `json:"supported" jsonschema:"false when this dbbat version cannot run statements against this protocol yet"`
	GrantExpiresAt   string `json:"grant_expires_at" jsonschema:"RFC3339 instant after which the grant stops working mid-session"`
	ReadOnly         bool   `json:"read_only"`
	BlockDDL         bool   `json:"block_ddl"`
	BlockCopy        bool   `json:"block_copy"`
	ApprovalPatterns int    `json:"approval_patterns" jsonschema:"how many statement patterns on this grant suspend a matching statement until a human approves it"`
	MaxQueries       *int64 `json:"max_queries,omitempty"`
	QueriesUsed      int64  `json:"queries_used"`
	MaxBytes         *int64 `json:"max_bytes_transferred,omitempty"`
	BytesUsed        int64  `json:"bytes_transferred"`
}

// ListDatabasesOutput is the tool result.
type ListDatabasesOutput struct {
	Databases []DatabaseInfo `json:"databases"`
}

func (s *Server) toolListDatabases(caller *Caller) mcpsdk.ToolHandlerFor[ListDatabasesInput, ListDatabasesOutput] {
	return func(ctx context.Context, _ *mcpsdk.CallToolRequest, _ ListDatabasesInput) (
		*mcpsdk.CallToolResult, ListDatabasesOutput, error,
	) {
		list, err := s.listAccessible(ctx, caller)
		if err != nil {
			return nil, ListDatabasesOutput{}, err
		}

		out := ListDatabasesOutput{Databases: make([]DatabaseInfo, 0, len(list))}

		for _, a := range list {
			out.Databases = append(out.Databases, DatabaseInfo{
				Name:             a.server.Name,
				Description:      a.server.Description,
				Protocol:         a.server.Protocol,
				Supported:        SupportedProtocol(a.server.Protocol),
				GrantExpiresAt:   a.grant.ExpiresAt.UTC().Format(time.RFC3339),
				ReadOnly:         a.grant.IsReadOnly(),
				BlockDDL:         a.grant.ShouldBlockDDL(),
				BlockCopy:        a.grant.ShouldBlockCopy(),
				ApprovalPatterns: len(a.grant.ApprovalPatterns()),
				MaxQueries:       a.grant.MaxQueryCounts(),
				QueriesUsed:      a.grant.QueryCount,
				MaxBytes:         a.grant.MaxBytesTransferred(),
				BytesUsed:        a.grant.BytesTransferred,
			})
		}

		return nil, out, nil
	}
}

// -- query ------------------------------------------------------------------

// QueryInput is the `query` tool's arguments.
type QueryInput struct {
	Database string `json:"database" jsonschema:"database name, as returned by list_databases"`
	SQL      string `json:"sql" jsonschema:"one SQL statement. It is sent to the database untouched - dbbat never rewrites it"`
	Params   []any  `json:"params,omitempty" jsonschema:"bind parameters for the statement's placeholders ($1.. on PostgreSQL, ? on MySQL). Use these for values rather than string-concatenating them into sql"`
	MaxRows  int    `json:"max_rows,omitempty" jsonschema:"maximum rows to return. Clamped by the server; asking for more than the server cap simply returns the cap with truncated=true"`
}

// QueryOutput is the `query` (and `await_approval`) result.
type QueryOutput struct {
	Status          string           `json:"status" jsonschema:"ok, approval_pending or still_running. Anything but ok means the statement has not finished and you must call await_approval"`
	Database        string           `json:"database"`
	Columns         []string         `json:"columns,omitempty"`
	Rows            []map[string]any `json:"rows,omitempty" jsonschema:"result rows keyed by column name, in result order"`
	RowCount        int              `json:"row_count"`
	Truncated       bool             `json:"truncated" jsonschema:"true when the statement produced more rows than max_rows"`
	MaxRows         int              `json:"max_rows" jsonschema:"the cap actually applied, after server-side clamping"`
	RowsAffected    *int64           `json:"rows_affected,omitempty"`
	DurationMs      int64            `json:"duration_ms"`
	ExecutionID     string           `json:"execution_id,omitempty" jsonschema:"pass this to await_approval while the status is not ok"`
	QueryUID        string           `json:"query_uid,omitempty" jsonschema:"the dbbat query a human is being asked to approve, as shown in the dbbat UI and Slack"`
	ApprovalPattern string           `json:"approval_pattern,omitempty" jsonschema:"the grant pattern that matched and caused the hold"`
	Message         string           `json:"message,omitempty"`
}

func (s *Server) toolQuery(caller *Caller) mcpsdk.ToolHandlerFor[QueryInput, QueryOutput] {
	return func(ctx context.Context, _ *mcpsdk.CallToolRequest, in QueryInput) (
		*mcpsdk.CallToolResult, QueryOutput, error,
	) {
		sqlText := strings.TrimSpace(in.SQL)
		if sqlText == "" {
			return nil, QueryOutput{}, ErrSQLRequired
		}

		target, err := s.resolve(ctx, caller, in.Database)
		if err != nil {
			return nil, QueryOutput{}, err
		}

		if !SupportedProtocol(target.server.Protocol) {
			return nil, QueryOutput{}, fmt.Errorf("%w: %s (database %q)",
				ErrProtocolUnsupported, target.server.Protocol, target.server.Name)
		}

		maxRows := clampMaxRows(in.MaxRows)

		e := s.start(ctx, caller, target, sqlText, in.Params, maxRows)

		out, err := renderExecution(e, maxRows)
		if err != nil {
			return nil, QueryOutput{}, err
		}

		return nil, *out, nil
	}
}

// -- await_approval ---------------------------------------------------------

// AwaitApprovalInput identifies the execution to wait on.
type AwaitApprovalInput struct {
	ExecutionID    string `json:"execution_id" jsonschema:"the execution_id returned by query or describe when the status was not ok"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty" jsonschema:"how long to block before answering. Clamped by the server; the call always answers, so a pending status simply means call again"`
}

func (s *Server) toolAwaitApproval(caller *Caller) mcpsdk.ToolHandlerFor[AwaitApprovalInput, QueryOutput] {
	return func(ctx context.Context, _ *mcpsdk.CallToolRequest, in AwaitApprovalInput) (
		*mcpsdk.CallToolResult, QueryOutput, error,
	) {
		if strings.TrimSpace(in.ExecutionID) == "" {
			return nil, QueryOutput{}, ErrUnknownExecution
		}

		e, err := s.execs.get(in.ExecutionID, caller.User.UID)
		if err != nil {
			// Deliberately explicit rather than a guess: dbbat runs several
			// replicas and an execution lives on the one that started it, so
			// "unknown here" is a real possibility the agent has to be told
			// about instead of being handed a fabricated outcome.
			return nil, QueryOutput{}, fmt.Errorf("%w %q: it was started by another dbbat replica, "+
				"or it has already been collected. Look the statement up in dbbat "+
				"(GET /api/v1/queries) rather than re-running it — a retry parks a second connection",
				ErrUnknownExecution, in.ExecutionID)
		}

		s.wait(ctx, e, s.awaitWindow(in.TimeoutSeconds))

		out, err := renderExecution(e, 0)
		if err != nil {
			return nil, QueryOutput{}, err
		}

		return nil, *out, nil
	}
}

// awaitWindow clamps the agent's requested poll duration.
func (s *Server) awaitWindow(requested int) time.Duration {
	if requested <= 0 {
		return s.deps.AwaitWindow
	}

	d := time.Duration(requested) * time.Second
	if d > MaxAwaitWindow {
		return MaxAwaitWindow
	}

	return d
}

// renderExecution turns an execution's state into a tool result.
//
// A statement that failed — including one a human denied — comes back as a Go
// error, so the SDK marks the tool result as an error and the agent sees the
// database's own message (`query denied by alice: not during the freeze`)
// rather than a status code it has to interpret.
func renderExecution(e *execution, maxRows int) (*QueryOutput, error) {
	out := &QueryOutput{
		Database:   e.database,
		MaxRows:    maxRows,
		DurationMs: time.Since(e.startedAt).Milliseconds(),
	}

	if !e.finished() {
		info := e.hold()

		out.ExecutionID = e.id
		out.QueryUID = info.QueryUID
		out.ApprovalPattern = info.Pattern

		if info.Held {
			out.Status = StatusApprovalPending
			out.Message = "The statement is suspended awaiting human approval and is not running. " +
				"Call await_approval with this execution_id until the status changes. " +
				"Do not re-run the statement: a retry parks a second connection on the same approver."

			return out, nil
		}

		out.Status = StatusStillRunning
		out.Message = "The statement has not finished yet and no approval hold was observed. " +
			"Call await_approval with this execution_id."

		return out, nil
	}

	if e.err != nil {
		return nil, e.err
	}

	out.Status = StatusOK
	out.DurationMs = e.durationMs()

	if e.result != nil {
		out.Columns = e.result.Columns
		out.Rows = e.result.Rows
		out.RowCount = len(e.result.Rows)
		out.Truncated = e.result.Truncated
		out.RowsAffected = e.result.RowsAffected
	}

	return out, nil
}

// -- describe ---------------------------------------------------------------

// DescribeInput selects what to introspect.
type DescribeInput struct {
	Database string `json:"database" jsonschema:"database name, as returned by list_databases"`
	Table    string `json:"table,omitempty" jsonschema:"table to describe. Omit to list the database's tables instead"`
	Schema   string `json:"schema,omitempty" jsonschema:"PostgreSQL schema to disambiguate a table name present in several schemas"`
}

// TableInfo is one table in a describe listing.
type TableInfo struct {
	Schema string `json:"schema,omitempty"`
	Name   string `json:"name"`
}

// ColumnInfo is one column of a described table.
type ColumnInfo struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Nullable bool   `json:"nullable"`
	Default  string `json:"default,omitempty"`
}

// DescribeOutput is the describe result. It carries the same status/execution
// fields as QueryOutput because introspection runs through the governed path
// too, and a grant whose approval patterns match `SELECT` will hold it.
type DescribeOutput struct {
	Status      string       `json:"status"`
	Database    string       `json:"database"`
	Protocol    string       `json:"protocol"`
	Tables      []TableInfo  `json:"tables,omitempty"`
	Table       string       `json:"table,omitempty"`
	Columns     []ColumnInfo `json:"columns,omitempty"`
	Truncated   bool         `json:"truncated" jsonschema:"true when the database has more tables or columns than one call returns"`
	ExecutionID string       `json:"execution_id,omitempty"`
	QueryUID    string       `json:"query_uid,omitempty"`
	Message     string       `json:"message,omitempty"`
}

func (s *Server) toolDescribe(caller *Caller) mcpsdk.ToolHandlerFor[DescribeInput, DescribeOutput] {
	return func(ctx context.Context, _ *mcpsdk.CallToolRequest, in DescribeInput) (
		*mcpsdk.CallToolResult, DescribeOutput, error,
	) {
		target, err := s.resolve(ctx, caller, in.Database)
		if err != nil {
			return nil, DescribeOutput{}, err
		}

		sqlText, params, err := introspectionSQL(target.server.Protocol, in.Table, in.Schema)
		if err != nil {
			return nil, DescribeOutput{}, err
		}

		e := s.start(ctx, caller, target, sqlText, params, HardMaxRows)

		out := DescribeOutput{
			Database: target.server.Name,
			Protocol: target.server.Protocol,
			Table:    in.Table,
		}

		if !e.finished() {
			info := e.hold()
			out.ExecutionID = e.id
			out.QueryUID = info.QueryUID
			out.Status = StatusStillRunning

			if info.Held {
				out.Status = StatusApprovalPending
			}

			out.Message = "Introspection has not finished; call await_approval with this execution_id."

			return nil, out, nil
		}

		if e.err != nil {
			return nil, DescribeOutput{}, e.err
		}

		out.Status = StatusOK
		out.Truncated = e.result != nil && e.result.Truncated

		if in.Table == "" {
			out.Tables = renderTables(e.result)
		} else {
			out.Columns = renderColumns(e.result)
		}

		return nil, out, nil
	}
}

// introspectionSQL builds the catalog query for a protocol.
//
// Table and schema names ride as bind parameters, never as interpolated text:
// the input comes from a model, and an introspection helper must not be the
// one place in dbbat that concatenates a name into SQL.
func introspectionSQL(protocol, table, schema string) (string, []any, error) {
	switch protocol {
	case store.ProtocolPostgreSQL:
		if table == "" {
			return `SELECT table_schema, table_name FROM information_schema.tables ` +
				`WHERE table_schema NOT IN ('pg_catalog', 'information_schema') ` +
				`ORDER BY table_schema, table_name`, nil, nil
		}

		if schema != "" {
			return `SELECT column_name, data_type, is_nullable, column_default ` +
				`FROM information_schema.columns WHERE table_name = $1 AND table_schema = $2 ` +
				`ORDER BY ordinal_position`, []any{table, schema}, nil
		}

		return `SELECT column_name, data_type, is_nullable, column_default ` +
			`FROM information_schema.columns WHERE table_name = $1 ` +
			`AND table_schema NOT IN ('pg_catalog', 'information_schema') ` +
			`ORDER BY table_schema, ordinal_position`, []any{table}, nil

	case store.ProtocolMySQL, store.ProtocolMariaDB:
		if table == "" {
			return `SELECT table_schema, table_name FROM information_schema.tables ` +
				`WHERE table_schema = DATABASE() ORDER BY table_name`, nil, nil
		}

		return `SELECT column_name, column_type, is_nullable, column_default ` +
			`FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = ? ` +
			`ORDER BY ordinal_position`, []any{table}, nil

	case store.ProtocolOracle:
		// Every column is aliased to a quoted lower-case name: Oracle folds
		// unquoted identifiers to upper case and reports them that way in the
		// result metadata, which the renderers below key on.
		//
		// UPPER() on the bound name is the same folding applied to the input, so
		// `describe employees` finds the EMPLOYEES an Oracle DDL created. It
		// stays a bind parameter — the folding is Oracle's, not string surgery.
		//
		// DATA_DEFAULT is deliberately not selected: it is a LONG, and a LONG in
		// a catalog query is exactly the kind of value that makes a describe
		// fail on a driver quirk rather than return columns.
		if table == "" {
			return `SELECT owner AS "table_schema", table_name AS "table_name" FROM all_tables ` +
				`WHERE owner = SYS_CONTEXT('USERENV', 'CURRENT_SCHEMA') ORDER BY table_name`, nil, nil
		}

		if schema != "" {
			return `SELECT column_name AS "column_name", data_type AS "data_type", ` +
				`DECODE(nullable, 'Y', 'YES', 'NO') AS "is_nullable" FROM all_tab_columns ` +
				`WHERE table_name = UPPER(:1) AND owner = UPPER(:2) ORDER BY column_id`, []any{table, schema}, nil
		}

		return `SELECT column_name AS "column_name", data_type AS "data_type", ` +
			`DECODE(nullable, 'Y', 'YES', 'NO') AS "is_nullable" FROM all_tab_columns ` +
			`WHERE table_name = UPPER(:1) AND owner = SYS_CONTEXT('USERENV', 'CURRENT_SCHEMA') ` +
			`ORDER BY column_id`, []any{table}, nil

	default:
		return "", nil, fmt.Errorf("%w: %s", ErrProtocolUnsupported, protocol)
	}
}

func renderTables(result *QueryResult) []TableInfo {
	if result == nil {
		return nil
	}

	out := make([]TableInfo, 0, len(result.Rows))

	for _, row := range result.Rows {
		out = append(out, TableInfo{
			Schema: rowString(row, "table_schema"),
			Name:   rowString(row, "table_name"),
		})
	}

	return out
}

func renderColumns(result *QueryResult) []ColumnInfo {
	if result == nil {
		return nil
	}

	out := make([]ColumnInfo, 0, len(result.Rows))

	for _, row := range result.Rows {
		out = append(out, ColumnInfo{
			Name:     rowString(row, "column_name"),
			Type:     firstString(row, "data_type", "column_type"),
			Nullable: strings.EqualFold(rowString(row, "is_nullable"), "YES"),
			Default:  rowString(row, "column_default"),
		})
	}

	return out
}

// rowString reads a catalog column as text. Catalog columns come back as
// strings on both protocols, but a NULL default is a real and common value, so
// anything unset is simply empty.
func rowString(row map[string]any, key string) string {
	switch v := row[key].(type) {
	case nil:
		return ""
	case string:
		return v
	case []byte:
		return string(v)
	default:
		return fmt.Sprint(v)
	}
}

func firstString(row map[string]any, keys ...string) string {
	for _, k := range keys {
		if v := rowString(row, k); v != "" {
			return v
		}
	}

	return ""
}
