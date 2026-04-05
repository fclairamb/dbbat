package dump

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// Writer writes packet dumps to a binary file.
type Writer struct {
	file      *os.File
	startTime time.Time
	maxSize   int64
	written   int64
	mu        sync.Mutex
}

// NewWriter creates a new dump file and writes the file header + JSON header.
func NewWriter(path string, header Header, maxSize int64) (*Writer, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, fmt.Errorf("create dump file: %w", err)
	}

	w := &Writer{
		file:      f,
		startTime: header.StartTime,
		maxSize:   maxSize,
	}

	if err := w.writeHeader(header); err != nil {
		_ = f.Close()
		_ = os.Remove(path)

		return nil, err
	}

	return w, nil
}

func (w *Writer) writeHeader(header Header) error {
	// Magic (16 bytes)
	if _, err := w.file.Write([]byte(magic)); err != nil {
		return fmt.Errorf("write magic: %w", err)
	}

	// Version (uint16 BE)
	var vBuf [versionLen]byte
	binary.BigEndian.PutUint16(vBuf[:], version)

	if _, err := w.file.Write(vBuf[:]); err != nil {
		return fmt.Errorf("write version: %w", err)
	}

	// JSON header
	jsonData, err := json.Marshal(header)
	if err != nil {
		return fmt.Errorf("marshal header: %w", err)
	}

	// Header length (uint32 BE)
	var hLenBuf [headerLenLen]byte
	binary.BigEndian.PutUint32(hLenBuf[:], uint32(len(jsonData)))

	if _, err := w.file.Write(hLenBuf[:]); err != nil {
		return fmt.Errorf("write header length: %w", err)
	}

	// JSON data
	if _, err := w.file.Write(jsonData); err != nil {
		return fmt.Errorf("write header json: %w", err)
	}

	// Track written bytes for max size enforcement
	n, _ := w.file.Seek(0, io.SeekCurrent)
	w.written = n

	return nil
}

// WritePacket appends a single packet to the dump file. Thread-safe.
// Silently skips if maxSize would be exceeded.
func (w *Writer) WritePacket(direction byte, data []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	frameTotal := int64(packetFrameSize + len(data))
	if w.maxSize > 0 && w.written+frameTotal > w.maxSize {
		return nil // silently skip
	}

	var buf [packetFrameSize]byte

	// RelativeNs (int64 BE)
	relNs := time.Since(w.startTime).Nanoseconds()
	binary.BigEndian.PutUint64(buf[:8], uint64(relNs))

	// Direction (uint8)
	buf[8] = direction

	// Length (uint32 BE)
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

// Close writes the EOF marker and closes the file.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	var buf [packetFrameSize]byte

	relNs := time.Since(w.startTime).Nanoseconds()
	binary.BigEndian.PutUint64(buf[:8], uint64(relNs))

	buf[8] = eofMarker
	binary.BigEndian.PutUint32(buf[9:13], 0)

	_, _ = w.file.Write(buf[:])

	return w.file.Close()
}
