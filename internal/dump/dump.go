package dump

import (
	"errors"
	"time"
)

// File format constants.
const (
	magic        = "DBBAT_DUMP\x00\x00\x00\x00\x00\x00" // 16 bytes
	magicSize    = 16
	minMagic     = 10 // length of "DBBAT_DUMP"
	version      = uint16(2)
	versionLen   = 2
	headerLenLen = 4
	FileExt      = ".dbbat-dump"
	eofMarker    = byte(0xFF)
)

// Packet direction constants.
const (
	DirClientToServer byte = 0x00
	DirServerToClient byte = 0x01
)

// Protocol identifiers.
const (
	ProtocolOracle     = "oracle"
	ProtocolPostgreSQL = "postgresql"
)

// Packet frame size: 8 (relativeNs) + 1 (direction) + 4 (length).
const packetFrameSize = 13

// Errors.
var (
	ErrInvalidMagic       = errors.New("invalid dump file magic")
	ErrUnsupportedVersion = errors.New("unsupported dump format version")
)

// Header holds the JSON-serializable session metadata.
type Header struct {
	SessionID  string         `json:"session_id"`
	Protocol   string         `json:"protocol"`
	StartTime  time.Time      `json:"start_time"`
	Connection map[string]any `json:"connection"`
}

// Packet represents a single captured packet.
type Packet struct {
	RelativeNs int64  // Nanoseconds since session start
	Direction  byte   // DirClientToServer or DirServerToClient
	Data       []byte // Raw protocol bytes
}
