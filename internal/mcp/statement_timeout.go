package mcp

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrStatementTimeout is what an agent is told when dbbat cancelled its
// statement for running past the grant's per-statement limit.
//
// It exists as its own error, rather than the raw driver failure being passed
// through, because the raw failure is useless to the caller: PostgreSQL says
// "canceling statement due to statement timeout" with no mention of who
// configured it, MySQL says "Query execution was interrupted", and on Oracle and
// SQL Server — which have no server-side limit at all — the watchdog's socket
// close arrives as a bare connection reset. An agent reading any of those
// retries; an agent told it exceeded a named limit narrows its query, which is
// the whole behavioural point.
var ErrStatementTimeout = errors.New("statement exceeded the per-statement time limit of your grant and was cancelled")

// statementTimeoutSignatures are the messages a cancelled statement produces on
// each protocol's client library.
//
// Matched on text because that is all the drivers expose in common: pgx surfaces
// SQLSTATE 57014 as a *pgconn.PgError, go-mysql an error number, the MongoDB
// driver a code — three different types, none of which the other executors can
// see. The strings below are the stable, documented server messages, and a
// false positive only ever changes the wording of an error that already failed.
var statementTimeoutSignatures = []string{
	// PostgreSQL, SQLSTATE 57014 (query_canceled).
	"canceling statement due to statement timeout",
	// PostgreSQL, the watchdog's CancelRequest rather than the server-side
	// setting.
	"canceling statement due to user request",
	// MySQL 3024, and MariaDB's max_statement_time.
	"maximum statement execution time exceeded",
	"query execution was interrupted",
	// MongoDB, error code 50.
	"maxtimemsexpired",
	"operation exceeded time limit",
	// dbbat's own courtesy refusal of an attempt to unset the limit.
	"is managed by dbbat",
	"managed by dbbat",
}

// classifyStatementTimeout rewrites err as ErrStatementTimeout when it is one,
// naming the limit that was crossed.
//
// limit is the grant's resolved per-statement limit; zero means no limit
// applies, in which case nothing is reclassified — a statement that was
// cancelled for some other reason must keep saying so.
//
// The original error is wrapped rather than discarded: an operator reading the
// audit trail still wants the protocol's own words.
func classifyStatementTimeout(err error, limit time.Duration) error {
	if err == nil || limit <= 0 {
		return err
	}

	if !isStatementTimeout(err) {
		return err
	}

	return fmt.Errorf("%w (limit %s): %w", ErrStatementTimeout, limit, err)
}

// isStatementTimeout reports whether err carries one of the signatures above.
func isStatementTimeout(err error) bool {
	text := strings.ToLower(err.Error())

	for _, signature := range statementTimeoutSignatures {
		if strings.Contains(text, signature) {
			return true
		}
	}

	return false
}
