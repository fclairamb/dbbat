package oracle

import "encoding/binary"

// TNS Accept packet fields.
const (
	tnsAcceptVersion      = 0x013C // Version 316
	tnsAcceptServiceFlags = 0x0000
	tnsAcceptSDUSize      = 0x2000 // 8192 bytes
	tnsAcceptTDUSize      = 0xFFFF // 65535 bytes
)

// buildTNSAccept crafts a TNS Accept packet for the client.
// This is sent by dbbat (acting as Oracle server) in response to the client's Connect.
func buildTNSAccept() []byte {
	// Accept header (24 bytes after TNS header):
	// Version (2) + Service Options (2) + SDU (2) + TDU (2) + unused (16)
	acceptPayload := make([]byte, 24)
	binary.BigEndian.PutUint16(acceptPayload[0:2], tnsAcceptVersion)
	binary.BigEndian.PutUint16(acceptPayload[2:4], tnsAcceptServiceFlags)
	binary.BigEndian.PutUint16(acceptPayload[4:6], tnsAcceptSDUSize)
	binary.BigEndian.PutUint16(acceptPayload[6:8], tnsAcceptTDUSize)
	// Remaining 16 bytes are zeros (hardware info, etc.)

	return encodeTNSPacket(TNSPacketTypeAccept, acceptPayload)
}

// buildSetProtocolResponse constructs a Set Protocol (OSETPRO) response.
// This is the server's response to the client's Set Protocol request.
//
// The response is a TNS Data packet with:
//   - Data flags: 0x0000
//   - Function code: 0x01 (OSETPRO)
//   - Protocol version negotiation data
//
// This is a minimal response captured from a real Oracle 19c server.
func buildSetProtocolResponse() []byte {
	// Minimal Set Protocol response:
	// data_flags(2) + func_code(1) + protocol_data
	payload := []byte{
		0x00, 0x00, // data flags
		byte(TTCFuncSetProtocol), // 0x01
		// Protocol negotiation response — accept client's proposed protocol
		0x06,       // protocol version
		0x00,       // compatibility flags
		0x00, 0x00, // service options
		0x00, 0x01, // session data unit
		0x00, 0x00, // transport data unit
		0x06,       // char set id (UTF-8)
		0x01,       // char set form
		0xb2, 0x00, // server character set (AL32UTF8 = 873)
		0x03, 0x61, // national character set (UTF-16)
		0x01,       // server flags
		0x00, 0x20, // buffer length
	}

	return encodeTNSPacket(TNSPacketTypeData, payload)
}

// buildSetDataTypesResponse constructs a Set Data Types (ODTYPES) response.
// This is the server's response to the client's data type negotiation request.
//
// The response contains the server's supported data types and their representations.
// This is a minimal response that satisfies Oracle thin clients.
func buildSetDataTypesResponse() []byte {
	// Set Data Types response — minimal response accepted by Oracle thin clients.
	// Format: data_flags(2) + func_code(1) + type_rep_count + type_rep_data
	payload := []byte{
		0x00, 0x00, // data flags
		byte(TTCFuncSetDataTypes), // 0x02
		// Type representation data — server acknowledges client's types
		0x00, // acceptance flag
	}

	return encodeTNSPacket(TNSPacketTypeData, payload)
}
