package oracle

// Oracle TNS Marker packet types (sub-operation codes carried in a Control/Marker packet).
const (
	markerTypeBreak     = 0x01
	markerTypeReset     = 0x02
	markerTypeInterrupt = 0x03
)

// isBreakMarker reports whether a TNS Control/Marker packet signals a client break.
// Oracle clients send a break marker (type=1 or 3) to cancel the current operation.
// Payload layout: [flag=0x01, pad=0x00, markerType].
func isBreakMarker(pkt *TNSPacket) bool {
	if pkt.Type != TNSPacketTypeControl {
		return false
	}

	if len(pkt.Payload) < 3 {
		return false
	}

	return pkt.Payload[2] == markerTypeBreak || pkt.Payload[2] == markerTypeInterrupt
}

// isResetMarker reports whether a TNS Control/Marker packet signals a reset.
// After a break, the client sends a reset marker to synchronize.
func isResetMarker(pkt *TNSPacket) bool {
	if pkt.Type != TNSPacketTypeControl {
		return false
	}

	if len(pkt.Payload) < 3 {
		return false
	}

	return pkt.Payload[2] == markerTypeReset
}

// buildResetMarker returns the raw bytes of a TNS Reset Marker packet. This is the
// server's acknowledgement of a client's break — required for clients that implement
// OOB break signaling (e.g., sqlplus 23c) to proceed with the session.
//
// Packet layout (11 bytes, v315+ format):
//
//	[0]   = 0x00           first 2 bytes of 4-byte length field (upper 16 bits always 0 for small packets)
//	[1]   = 0x00
//	[2]   = 0x00           legacy 2-byte length (must be 0 to signal v315+ length)
//	[3]   = 0x0B           packet length = 11
//	[4]   = 0x0C           packet type = Control/Marker
//	[5]   = 0x00           flags
//	[6-7] = 0x00 0x00      header checksum
//	[8]   = 0x01           marker count
//	[9]   = 0x00           reserved
//	[10]  = 0x02           marker type = reset
func buildResetMarker() []byte {
	return []byte{0x00, 0x00, 0x00, 0x0B, 0x0C, 0x00, 0x00, 0x00, 0x01, 0x00, markerTypeReset}
}

// buildBreakMarker returns the raw bytes of a TNS Break Marker packet — the
// out-of-band signal an Oracle client sends to interrupt the call the server is
// executing (what Ctrl-C does in sqlplus).
//
// Same 11-byte v315+ layout as buildResetMarker, with the marker type changed;
// see that function for the field-by-field breakdown.
//
// **Unverified against a real Oracle server.** dbbat sends this on the watchdog
// teardown path as a courtesy — a server that honours it stops burning CPU on a
// statement nobody will read — but the socket close immediately after is what
// the enforcement actually rests on, and the end-to-end suite has not yet proven
// the marker alone ends the call. See docs/oracle.md.
func buildBreakMarker() []byte {
	return []byte{0x00, 0x00, 0x00, 0x0B, 0x0C, 0x00, 0x00, 0x00, 0x01, 0x00, markerTypeBreak}
}
