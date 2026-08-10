package mcp

import (
	"context"
	"database/sql"
	"strings"
	"time"
	"unicode/utf8"
)

// executeSQLDB runs one statement on a database/sql handle already pointed at a
// dbbat proxy listener.
//
// Oracle and SQL Server both reach their proxy through an ordinary
// database/sql driver, so the row cap, the column handling and the value
// rendering live here once instead of twice. PostgreSQL and MySQL do not use
// it: pgx and go-mysql are driven natively so their result sets can be *read*
// incrementally rather than buffered.
//
// asExec selects ExecContext over QueryContext. That is a client-API choice,
// not a rewrite: the statement text handed to the driver is byte-for-byte the
// one the agent sent, and misjudging it can only cost the `rows_affected`
// field — never a row, and never an enforcement decision, which happens in the
// proxy on the other end of the socket.
func executeSQLDB(ctx context.Context, db *sql.DB, req ExecRequest, asExec bool) (*QueryResult, error) {
	if asExec {
		return execSQLDB(ctx, db, req)
	}

	return querySQLDB(ctx, db, req)
}

// execSQLDB runs a statement that returns no result set.
func execSQLDB(ctx context.Context, db *sql.DB, req ExecRequest) (*QueryResult, error) {
	res, err := db.ExecContext(ctx, req.SQL, req.Params...)
	if err != nil {
		return nil, err
	}

	result := &QueryResult{}

	// Not every driver reports it for every statement (DDL in particular), and
	// an unavailable count is better left absent than reported as zero.
	if affected, affErr := res.RowsAffected(); affErr == nil {
		result.RowsAffected = &affected
	}

	return result, nil
}

// querySQLDB runs a statement and reads at most MaxRows rows out of it.
func querySQLDB(ctx context.Context, db *sql.DB, req ExecRequest) (*QueryResult, error) {
	rows, err := db.QueryContext(ctx, req.SQL, req.Params...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	names, err := rows.Columns()
	if err != nil {
		return nil, err
	}

	result := &QueryResult{Columns: dedupeColumns(names)}

	values := make([]any, len(names))
	targets := make([]any, len(names))

	for i := range values {
		targets[i] = &values[i]
	}

	for rows.Next() {
		if len(result.Rows) >= req.MaxRows {
			result.Truncated = true

			break
		}

		if scanErr := rows.Scan(targets...); scanErr != nil {
			return nil, scanErr
		}

		row := make(map[string]any, len(result.Columns))
		for i, name := range result.Columns {
			row[name] = normalizeSQLValue(values[i])
		}

		result.Rows = append(result.Rows, row)
	}

	// Truncating means we stop reading a live result set; the deferred Close
	// discards the rest and rows.Err() would then report that rather than a
	// real failure, so it is only meaningful on a complete read.
	if !result.Truncated {
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}

	return result, nil
}

// normalizeSQLValue renders one scanned database/sql value as something
// json.Marshal turns into what an agent expects.
//
// database/sql hands back a small set of types, and two of them marshal badly
// on their own: a time.Time becomes a quoted RFC3339 string either way but only
// after a timezone decision, and a []byte becomes base64 — which would make
// every Oracle VARCHAR2 and every SQL Server nvarchar unreadable to a model.
func normalizeSQLValue(v any) any {
	switch value := v.(type) {
	case nil, bool, string, int64, float64:
		return v
	case time.Time:
		return value.UTC().Format(time.RFC3339Nano)
	case []byte:
		if utf8.Valid(value) {
			return string(value)
		}

		// Genuinely binary: keep the bytes (and the base64 rendering) but copy
		// them, since a driver may reuse its read buffer.
		return append([]byte(nil), value...)
	default:
		return v
	}
}

// leadingKeyword returns a statement's first word, lowercased.
//
// A statement that opens with a comment, a bind marker or anything else that is
// not a letter yields "", which sends it down the query path. That is the safe
// direction: the query path never loses rows, it only omits `rows_affected`.
func leadingKeyword(sqlText string) string {
	trimmed := strings.TrimLeft(sqlText, " \t\r\n(")

	end := 0
	for end < len(trimmed) && isLetter(trimmed[end]) {
		end++
	}

	return strings.ToLower(trimmed[:end])
}

func isLetter(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}
