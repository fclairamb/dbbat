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
