package mcp

import (
	"context"
	"database/sql/driver"
	"fmt"
	"math"
	"net"
	"net/url"
	"strconv"

	"github.com/jackc/pgx/v5"
)

// mcpApplicationName is what the loopback session advertises. It shows up in
// the upstream application_name the proxy builds, so an agent's traffic is
// distinguishable from a human's in the target database's own views.
const mcpApplicationName = "dbbat-mcp"

// executePostgreSQL runs one statement against the PostgreSQL proxy listener.
//
// The connection is plaintext (sslmode=disable) on purpose: it never leaves
// the loopback interface, and the alternative — the proxy's auto-generated
// self-signed certificate — would only add a verification decision with no
// confidentiality gained.
func executePostgreSQL(ctx context.Context, addr string, req ExecRequest) (*QueryResult, error) {
	cfg, err := pgConnConfig(addr, req)
	if err != nil {
		return nil, err
	}

	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connecting to the dbbat PostgreSQL proxy: %w", err)
	}

	// The hold happens inside Query; closing on a canceled context is what
	// turns an abandoned MCP execution into the proxy's ordinary
	// client-disconnect path (hold -> abandoned, nothing forwarded).
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()

	rows, err := conn.Query(ctx, req.SQL, req.Params...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := &QueryResult{}

	fields := rows.FieldDescriptions()
	names := make([]string, len(fields))

	for i := range fields {
		names[i] = fields[i].Name
	}

	result.Columns = dedupeColumns(names)

	for rows.Next() {
		if len(result.Rows) >= req.MaxRows {
			result.Truncated = true

			break
		}

		values, valErr := rows.Values()
		if valErr != nil {
			return nil, valErr
		}

		row := make(map[string]any, len(result.Columns))
		for i, name := range result.Columns {
			if i < len(values) {
				row[name] = normalizePGValue(values[i])
			}
		}

		result.Rows = append(result.Rows, row)
	}

	// Truncating means we stop reading a live result set; the deferred Close
	// discards the rest, and rows.Err() would report the cancellation rather
	// than a real failure, so it is only meaningful on a complete read.
	if !result.Truncated {
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}

	rows.Close()

	// Only for statements where "rows affected" means something: on a SELECT
	// the tag repeats the row count, which the caller already reports.
	if tag := rows.CommandTag(); !tag.Select() {
		affected := tag.RowsAffected()
		result.RowsAffected = &affected
	}

	return result, nil
}

// pgConnConfig builds a fully explicit pgx config.
//
// pgx.ParseConfig applies libpq environment defaults (PGHOST, PGOPTIONS,
// PGSSLMODE, …). A dbbat process is not a psql shell and must not inherit
// them, so everything that decides *where* and *as whom* we connect is
// overwritten afterwards, and RuntimeParams is replaced wholesale rather than
// amended.
func pgConnConfig(addr string, req ExecRequest) (*pgx.ConnConfig, error) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrListenerDisabled, addr)
	}

	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 || port > math.MaxUint16 {
		return nil, fmt.Errorf("%w: %s", ErrListenerDisabled, addr)
	}

	dsn := (&url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(req.Username, req.APIKey),
		Host:     addr,
		Path:     "/" + req.Database,
		RawQuery: "sslmode=disable",
	}).String()

	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("building the loopback PostgreSQL config: %w", err)
	}

	cfg.Host = host
	cfg.Port = uint16(port)
	cfg.User = req.Username
	cfg.Password = req.APIKey
	cfg.Database = req.Database
	cfg.TLSConfig = nil
	cfg.Fallbacks = nil
	cfg.RuntimeParams = map[string]string{"application_name": mcpApplicationName}

	return cfg, nil
}

// normalizePGValue renders a pgx value as something json.Marshal turns into
// what an agent expects.
//
// pgx hands back rich types for several PostgreSQL types (numeric, interval,
// ranges, …). Most of them implement driver.Valuer and render as a string or a
// primitive, which is exactly the JSON an agent can reason about; anything
// that does not is passed through and marshaled on its own terms.
func normalizePGValue(v any) any {
	switch v.(type) {
	case nil, bool, string, []byte,
		int8, int16, int32, int64, int,
		uint8, uint16, uint32, uint64, uint,
		float32, float64:
		return v
	}

	if valuer, ok := v.(driver.Valuer); ok {
		if dv, err := valuer.Value(); err == nil {
			return dv
		}
	}

	return v
}
