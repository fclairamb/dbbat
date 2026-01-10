// Package store provides database access and persistence for DBBat.
package store

import "errors"

// Store errors.
var (
	ErrUserNotFound        = errors.New("user not found")
	ErrDatabaseNotFound    = errors.New("database not found")
	ErrGrantNotFound       = errors.New("grant not found")
	ErrNoActiveGrant       = errors.New("no active grant found")
	ErrGrantAlreadyRevoked = errors.New("grant not found or already revoked")
	ErrConnectionNotFound  = errors.New("connection not found or already closed")
	ErrQueryNotFound       = errors.New("query not found")
	ErrInvalidCursor       = errors.New("invalid cursor")
)
