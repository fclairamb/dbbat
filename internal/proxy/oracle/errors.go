package oracle

import "errors"

// Oracle proxy errors.
var (
	// ErrEmptyUsername indicates an AUTH message with no username.
	ErrEmptyUsername = errors.New("empty username in AUTH message")
	// ErrNoActiveGrant indicates no active grant exists for the user/database pair.
	ErrNoActiveGrant = errors.New("no active grant for this user/database")
	// ErrQueryLimitExceed indicates the grant's query count quota has been reached.
	ErrQueryLimitExceed = errors.New("query limit exceeded")
	// ErrDataLimitExceed indicates the grant's data transfer quota has been reached.
	ErrDataLimitExceed = errors.New("data transfer limit exceeded")
	// ErrDatabaseNotFound indicates the requested database was not found in the store.
	ErrDatabaseNotFound = errors.New("database not found")
	// ErrUserNotFound indicates the requested user was not found in the store.
	ErrUserNotFound = errors.New("user not found")

	// ErrTTCPayloadTooShort indicates a TTC payload is shorter than expected.
	ErrTTCPayloadTooShort = errors.New("TTC payload too short")
	// ErrNotDataPacket indicates a non-Data TNS packet was received where Data was expected.
	ErrNotDataPacket = errors.New("expected TNS Data packet")
	// ErrAuthFailed indicates upstream authentication did not succeed.
	ErrAuthFailed = errors.New("upstream authentication failed")

	// ErrExpectedConnectPacket indicates a non-Connect packet was received at session start.
	ErrExpectedConnectPacket = errors.New("expected TNS Connect packet")
	// ErrNoServiceName indicates the connect descriptor lacks a SERVICE_NAME.
	ErrNoServiceName = errors.New("no SERVICE_NAME in connect descriptor")
	// ErrUpstreamRefused indicates the upstream Oracle server refused the connection.
	ErrUpstreamRefused = errors.New("upstream refused connection")

	// ErrColumnDefTooShort indicates a column definition is shorter than expected.
	ErrColumnDefTooShort = errors.New("column definition too short")
	// ErrColumnNameTruncated indicates a column name extends beyond the payload.
	ErrColumnNameTruncated = errors.New("column name exceeds payload")
	// ErrNoTypeCode indicates a column definition is missing the type code byte.
	ErrNoTypeCode = errors.New("column definition missing type code")
	// ErrEmptyRowData indicates an empty row data payload.
	ErrEmptyRowData = errors.New("empty row data")
	// ErrRowValueTruncated indicates a row value extends beyond the payload.
	ErrRowValueTruncated = errors.New("row value exceeds payload")
	// ErrInvalidFloatLength indicates float data has an unexpected length.
	ErrInvalidFloatLength = errors.New("invalid float data length")
)
