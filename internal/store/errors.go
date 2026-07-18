// Package store provides database access and persistence for DBBat.
package store

import (
	"errors"

	"github.com/uptrace/bun/driver/pgdriver"
)

// Store errors.
var (
	ErrUserNotFound         = errors.New("user not found")
	ErrServerNotFound       = errors.New("database not found")
	ErrGrantNotFound        = errors.New("grant not found")
	ErrNoActiveGrant        = errors.New("no active grant found")
	ErrGrantAlreadyRevoked  = errors.New("grant not found or already revoked")
	ErrConnectionNotFound   = errors.New("connection not found or already closed")
	ErrQueryNotFound        = errors.New("query not found")
	ErrInvalidCursor        = errors.New("invalid cursor")
	ErrTargetMatchesStorage = errors.New("target database cannot match DBBat storage database")
	ErrIdentityNotFound     = errors.New("identity not found")
	ErrOAuthStateNotFound   = errors.New("oauth state not found")
	// ErrServerViaNotSSH is returned when via_uid points at a row whose protocol
	// is not 'ssh' — only SSH bastions can be tunneled through.
	ErrServerViaNotSSH = errors.New("via_uid must reference an ssh server")
	// ErrServerViaCycle is returned when a via_uid chain loops back on itself.
	ErrServerViaCycle = errors.New("via_uid chain forms a cycle")
	// ErrServerNameConflict is returned when creating or renaming a server to a
	// name that is already taken (violates the servers_name_key unique constraint).
	ErrServerNameConflict = errors.New("a server with this name already exists")
)

// isUniqueViolation reports whether err is a PostgreSQL unique-constraint
// violation (SQLSTATE 23505), optionally scoped to a specific constraint name.
// Pass an empty constraint to match any unique violation.
func isUniqueViolation(err error, constraint string) bool {
	var pgErr pgdriver.Error
	if !errors.As(err, &pgErr) {
		return false
	}
	if pgErr.Field('C') != "23505" {
		return false
	}
	return constraint == "" || pgErr.Field('n') == constraint
}
