package oracle

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Dump file constants.
const (
	dumpMagic      = "DBBAT_DUMP\x00\x00\x00\x00\x00\x00" // 16 bytes
	dumpMagicSize  = 16
	dumpVersion    = 1
	dumpEOFMarker  = 0xFF
	dumpFileExt    = ".dbbat-dump"
	dumpMinMagic   = 10 // length of "DBBAT_DUMP"
	dumpUUIDSize   = 16
	dumpVersionLen = 2
	dumpTimeLen    = 8
)

// Dump packet direction constants.
const (
	DumpDirClientToServer byte = 0
	DumpDirServerToClient byte = 1
)

// DumpWriter writes TNS packet dumps to a binary file.
type DumpWriter struct {
	file      *os.File
	startTime time.Time
	maxSize   int64
	written   int64
	mu        sync.Mutex
}

// NewDumpWriter creates a new dump file and writes the header.
func NewDumpWriter(path string, sessionUID uuid.UUID, serviceName, upstreamAddr string, maxSize int64) (*DumpWriter, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, fmt.Errorf("create dump file: %w", err)
	}

	now := time.Now()
	w := &DumpWriter{
		file:      f,
		startTime: now,
		maxSize:   maxSize,
	}

	if err := w.writeHeader(sessionUID, serviceName, upstreamAddr, now); err != nil {
		_ = f.Close()
		_ = os.Remove(path)

		return nil, err
	}

	return w, nil
}

func (w *DumpWriter) writeHeader(sessionUID uuid.UUID, serviceName, upstreamAddr string, startTime time.Time) error {
	// Magic (16 bytes)
	if _, err := w.file.Write([]byte(dumpMagic)); err != nil {
		return fmt.Errorf("write magic: %w", err)
	}

	// Version (uint16 BE)
	var buf [8]byte
	binary.BigEndian.PutUint16(buf[:2], dumpVersion)

	if _, err := w.file.Write(buf[:2]); err != nil {
		return fmt.Errorf("write version: %w", err)
	}

	// Session UID (16 bytes)
	uidBytes := sessionUID[:]
	if _, err := w.file.Write(uidBytes); err != nil {
		return fmt.Errorf("write session uid: %w", err)
	}

	// Service name (length-prefixed)
	svcBytes := []byte(serviceName)
	if len(svcBytes) > 255 {
		svcBytes = svcBytes[:255]
	}

	if _, err := w.file.Write([]byte{byte(len(svcBytes))}); err != nil {
		return fmt.Errorf("write service len: %w", err)
	}

	if _, err := w.file.Write(svcBytes); err != nil {
		return fmt.Errorf("write service name: %w", err)
	}

	// Upstream addr (length-prefixed)
	upBytes := []byte(upstreamAddr)
	if len(upBytes) > 255 {
		upBytes = upBytes[:255]
	}

	if _, err := w.file.Write([]byte{byte(len(upBytes))}); err != nil {
		return fmt.Errorf("write upstream len: %w", err)
	}

	if _, err := w.file.Write(upBytes); err != nil {
		return fmt.Errorf("write upstream addr: %w", err)
	}

	// Start time (int64 BE, unix nanoseconds)
	binary.BigEndian.PutUint64(buf[:8], uint64(startTime.UnixNano()))

	if _, err := w.file.Write(buf[:8]); err != nil {
		return fmt.Errorf("write start time: %w", err)
	}

	// Track written bytes for max size enforcement
	n, _ := w.file.Seek(0, io.SeekCurrent)
	w.written = n

	return nil
}

// WritePacket writes a single TNS packet to the dump file.
// direction: 0 = client->upstream, 1 = upstream->client
func (w *DumpWriter) WritePacket(direction byte, data []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	// Check max size (13 = 8 bytes relativeNs + 1 direction + 4 length)
	packetFrameSize := int64(13 + len(data))
	if w.maxSize > 0 && w.written+packetFrameSize > w.maxSize {
		return nil // silently skip
	}

	var buf [13]byte

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

	w.written += packetFrameSize

	return nil
}

// Close writes the EOF marker and closes the file.
func (w *DumpWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	// EOF marker: relativeNs + direction=0xFF + length=0
	var buf [13]byte

	relNs := time.Since(w.startTime).Nanoseconds()
	binary.BigEndian.PutUint64(buf[:8], uint64(relNs))

	buf[8] = dumpEOFMarker
	binary.BigEndian.PutUint32(buf[9:13], 0)

	_, _ = w.file.Write(buf[:])

	return w.file.Close()
}

// DumpHeader holds metadata from a dump file header.
type DumpHeader struct {
	Version      uint16
	SessionUID   uuid.UUID
	ServiceName  string
	UpstreamAddr string
	StartTime    time.Time
}

// DumpPacket represents a single captured packet.
type DumpPacket struct {
	RelativeNs int64
	Direction  byte // 0=C->S, 1=S->C, 0xFF=EOF
	Data       []byte
}

// DumpReader reads packets from a dump file.
type DumpReader struct {
	file   *os.File
	Header DumpHeader
}

// Dump reader errors.
var (
	ErrInvalidDumpMagic = errors.New("invalid dump file magic")
)

// OpenDump opens a dump file for reading.
func OpenDump(path string) (*DumpReader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open dump file: %w", err)
	}

	r := &DumpReader{file: f}

	if err := r.readHeader(); err != nil {
		_ = f.Close()

		return nil, err
	}

	return r, nil
}

func (r *DumpReader) readHeader() error {
	// Magic
	magic := make([]byte, dumpMagicSize)
	if _, err := io.ReadFull(r.file, magic); err != nil {
		return fmt.Errorf("read magic: %w", err)
	}

	if string(magic[:dumpMinMagic]) != dumpMagic[:dumpMinMagic] {
		return ErrInvalidDumpMagic
	}

	// Version
	var vBuf [2]byte
	if _, err := io.ReadFull(r.file, vBuf[:]); err != nil {
		return fmt.Errorf("read version: %w", err)
	}

	r.Header.Version = binary.BigEndian.Uint16(vBuf[:])

	// Session UID
	var uidBuf [dumpUUIDSize]byte
	if _, err := io.ReadFull(r.file, uidBuf[:]); err != nil {
		return fmt.Errorf("read session uid: %w", err)
	}

	r.Header.SessionUID = uuid.UUID(uidBuf)

	// Service name
	var svcLenBuf [1]byte
	if _, err := io.ReadFull(r.file, svcLenBuf[:]); err != nil {
		return fmt.Errorf("read service len: %w", err)
	}

	svcBuf := make([]byte, svcLenBuf[0])
	if _, err := io.ReadFull(r.file, svcBuf); err != nil {
		return fmt.Errorf("read service name: %w", err)
	}

	r.Header.ServiceName = string(svcBuf)

	// Upstream addr
	var upLenBuf [1]byte
	if _, err := io.ReadFull(r.file, upLenBuf[:]); err != nil {
		return fmt.Errorf("read upstream len: %w", err)
	}

	upBuf := make([]byte, upLenBuf[0])
	if _, err := io.ReadFull(r.file, upBuf); err != nil {
		return fmt.Errorf("read upstream addr: %w", err)
	}

	r.Header.UpstreamAddr = string(upBuf)

	// Start time
	var timeBuf [dumpTimeLen]byte
	if _, err := io.ReadFull(r.file, timeBuf[:]); err != nil {
		return fmt.Errorf("read start time: %w", err)
	}

	r.Header.StartTime = time.Unix(0, int64(binary.BigEndian.Uint64(timeBuf[:])))

	return nil
}

// ReadPacket reads the next packet from the dump. Returns io.EOF when the EOF marker is reached.
func (r *DumpReader) ReadPacket() (*DumpPacket, error) {
	var frameBuf [13]byte
	if _, err := io.ReadFull(r.file, frameBuf[:]); err != nil {
		return nil, fmt.Errorf("read packet frame: %w", err)
	}

	pkt := &DumpPacket{
		RelativeNs: int64(binary.BigEndian.Uint64(frameBuf[:8])),
		Direction:  frameBuf[8],
	}

	length := binary.BigEndian.Uint32(frameBuf[9:13])

	if pkt.Direction == dumpEOFMarker {
		return nil, io.EOF
	}

	pkt.Data = make([]byte, length)
	if _, err := io.ReadFull(r.file, pkt.Data); err != nil {
		return nil, fmt.Errorf("read packet data: %w", err)
	}

	return pkt, nil
}

// Close closes the dump reader.
func (r *DumpReader) Close() error {
	return r.file.Close()
}

// CleanupOldDumps deletes dump files older than the retention period.
// Returns the number of files deleted.
func CleanupOldDumps(dir string, retention time.Duration) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, fmt.Errorf("read dump dir: %w", err)
	}

	cutoff := time.Now().Add(-retention)
	deleted := 0

	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != dumpFileExt {
			continue
		}

		info, err := e.Info()
		if err != nil {
			continue
		}

		if info.ModTime().Before(cutoff) {
			if err := os.Remove(filepath.Join(dir, e.Name())); err == nil {
				deleted++
			}
		}
	}

	return deleted, nil
}
