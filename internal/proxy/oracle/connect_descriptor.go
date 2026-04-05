package oracle

import (
	"regexp"
	"strconv"
	"strings"
)

// ConnectDescriptor holds metadata parsed from an Oracle connect descriptor.
type ConnectDescriptor struct {
	ServiceName string
	SID         string
	Host        string
	Port        int
	Program     string // From CID
	OSUser      string // From CID
}

var (
	serviceNameRe = regexp.MustCompile(`(?i)SERVICE_NAME\s*=\s*([^)]+)`)
	sidRe         = regexp.MustCompile(`(?i)SID\s*=\s*([^)]+)`)
	hostRe        = regexp.MustCompile(`(?i)HOST\s*=\s*([^)]+)`)
	portRe        = regexp.MustCompile(`(?i)PORT\s*=\s*([^)]+)`)
	programRe     = regexp.MustCompile(`(?i)PROGRAM\s*=\s*([^)]+)`)
	userRe        = regexp.MustCompile(`(?i)USER\s*=\s*([^)]+)`)
)

// parseServiceName extracts SERVICE_NAME from a connect descriptor string.
func parseServiceName(descriptor string) string {
	return extractField(descriptor, serviceNameRe)
}

// parseSID extracts SID from a connect descriptor string.
func parseSID(descriptor string) string {
	return extractField(descriptor, sidRe)
}

// parseServiceNameEZConnect extracts the service name from EZ Connect format.
// EZ Connect: host:port/service_name or host/service_name
func parseServiceNameEZConnect(descriptor string) string {
	// Not a parenthesized descriptor — try EZ Connect
	slashIdx := strings.LastIndex(descriptor, "/")
	if slashIdx == -1 || slashIdx == len(descriptor)-1 {
		return ""
	}

	return strings.TrimSpace(descriptor[slashIdx+1:])
}

// parseConnectDescriptor parses a full Oracle connect descriptor into its components.
func parseConnectDescriptor(descriptor string) ConnectDescriptor {
	cd := ConnectDescriptor{
		ServiceName: parseServiceName(descriptor),
		SID:         parseSID(descriptor),
		Host:        extractField(descriptor, hostRe),
		Program:     extractField(descriptor, programRe),
		OSUser:      extractField(descriptor, userRe),
	}

	if portStr := extractField(descriptor, portRe); portStr != "" {
		if port, err := strconv.Atoi(portStr); err == nil {
			cd.Port = port
		}
	}

	return cd
}

// extractConnectString extracts the connect descriptor string from a TNS Connect packet payload.
// The connect descriptor starts after the TNS Connect header fields.
func extractConnectString(payload []byte) string {
	// TNS Connect packet payload structure:
	// Bytes 0-1:  version
	// Bytes 2-3:  version compatible
	// Bytes 4-5:  service options
	// Bytes 6-7:  SDU size
	// Bytes 8-9:  TDU size
	// Bytes 10-11: protocol characteristics
	// Bytes 12-13: line turnaround
	// Bytes 14-15: value of one (byte order)
	// Bytes 16-17: connect data length
	// Bytes 18-19: connect data offset
	// Bytes 20-23: max receivable connect data
	// Bytes 24:    connect flags 0
	// Bytes 25:    connect flags 1
	// ... then connect data at the specified offset

	if len(payload) < 26 {
		// Try to find the descriptor by looking for opening paren
		return findDescriptorInPayload(payload)
	}

	connectDataLen := int(payload[16])<<8 | int(payload[17])
	connectDataOffset := int(payload[18])<<8 | int(payload[19])

	// The offset can be interpreted two ways depending on TNS version:
	// - Old TNS: offset from start of full packet (subtract tnsHeaderSize for payload index)
	// - New TNS (v315+): offset happens to match because extended data is appended
	// Try both: first the raw offset (works for v315+), then adjusted (works for old TNS).
	for _, payloadOffset := range []int{connectDataOffset, connectDataOffset - tnsHeaderSize} {
		if payloadOffset >= 0 && payloadOffset < len(payload) {
			end := payloadOffset + connectDataLen
			if end > len(payload) {
				end = len(payload)
			}

			candidate := string(payload[payloadOffset:end])
			if strings.Contains(candidate, "DESCRIPTION") || strings.Contains(candidate, "SERVICE_NAME") {
				return candidate
			}
		}
	}

	// Fallback: scan for parenthesized descriptor
	return findDescriptorInPayload(payload)
}

// findDescriptorInPayload scans payload bytes for a parenthesized descriptor.
func findDescriptorInPayload(payload []byte) string {
	s := string(payload)
	start := strings.Index(s, "(")
	if start == -1 {
		return ""
	}

	return s[start:]
}

// rewriteServiceName replaces the SERVICE_NAME value in a TNS Connect packet.
// Uses same-length replacement (padding shorter names with spaces, or truncating longer
// names to fit) to preserve the exact packet structure and all length fields.
func rewriteServiceName(pkt *TNSPacket, oldName, newName string) *TNSPacket {
	// Pad or truncate the new name to match the old name's length,
	// so no length fields need updating anywhere in the packet.
	paddedName := newName
	if len(paddedName) < len(oldName) {
		// Pad with closing paren + reopen to maintain descriptor structure.
		// Actually, Oracle descriptors tolerate trailing spaces in values.
		// Use exact padding: "TEST01    " to match "abynonprod" length.
		paddedName = paddedName + strings.Repeat(" ", len(oldName)-len(paddedName))
	} else if len(paddedName) > len(oldName) {
		// Truncate (unlikely in practice but safe)
		paddedName = paddedName[:len(oldName)]
	}

	re := regexp.MustCompile(`(?i)(SERVICE_NAME\s*=\s*)` + regexp.QuoteMeta(oldName))

	// Replace in Payload
	oldPayloadStr := string(pkt.Payload)
	newPayloadStr := re.ReplaceAllString(oldPayloadStr, "${1}"+paddedName)

	if newPayloadStr == oldPayloadStr {
		return pkt
	}

	result := &TNSPacket{
		Type:    pkt.Type,
		Flags:   pkt.Flags,
		Payload: []byte(newPayloadStr),
	}

	// Replace in Raw bytes too (same-length, so all headers stay valid)
	if len(pkt.Raw) > 0 {
		oldRawStr := string(pkt.Raw)
		newRawStr := re.ReplaceAllString(oldRawStr, "${1}"+paddedName)
		result.Raw = []byte(newRawStr)
	}

	return result
}

// extractField extracts a named field value from a descriptor using a compiled regex.
func extractField(descriptor string, re *regexp.Regexp) string {
	m := re.FindStringSubmatch(descriptor)
	if len(m) > 1 {
		return strings.TrimSpace(m[1])
	}

	return ""
}
