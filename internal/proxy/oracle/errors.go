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

	// Session errors.
	ErrExpectedConnectPacket = errors.New("expected TNS Connect packet")
	ErrNoServiceName         = errors.New("no SERVICE_NAME in connect descriptor")
	ErrUpstreamRefused       = errors.New("upstream refused connection")

	// Decode errors.
	ErrColumnDefTooShort   = errors.New("column definition too short")
	ErrColumnNameTruncated = errors.New("column name exceeds payload")
	ErrNoTypeCode          = errors.New("column definition missing type code")
	ErrEmptyRowData        = errors.New("empty row data")
	ErrRowValueTruncated   = errors.New("row value exceeds payload")
	ErrInvalidFloatLength  = errors.New("invalid float data length")
)
