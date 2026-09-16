package store

import "github.com/google/uuid"

// newUIDv7 generates a new UUIDv7 for high-volume tables.
// UUIDv7 is time-ordered for better B-tree index performance.
func newUIDv7() uuid.UUID {
	uid, err := uuid.NewV7()
	if err != nil {
		// Fallback to V4 if V7 fails (should never happen)
		return uuid.New()
	}
	return uid
}

// NewConnectionUID generates the uid a connection row will be created with,
// exposed so a proxy session can generate it ahead of CreateConnection —
// sometimes ahead of the upstream dial itself — and tag the upstream-facing
// application/program name with it (shared.BuildUpstreamName's "c=" field).
// CreateConnection is then told to use this exact value via WithUID, rather
// than generating its own, so the tag matches the row. Same generator
// createConnection uses internally when no uid is pinned.
func NewConnectionUID() uuid.UUID {
	return newUIDv7()
}
