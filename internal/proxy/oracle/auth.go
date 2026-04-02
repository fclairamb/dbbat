package oracle

import (
	"fmt"
	"log/slog"
	"strings"
)

// TTC function codes relevant to auth.
const (
	ttcFuncSetProtocol  byte = 0x01
	ttcFuncSetDataTypes byte = 0x02
	ttcFuncOAUTH        byte = 0x73
	ttcFuncResponse     byte = 0x08

	// Data flags size at the start of TNS Data payload.
	tnsDataFlagsSize = 2
)

// handleAuthPhase relays TTC negotiation and authentication between client and upstream,
// intercepting the AUTH phase to extract the username and check grants.
func (s *session) handleAuthPhase() error {
	// Relay packets until auth is complete.
	// The flow is:
	// 1. Set Protocol (client → upstream, upstream → client)
	// 2. Set Data Types (client → upstream, upstream → client)
	// 3. AUTH Phase 1 (client → proxy: extract username, check grant, relay to upstream)
	// 4. AUTH Challenge (upstream → client)
	// 5. AUTH Phase 2 (client → upstream)
	// 6. AUTH Response (upstream → client — success or failure)
	//
	// We identify AUTH messages by the TTC function code (0x73) in the first Data packet
	// from the client that isn't Set Protocol or Set Data Types.

	authComplete := false
	authPhasesSeen := 0

	for !authComplete {
		// Read from client
		clientPkt, err := readTNSPacket(s.clientConn)
		if err != nil {
			return fmt.Errorf("failed to read from client during auth: %w", err)
		}

		// Only inspect Data packets
		if clientPkt.Type == TNSPacketTypeData && len(clientPkt.Payload) > tnsDataFlagsSize {
			funcCode := clientPkt.Payload[tnsDataFlagsSize]

			// If this is an AUTH message and we haven't extracted the username yet
			if funcCode == ttcFuncOAUTH && s.username == "" {
				username, extractErr := extractUsernameFromAuth(clientPkt.Payload)
				if extractErr != nil {
					s.logger.WarnContext(s.ctx, "failed to extract username from AUTH",
						slog.Any("error", extractErr))
				} else {
					s.username = username
					s.logger.InfoContext(s.ctx, "extracted username from AUTH",
						slog.String("username", username))

					// Check grants before relaying to upstream
					if err := s.checkAccess(); err != nil {
						s.sendRefuse(err.Error())

						return err
					}
				}
			}

			if funcCode == ttcFuncOAUTH {
				authPhasesSeen++
			}
		}

		// Forward to upstream
		if err := writeTNSPacket(s.upstreamConn, clientPkt); err != nil {
			return fmt.Errorf("failed to forward to upstream during auth: %w", err)
		}

		// Read response(s) from upstream and forward to client
		// After each client message, upstream sends one or more responses
		for {
			upstreamPkt, err := readTNSPacket(s.upstreamConn)
			if err != nil {
				return fmt.Errorf("failed to read from upstream during auth: %w", err)
			}

			// Forward to client
			if err := writeTNSPacket(s.clientConn, upstreamPkt); err != nil {
				return fmt.Errorf("failed to forward to client during auth: %w", err)
			}

			// Check if this is a response that completes an auth round
			if upstreamPkt.Type == TNSPacketTypeData && len(upstreamPkt.Payload) > tnsDataFlagsSize {
				funcCode := upstreamPkt.Payload[tnsDataFlagsSize]
				if funcCode == ttcFuncResponse || funcCode == ttcFuncOAUTH {
					// After seeing 2+ AUTH phases from client and getting a response,
					// auth is complete (success or failure — either way, we're done)
					if authPhasesSeen >= 2 {
						authComplete = true
					}

					break
				}
			}

			// Non-auth responses (Set Protocol/Data Types) — just one response per request
			break
		}
	}

	return nil
}

// checkAccess verifies the user has an active grant for the database.
func (s *session) checkAccess() error {
	// Look up user
	user, err := s.store.GetUserByUsername(s.ctx, s.username)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrUserNotFound, s.username)
	}

	s.user = user

	// Check for active grant
	grant, err := s.store.GetActiveGrant(s.ctx, user.UID, s.database.UID)
	if err != nil {
		return fmt.Errorf("%w: user=%s database=%s", ErrNoActiveGrant, s.username, s.database.Name)
	}

	s.grant = grant

	// Check quotas
	if grant.MaxQueryCounts != nil && grant.QueryCount >= *grant.MaxQueryCounts {
		return ErrQueryLimitExceed
	}

	if grant.MaxBytesTransferred != nil && grant.BytesTransferred >= *grant.MaxBytesTransferred {
		return ErrDataLimitExceed
	}

	return nil
}

// extractUsernameFromAuth extracts the username from a TTC AUTH Phase 1 message.
//
// The AUTH message (function code 0x73) contains the username as one of the first
// fields. The encoding varies by Oracle version, but the username is typically
// sent as a length-prefixed string early in the payload.
//
// Simplified extraction: scan for readable ASCII username after the function code.
// This works for the common case where username is sent in cleartext in AUTH Phase 1.
func extractUsernameFromAuth(tnsDataPayload []byte) (string, error) {
	if len(tnsDataPayload) <= tnsDataFlagsSize+1 {
		return "", ErrTTCPayloadTooShort
	}

	// Skip data flags (2 bytes) + function code (1 byte)
	payload := tnsDataPayload[tnsDataFlagsSize+1:]

	// The AUTH Phase 1 structure (simplified):
	// After the function code, there's a sequence number and logon mode,
	// then key-value pairs. The username is sent as one of the first pairs.
	//
	// Strategy: look for a length-prefixed string that looks like a username.
	// In Oracle's TTC encoding, strings are often prefixed with their length
	// as a single byte (for short strings) followed by the UTF-8 bytes.

	// Scan through the payload looking for a plausible username.
	// The username in AUTH Phase 1 typically appears within the first ~50 bytes
	// after the function code, as a length-prefixed string.
	username := extractLengthPrefixedString(payload)
	if username == "" {
		// Fallback: scan for the longest sequence of printable ASCII
		username = extractPrintableASCII(payload)
	}

	if username == "" {
		return "", ErrEmptyUsername
	}

	// Oracle usernames are typically uppercase
	return strings.ToUpper(username), nil
}

// extractLengthPrefixedString tries to find a length-prefixed string in the payload.
// Returns the first plausible username found.
func extractLengthPrefixedString(payload []byte) string {
	// Skip the first few bytes (sequence number, logon mode flags)
	// and look for a byte that could be a string length followed by that many
	// printable characters.
	for i := 0; i < len(payload)-1 && i < 64; i++ {
		strLen := int(payload[i])
		if strLen < 1 || strLen > 128 || i+1+strLen > len(payload) {
			continue
		}

		candidate := string(payload[i+1 : i+1+strLen])
		if isPlausibleUsername(candidate) {
			return candidate
		}
	}

	return ""
}

// extractPrintableASCII extracts the longest run of printable ASCII from the payload.
func extractPrintableASCII(payload []byte) string {
	var best string
	var current []byte

	for _, b := range payload {
		if b >= 0x20 && b < 0x7F && b != ' ' {
			current = append(current, b)
		} else {
			if len(current) > len(best) && len(current) >= 2 {
				candidate := string(current)
				if isPlausibleUsername(candidate) {
					best = candidate
				}
			}

			current = current[:0]
		}
	}

	if len(current) > len(best) && len(current) >= 2 {
		candidate := string(current)
		if isPlausibleUsername(candidate) {
			best = candidate
		}
	}

	return best
}

// isPlausibleUsername checks if a string looks like an Oracle username.
func isPlausibleUsername(s string) bool {
	if len(s) < 1 || len(s) > 128 {
		return false
	}

	// Oracle usernames: letters, digits, underscores, $, #
	for _, c := range s {
		if !((c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') ||
			(c >= '0' && c <= '9') || c == '_' || c == '$' || c == '#') {
			return false
		}
	}

	// First character should be a letter
	first := s[0]

	return (first >= 'A' && first <= 'Z') || (first >= 'a' && first <= 'z')
}
