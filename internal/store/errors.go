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
	// ErrServerViaNotSSH is returned when via_uid points at a row that is not a
	// dial path — only tunnel rows ('ssh' bastions and 'kubernetes' clusters)
	// can be tunneled through.
	//
	// The name predates the Kubernetes tunnel and is kept so existing callers
	// and tests keep matching; ErrServerViaNotTunnel is the accurate alias.
	ErrServerViaNotSSH = errors.New("via_uid must reference an ssh or kubernetes server")
	// ErrServerViaNotTunnel is the protocol-neutral name for ErrServerViaNotSSH.
	ErrServerViaNotTunnel = ErrServerViaNotSSH
	// ErrServerViaCycle is returned when a via_uid chain loops back on itself.
	ErrServerViaCycle = errors.New("via_uid chain forms a cycle")
	// ErrServerNameConflict is returned when creating or renaming a server to a
	// name that is already taken (violates the servers_name_key unique constraint).
	ErrServerNameConflict = errors.New("a server with this name already exists")
	// ErrUserNameConflict is returned when creating a user whose username is
	// already taken by an active (non-soft-deleted) user (violates the
	// users_username_active_uq unique index).
	ErrUserNameConflict = errors.New("a user with this username already exists")
	// ErrGrantDefinitionRequired is returned when a grant is created without
	// naming the definition it instantiates. A grant carries no shape of its
	// own, so one without a definition would authorize nothing meaningful —
	// it is rejected rather than stored.
	ErrGrantDefinitionRequired = errors.New("a grant must reference a grant definition")
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
