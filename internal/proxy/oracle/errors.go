package oracle

import "errors"

var (
	ErrEmptyUsername     = errors.New("empty username in AUTH message")
	ErrNoActiveGrant    = errors.New("no active grant for this user/database")
	ErrQueryLimitExceed = errors.New("query limit exceeded")
	ErrDataLimitExceed  = errors.New("data transfer limit exceeded")
	ErrDatabaseNotFound = errors.New("database not found")
	ErrUserNotFound     = errors.New("user not found")

	ErrTTCPayloadTooShort = errors.New("TTC payload too short")
	ErrNotDataPacket      = errors.New("expected TNS Data packet")
	ErrAuthFailed         = errors.New("upstream authentication failed")
)
