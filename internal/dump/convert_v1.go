package dump

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"time"
)

// V1Header holds metadata from a v1 dump file header.
type V1Header struct {
	Version      uint16
	SessionUID   [16]byte
	ServiceName  string
	UpstreamAddr string
	StartTime    time.Time
}

// ConvertV1ToV2 reads a v1 dump file and writes a v2 dump file.
func ConvertV1ToV2(srcPath, dstPath string) error {
	f, err := os.Open(srcPath)
	if err != nil {
		return fmt.Errorf("open source: %w", err)
	}
	defer func() { _ = f.Close() }()

	hdr, err := readV1Header(f)
	if err != nil {
		return fmt.Errorf("read v1 header: %w", err)
	}

	// Format UUID from raw bytes
	uid := hdr.SessionUID
	sessionID := fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		uid[0:4], uid[4:6], uid[6:8], uid[8:10], uid[10:16])

	w, err := NewWriter(dstPath, Header{
		SessionID: sessionID,
		Protocol:  ProtocolOracle,
		StartTime: hdr.StartTime,
		Connection: map[string]any{
			"service_name":  hdr.ServiceName,
			"upstream_addr": hdr.UpstreamAddr,
		},
	}, 0)
	if err != nil {
		return fmt.Errorf("create v2 writer: %w", err)
	}

	// Copy packets
	for {
		var frameBuf [13]byte
		if _, err := io.ReadFull(f, frameBuf[:]); err != nil {
			_ = w.Close()
			return fmt.Errorf("read v1 packet frame: %w", err)
		}

		direction := frameBuf[8]
		length := binary.BigEndian.Uint32(frameBuf[9:13])

		if direction == 0xFF {
			// EOF marker
			break
		}

		data := make([]byte, length)
		if _, err := io.ReadFull(f, data); err != nil {
			_ = w.Close()
			return fmt.Errorf("read v1 packet data: %w", err)
		}

		// Write using raw frame to preserve original timestamps
		if err := w.writePacketRaw(frameBuf[:8], direction, data); err != nil {
			_ = w.Close()
			return fmt.Errorf("write v2 packet: %w", err)
		}
	}

	return w.Close()
}

// writePacketRaw writes a packet with a pre-encoded timestamp (for conversion).
func (w *Writer) writePacketRaw(relativeNsBuf []byte, direction byte, data []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	frameTotal := int64(packetFrameSize + len(data))

	var buf [packetFrameSize]byte
	copy(buf[:8], relativeNsBuf)
	buf[8] = direction
	binary.BigEndian.PutUint32(buf[9:13], uint32(len(data)))

	if _, err := w.file.Write(buf[:]); err != nil {
		return fmt.Errorf("write packet frame: %w", err)
	}

	if _, err := w.file.Write(data); err != nil {
		return fmt.Errorf("write packet data: %w", err)
	}

	w.written += frameTotal

	return nil
}

func readV1Header(f *os.File) (*V1Header, error) {
	// Magic (16 bytes)
	magicBuf := make([]byte, magicSize)
	if _, err := io.ReadFull(f, magicBuf); err != nil {
		return nil, fmt.Errorf("read magic: %w", err)
	}

	if string(magicBuf[:minMagic]) != magic[:minMagic] {
		return nil, ErrInvalidMagic
	}

	hdr := &V1Header{}

	// Version (uint16 BE)
	var vBuf [2]byte
	if _, err := io.ReadFull(f, vBuf[:]); err != nil {
		return nil, fmt.Errorf("read version: %w", err)
	}

	hdr.Version = binary.BigEndian.Uint16(vBuf[:])

	// Session UID (16 bytes)
	if _, err := io.ReadFull(f, hdr.SessionUID[:]); err != nil {
		return nil, fmt.Errorf("read session uid: %w", err)
	}

	// Service name (length-prefixed)
	var svcLen [1]byte
	if _, err := io.ReadFull(f, svcLen[:]); err != nil {
		return nil, fmt.Errorf("read service len: %w", err)
	}

	svcBuf := make([]byte, svcLen[0])
	if _, err := io.ReadFull(f, svcBuf); err != nil {
		return nil, fmt.Errorf("read service name: %w", err)
	}

	hdr.ServiceName = string(svcBuf)

	// Upstream addr (length-prefixed)
	var upLen [1]byte
	if _, err := io.ReadFull(f, upLen[:]); err != nil {
		return nil, fmt.Errorf("read upstream len: %w", err)
	}

	upBuf := make([]byte, upLen[0])
	if _, err := io.ReadFull(f, upBuf); err != nil {
		return nil, fmt.Errorf("read upstream addr: %w", err)
	}

	hdr.UpstreamAddr = string(upBuf)

	// Start time (int64 BE, unix nanoseconds)
	var timeBuf [8]byte
	if _, err := io.ReadFull(f, timeBuf[:]); err != nil {
		return nil, fmt.Errorf("read start time: %w", err)
	}

	hdr.StartTime = time.Unix(0, int64(binary.BigEndian.Uint64(timeBuf[:])))

	return hdr, nil
}
