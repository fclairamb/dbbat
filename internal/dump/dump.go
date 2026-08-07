package dump

import (
	"errors"
	"time"
)

// FileExt is the extension used for session capture files. Captures are plain
// pcapng, readable by tcpdump/Wireshark/tshark without any dbbat tooling.
const FileExt = ".pcapng"

// ContentType is the MIME type captures are served and stored with.
const ContentType = "application/x-pcapng"

// Packet direction constants.
const (
	DirClientToServer byte = 0x00
	DirServerToClient byte = 0x01
)

// Protocol identifiers.
const (
	ProtocolOracle     = "oracle"
	ProtocolPostgreSQL = "postgresql"
	ProtocolMySQL      = "mysql"
	ProtocolMongo      = "mongodb"
	ProtocolMSSQL      = "mssql"
)

// ErrMissingMetadata is returned when a capture carries no dbbat session
// metadata in its Section Header Block comment.
var ErrMissingMetadata = errors.New("capture has no dbbat session metadata")

// Header holds the JSON-serializable session metadata. It is stored as a JSON
// blob in the pcapng Section Header Block comment (opt_comment).
type Header struct {
	SessionID  string         `json:"session_id"`
	Protocol   string         `json:"protocol"`
	StartTime  time.Time      `json:"start_time"`
	Connection map[string]any `json:"connection"`
}

// Packet represents a single captured application-layer payload.
type Packet struct {
	RelativeNs int64  // Nanoseconds since session start
	Direction  byte   // DirClientToServer or DirServerToClient
	Data       []byte // Raw protocol bytes (TCP payload, synthesized headers stripped)
}
