package dump

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
)

// Reader reads packets from a dump file.
type Reader struct {
	file   *os.File
	header Header
}

// OpenReader opens a dump file and parses the header.
func OpenReader(path string) (*Reader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open dump file: %w", err)
	}

	r := &Reader{file: f}

	if err := r.readHeader(); err != nil {
		_ = f.Close()

		return nil, err
	}

	return r, nil
}

func (r *Reader) readHeader() error {
	// Magic (16 bytes)
	magicBuf := make([]byte, magicSize)
	if _, err := io.ReadFull(r.file, magicBuf); err != nil {
		return fmt.Errorf("read magic: %w", err)
	}

	if string(magicBuf[:minMagic]) != magic[:minMagic] {
		return ErrInvalidMagic
	}

	// Version (uint16 BE)
	var vBuf [versionLen]byte
	if _, err := io.ReadFull(r.file, vBuf[:]); err != nil {
		return fmt.Errorf("read version: %w", err)
	}

	ver := binary.BigEndian.Uint16(vBuf[:])
	if ver != version {
		return fmt.Errorf("%w: %d", ErrUnsupportedVersion, ver)
	}

	// Header length (uint32 BE)
	var hLenBuf [headerLenLen]byte
	if _, err := io.ReadFull(r.file, hLenBuf[:]); err != nil {
		return fmt.Errorf("read header length: %w", err)
	}

	hLen := binary.BigEndian.Uint32(hLenBuf[:])

	// JSON header
	jsonBuf := make([]byte, hLen)
	if _, err := io.ReadFull(r.file, jsonBuf); err != nil {
		return fmt.Errorf("read header json: %w", err)
	}

	if err := json.Unmarshal(jsonBuf, &r.header); err != nil {
		return fmt.Errorf("unmarshal header: %w", err)
	}

	return nil
}

// Header returns the parsed JSON header.
func (r *Reader) Header() Header {
	return r.header
}

// ReadPacket reads the next packet from the dump.
// Returns io.EOF after the EOF marker.
func (r *Reader) ReadPacket() (*Packet, error) {
	var frameBuf [packetFrameSize]byte
	if _, err := io.ReadFull(r.file, frameBuf[:]); err != nil {
		return nil, fmt.Errorf("read packet frame: %w", err)
	}

	pkt := &Packet{
		RelativeNs: int64(binary.BigEndian.Uint64(frameBuf[:8])),
		Direction:  frameBuf[8],
	}

	length := binary.BigEndian.Uint32(frameBuf[9:13])

	if pkt.Direction == eofMarker {
		return nil, io.EOF
	}

	pkt.Data = make([]byte, length)
	if _, err := io.ReadFull(r.file, pkt.Data); err != nil {
		return nil, fmt.Errorf("read packet data: %w", err)
	}

	return pkt, nil
}

// Close closes the underlying file.
func (r *Reader) Close() error {
	return r.file.Close()
}
