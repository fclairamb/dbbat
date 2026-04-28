package oracle

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// ORA error codes for TNS Refuse packets.
const (
	ORA01017 uint16 = 1017  // ORA-01017: invalid username/password; logon denied
	ORA12505 uint16 = 12505 // TNS:listener does not currently know of SID
	ORA12514 uint16 = 12514 // TNS:listener does not currently know of service
	ORA12520 uint16 = 12520 // TNS:listener could not find available handler
	ORA12535 uint16 = 12535 // TNS:operation timed out
	ORA12541 uint16 = 12541 // TNS:no listener
)

// buildErrorRedirectPayload constructs a TNS Redirect payload that carries
// an Oracle error descriptor. Oracle uses Redirect (type 4) — not Refuse
// (type 3) — to report listener errors like ORA-12514 to JDBC/thin clients.
//
// The captured wire format from a real Oracle 19c listener is:
//
//	[data_len_hi:1] [0x00] [desc_len_hi:1] [desc_len_lo:1] [descriptor...]
//
// where the descriptor is an Oracle-style parenthesized string.
func buildErrorRedirectPayload(oraCode uint16, reason string) []byte {
	descriptor := fmt.Sprintf(
		"(DESCRIPTION=(TMP=)(VSNNUM=0)(ERR=%d)(ERROR_STACK=(ERROR=(CODE=%d)(EMFI=4)(ARGS='(%s)'))))",
		oraCode, oraCode, reason,
	)

	descBytes := []byte(descriptor)
	descLen := len(descBytes)

	// Redirect payload: [2 bytes header] [2 bytes desc length BE] [descriptor]
	payload := make([]byte, 4+descLen)
	payload[0] = byte((descLen + 4) >> 8) // data length high
	payload[1] = 0x00
	binary.BigEndian.PutUint16(payload[2:4], uint16(descLen)) // descriptor length
	copy(payload[4:], descBytes)

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
	// ErrClientAuthFailed indicates client authentication to dbbat failed.
	ErrClientAuthFailed = errors.New("client authentication failed")
	// ErrNoO5LogonVerifier indicates no API key with O5LOGON verifier was found.
	ErrNoO5LogonVerifier = errors.New("no API key with O5LOGON verifier found")
	// ErrAPIKeyOwnerMismatch indicates the API key does not belong to the authenticated user.
	ErrAPIKeyOwnerMismatchOracle = errors.New("API key does not belong to user")

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

	// O5LOGON and TTC auth errors.
	ErrDecryptedPasswordTooShort = errors.New("decrypted password too short")
	ErrCiphertextNotAligned      = errors.New("ciphertext is not a multiple of block size")
	ErrInvalidPadding            = errors.New("invalid PKCS7 padding")
	ErrAuthPhase1TooShort        = errors.New("AUTH Phase 1 payload too short")
	ErrAuthPhase1NoData          = errors.New("AUTH Phase 1: no data after sub-op")
	ErrAuthPhase1BadUsername     = errors.New("AUTH Phase 1: invalid username length")
	ErrAuthPhase2TooShort        = errors.New("AUTH Phase 2 payload too short")
	ErrAuthPhase2MissingSessKey  = errors.New("AUTH Phase 2: missing AUTH_SESSKEY")
	ErrAuthPhase2MissingPassword = errors.New("AUTH Phase 2: missing AUTH_PASSWORD")
	ErrUnexpectedPacketType      = errors.New("unexpected TNS packet type")
	ErrMaxResendExceeded         = errors.New("exceeded maximum resend attempts")
	ErrUpstreamTooManyRedirects  = errors.New("upstream: too many redirects")
	ErrRedirectMissingHostPort   = errors.New("redirect: could not extract HOST/PORT from descriptor")
)
