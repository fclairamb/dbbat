package dump

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/gopacket/gopacket"
	"github.com/gopacket/gopacket/layers"
	"github.com/gopacket/gopacket/pcapgo"
)

// application is advertised in the Section Header Block shb_userappl option.
const application = "dbbat"

// epbOverhead is the fixed pcapng cost of one Enhanced Packet Block:
// 8 (block type + length) + 20 (interface, timestamp, capture/original length)
// + 8 (epb_flags option) + 4 (end-of-options) + 4 (trailing block length).
const epbOverhead = 44

// directionFlags maps a dump direction to the pcapng epb_flags direction bits.
// Traffic is described from the proxy's point of view: what dbbat receives from
// the client is inbound, what it receives from the upstream server is outbound.
func directionFlags(direction byte) *pcapgo.NgEpbFlags {
	dir := pcapgo.NgEpbFlagDirectionInbound
	if direction == DirServerToClient {
		dir = pcapgo.NgEpbFlagDirectionOutbound
	}

	return &pcapgo.NgEpbFlags{Direction: dir}
}

// countingWriter counts every byte handed to the underlying writer.
type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)

	if err != nil {
		return n, fmt.Errorf("write capture: %w", err)
	}

	return n, nil
}

// Writer writes a session capture as a pcapng file. Application payloads are
// wrapped in synthesized Ethernet/IPv4/TCP headers (see synth.go) and the
// session metadata is stored as a JSON blob in the Section Header Block comment.
type Writer struct {
	file      *os.File
	counter   *countingWriter
	ng        *pcapgo.NgWriter
	framer    *framer
	startTime time.Time
	maxSize   int64
	mu        sync.Mutex
}

// NewWriter creates a new capture file and writes the pcapng section header
// (carrying the session metadata) and interface description.
func NewWriter(path string, header Header, maxSize int64) (*Writer, error) {
	return newWriter(path, header, maxSize, newEndpoints(header, false))
}

func newWriter(path string, header Header, maxSize int64, ep endpoints) (*Writer, error) {
	// pcapng timestamps are unsigned microseconds/nanoseconds since the Unix
	// epoch: a zero or pre-epoch start time would not round-trip. Normalise it
	// (and record the normalised value in the metadata) so the capture stays
	// readable by standard tooling.
	if header.StartTime.Before(time.Unix(0, 0)) {
		header.StartTime = time.Now()
	}

	metadata, err := json.Marshal(header)
	if err != nil {
		return nil, fmt.Errorf("marshal session metadata: %w", err)
	}

	f, err := os.Create(path)
	if err != nil {
		return nil, fmt.Errorf("create capture file: %w", err)
	}

	counter := &countingWriter{w: f}

	intfName := header.Protocol
	if intfName == "" {
		intfName = application
	}

	ng, err := pcapgo.NewNgWriterInterface(counter, pcapgo.NgInterface{
		Name:                intfName,
		Description:         "dbbat proxied session (synthesized TCP/IP headers)",
		LinkType:            layers.LinkTypeEthernet,
		SnapLength:          snapLength,
		TimestampResolution: 9,
	}, pcapgo.NgWriterOptions{SectionInfo: pcapgo.NgSectionInfo{
		Application: application,
		Comment:     string(metadata),
	}})
	if err != nil {
		_ = f.Close()
		_ = os.Remove(path)

		return nil, fmt.Errorf("write pcapng header: %w", err)
	}

	if err := ng.Flush(); err != nil {
		_ = f.Close()
		_ = os.Remove(path)

		return nil, fmt.Errorf("flush pcapng header: %w", err)
	}

	return &Writer{
		file:      f,
		counter:   counter,
		ng:        ng,
		framer:    newFramer(ep),
		startTime: header.StartTime,
		maxSize:   maxSize,
	}, nil
}

// WritePacket appends a single application payload to the capture. Thread-safe.
// Silently skips the packet if maxSize would be exceeded.
func (w *Writer) WritePacket(direction byte, data []byte) error {
	return w.writeAt(time.Since(w.startTime).Nanoseconds(), direction, data)
}

// writeAt appends a payload with an explicit offset from the session start.
func (w *Writer) writeAt(relativeNs int64, direction byte, data []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	frames, err := w.framer.frames(direction, data)
	if err != nil {
		return err
	}

	total := int64(0)
	for _, frame := range frames {
		total += int64(epbOverhead + len(frame) + (4-len(frame)&3)&3)
	}

	if w.maxSize > 0 && w.counter.n+total > w.maxSize {
		return nil // silently skip
	}

	timestamp := w.startTime.Add(time.Duration(relativeNs))
	flags := directionFlags(direction)

	for _, frame := range frames {
		ci := gopacket.CaptureInfo{
			Timestamp:      timestamp,
			CaptureLength:  len(frame),
			Length:         len(frame),
			InterfaceIndex: 0,
		}

		if err := w.ng.WritePacketWithOptions(ci, frame, pcapgo.NgPacketOptions{Flags: flags}); err != nil {
			return fmt.Errorf("write packet block: %w", err)
		}
	}

	if err := w.ng.Flush(); err != nil {
		return fmt.Errorf("flush capture: %w", err)
	}

	return nil
}

// Close flushes any buffered block and closes the file.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if err := w.ng.Flush(); err != nil {
		_ = w.file.Close()

		return fmt.Errorf("flush capture: %w", err)
	}

	if err := w.file.Close(); err != nil {
		return fmt.Errorf("close capture file: %w", err)
	}

	return nil
}
