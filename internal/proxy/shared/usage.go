package shared

import (
	"sync/atomic"

	"github.com/fclairamb/dbbat/internal/cache"
	"github.com/fclairamb/dbbat/internal/store"
)

// RegisterGrantUsage adds a live session to its grant's shared consumption
// reading, so the grant's max_query_counts and max_bytes_transferred are one
// budget across every session holding it rather than one per session.
//
// grant's QueryCount and BytesTransferred must still be the connect-time
// snapshot. from/to are the session's wire byte counters. Returns nil (a no-op
// handle) for a nil grant or one with no quota: there is nothing to share.
// The caller releases the handle when the session ends.
func RegisterGrantUsage(registry *cache.UsageRegistry, grant *store.Grant, from, to *atomic.Int64) *cache.UsageHandle {
	if grant == nil || (grant.MaxQueryCounts() == nil && grant.MaxBytesTransferred() == nil) {
		return nil
	}

	return registry.Register(grant.UID, grant.QueryCount, grant.BytesTransferred, from, to)
}

// QuotaUsage is the grant's consumption the per-statement quota checks compare
// with its limits: the session's own reading (connect-time snapshot plus its
// own statements and bytes) plus what every other session of the grant used
// since. usage may be nil, which leaves the session's own reading.
//
// It returns (statements, bytes).
func QuotaUsage(grant *store.Grant, usage *cache.UsageHandle) (int64, int64) {
	return grant.QueryCount + usage.OtherQueries(), grant.BytesTransferred + usage.OtherBytes()
}
