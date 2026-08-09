package mcp

import (
	"context"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/go-mysql-org/go-mysql/client"
	gomysql "github.com/go-mysql-org/go-mysql/mysql"
)

// mysqlConnectTimeout bounds the handshake with our own listener. Reaching
// loopback is either immediate or broken.
const mysqlConnectTimeout = 10 * time.Second

// errRowCapReached aborts a streaming read from inside the row callback;
// go-mysql offers no other way to stop mid-result-set. It never escapes
// mysqlExecuteStreaming.
var errRowCapReached = errors.New("row cap reached")

// executeMySQL runs one statement against the MySQL/MariaDB proxy listener.
//
// The connection is plaintext: it never leaves loopback, and go-mysql's
// caching_sha2_password full-auth path then negotiates the RSA public-key
// exchange the dbbat proxy implements, which is how the `dbb_` key reaches the
// proxy as a cleartext credential without a TLS leg.
//
// go-mysql's Execute takes no context, so cancellation is done the only way
// the protocol allows from the client side: closing the socket. That is
// exactly the client-disconnect path the approval gate already handles — a
// hold ends as `abandoned` and nothing is forwarded upstream.
func executeMySQL(ctx context.Context, addr string, req ExecRequest) (*QueryResult, error) {
	conn, err := client.ConnectWithContext(ctx, addr, req.Username, req.APIKey, req.Database, mysqlConnectTimeout)
	if err != nil {
		return nil, fmt.Errorf("connecting to the dbbat MySQL proxy: %w", err)
	}

	done := make(chan struct{})
	defer close(done)

	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
			_ = conn.Close()
		}
	}()

	if len(req.Params) > 0 {
		return mysqlExecuteBuffered(conn, req)
	}

	return mysqlExecuteStreaming(conn, req)
}

// mysqlExecuteStreaming is the no-parameter path (COM_QUERY). It stops reading
// the moment the row cap is reached instead of letting go-mysql buffer the
// whole result set — an agent writing `SELECT * FROM events` must not be able
// to make the dbbat process hold the table in memory.
func mysqlExecuteStreaming(conn *client.Conn, req ExecRequest) (*QueryResult, error) {
	result := &QueryResult{}
	res := &gomysql.Result{}

	onResult := func(r *gomysql.Result) error {
		names := make([]string, len(r.Fields))
		for i, f := range r.Fields {
			names[i] = string(f.Name)
		}

		result.Columns = dedupeColumns(names)

		return nil
	}

	onRow := func(row []gomysql.FieldValue) error {
		if len(result.Rows) >= req.MaxRows {
			result.Truncated = true

			return errRowCapReached
		}

		result.Rows = append(result.Rows, mysqlRow(result.Columns, row))

		return nil
	}

	if err := conn.ExecuteSelectStreaming(req.SQL, res, onRow, onResult); err != nil {
		if !result.Truncated {
			return nil, err
		}
		// The abort left unread packets on the wire; the connection is closed
		// by the caller's deferred cleanup and never reused.
	}

	mysqlSetAffected(result, res)

	return result, nil
}

// mysqlExecuteBuffered is the parameterized path. go-mysql only offers
// prepared-statement execution through the buffering Execute, so the cap is
// applied after the fact here; the parameters themselves are what make this
// path the right one for untrusted values.
func mysqlExecuteBuffered(conn *client.Conn, req ExecRequest) (*QueryResult, error) {
	res, err := conn.Execute(req.SQL, req.Params...)
	if err != nil {
		return nil, err
	}

	result := &QueryResult{}

	if res.Resultset != nil {
		names := make([]string, len(res.Fields))
		for i, f := range res.Fields {
			names[i] = string(f.Name)
		}

		result.Columns = dedupeColumns(names)

		for _, row := range res.Values {
			if len(result.Rows) >= req.MaxRows {
				result.Truncated = true

				break
			}

			result.Rows = append(result.Rows, mysqlRow(result.Columns, row))
		}
	}

	mysqlSetAffected(result, res)

	return result, nil
}

// mysqlSetAffected records rows-affected for statements that report one.
func mysqlSetAffected(result *QueryResult, res *gomysql.Result) {
	if len(result.Columns) > 0 {
		return
	}

	affected := int64(res.AffectedRows)
	result.RowsAffected = &affected
}

// mysqlRow converts one wire row into a column-keyed map.
//
// The []byte a FieldValue points at is backed by go-mysql's reused read
// buffer, so every value must be copied here — keeping the slice would hand
// the agent whatever packet happened to land next.
func mysqlRow(columns []string, row []gomysql.FieldValue) map[string]any {
	out := make(map[string]any, len(columns))

	for i, name := range columns {
		if i >= len(row) {
			break
		}

		out[name] = mysqlValue(row[i])
	}

	return out
}

// mysqlValue renders one MySQL field.
//
// MySQL returns text columns as bytes, so a []byte that is valid UTF-8 is
// turned into a string — base64 would make every VARCHAR unreadable to an
// agent. Genuinely binary columns keep their bytes and marshal as base64.
func mysqlValue(v gomysql.FieldValue) any {
	switch v.Type {
	case gomysql.FieldValueTypeNull:
		return nil
	case gomysql.FieldValueTypeUnsigned:
		return v.AsUint64()
	case gomysql.FieldValueTypeSigned:
		return v.AsInt64()
	case gomysql.FieldValueTypeFloat:
		return v.AsFloat64()
	case gomysql.FieldValueTypeString:
		b := v.AsString()
		if utf8.Valid(b) {
			return string(b)
		}

		return append([]byte(nil), b...)
	default:
		return nil
	}
}
