package oracle

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// ORA error codes for TNS Refuse packets.
const (
	ORA12505 uint16 = 12505 // TNS:listener does not currently know of SID
	ORA12514 uint16 = 12514 // TNS:listener does not currently know of service
	ORA12520 uint16 = 12520 // TNS:listener could not find available handler
	ORA12535 uint16 = 12535 // TNS:operation timed out
	ORA12541 uint16 = 12541 // TNS:no listener
)

// buildRefusePayload constructs a properly formatted TNS Refuse payload.
// Real Oracle Refuse packets have: [user_reason:2][system_reason:2][descriptor...]
// The descriptor is an Oracle-style parenthesized string.
func buildRefusePayload(oraCode uint16, reason string) []byte {
	descriptor := fmt.Sprintf(
		"(DESCRIPTION=(ERR=%d)(VSNNUM=0)(ERROR_STACK=(ERROR=(CODE=%d)(EMFI=4)(ARGS='(%s)'))))",
		oraCode, oraCode, reason,
	)

	payload := make([]byte, 4+len(descriptor))
	binary.BigEndian.PutUint16(payload[0:2], 0x0004) // User reason: user error
	binary.BigEndian.PutUint16(payload[2:4], 0x0000) // System reason: none
	copy(payload[4:], descriptor)

	return payload
}

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
