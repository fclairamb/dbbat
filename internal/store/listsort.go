package store

// Sort keys and directions accepted by the connections and queries listings.
//
// The keys are an allowlist mapped to SQL here; nothing from a request is ever
// interpolated into an ORDER BY.
const (
	SortAsc  = "asc"
	SortDesc = "desc"

	// ConnectionSortConnectedAt is the default: connections are ordered by uid
	// (a UUIDv7, so time-ordered).
	ConnectionSortConnectedAt = "connected_at"
	ConnectionSortDuration    = "duration"
	ConnectionSortQueries     = "queries"
	ConnectionSortBytes       = "bytes_transferred"

	// QuerySortExecutedAt is the default: queries are ordered by uid.
	QuerySortExecutedAt   = "executed_at"
	QuerySortDurationMs   = "duration_ms"
	QuerySortRowsAffected = "rows_affected"
)

// ValidConnectionSort reports whether key is an accepted connections sort key.
func ValidConnectionSort(key string) bool {
	switch key {
	case ConnectionSortConnectedAt, ConnectionSortDuration, ConnectionSortQueries, ConnectionSortBytes:
		return true
	}

	return false
}

// ValidQuerySort reports whether key is an accepted queries sort key.
func ValidQuerySort(key string) bool {
	switch key {
	case QuerySortExecutedAt, QuerySortDurationMs, QuerySortRowsAffected:
		return true
	}

	return false
}

// ValidSortOrder reports whether order is "asc" or "desc".
func ValidSortOrder(order string) bool {
	return order == SortAsc || order == SortDesc
}

// IsDefaultConnectionSort reports whether the sort is the uid keyset order,
// the only one before-cursor pagination is valid for.
func IsDefaultConnectionSort(key string) bool {
	return key == "" || key == ConnectionSortConnectedAt
}

// IsDefaultQuerySort is IsDefaultConnectionSort for queries.
func IsDefaultQuerySort(key string) bool {
	return key == "" || key == QuerySortExecutedAt
}

func sqlDirection(order string) string {
	if order == SortAsc {
		return "ASC"
	}

	return "DESC"
}

// connectionOrderExpr builds the ORDER BY for the connections listing. An
// unknown key falls back to the default (callers validate beforehand). uid DESC
// is always the tie-breaker so pages are stable.
func connectionOrderExpr(sort, order string) string {
	dir := sqlDirection(order)

	var col string

	switch sort {
	case ConnectionSortDuration:
		col = "COALESCE(disconnected_at, now()) - connected_at"
	case ConnectionSortQueries:
		col = "queries"
	case ConnectionSortBytes:
		col = "bytes_transferred"
	default:
		return "uid " + dir
	}

	return col + " " + dir + " NULLS LAST, uid DESC"
}

// queryOrderExpr builds the ORDER BY for the queries listing. NULLs (a
// statement still running has no duration) sort last in both directions.
func queryOrderExpr(sort, order string) string {
	dir := sqlDirection(order)

	var col string

	switch sort {
	case QuerySortDurationMs:
		col = "q.duration_ms"
	case QuerySortRowsAffected:
		col = "q.rows_affected"
	default:
		return "q.uid " + dir
	}

	return col + " " + dir + " NULLS LAST, q.uid DESC"
}
