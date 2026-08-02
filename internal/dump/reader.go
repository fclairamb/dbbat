package dump

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/gopacket/gopacket"
	"github.com/gopacket/gopacket/layers"
	"github.com/gopacket/gopacket/pcapgo"
)

// Reader reads application payloads back out of a pcapng capture, undoing the
// synthesized Ethernet/IPv4/TCP wrapping applied by Writer.
type Reader struct {
	file   *os.File
	ng     *pcapgo.NgReader
	header Header
	parser *gopacket.DecodingLayerParser
	eth    layers.Ethernet
	ip4    layers.IPv4
	tcp    layers.TCP
	decode []gopacket.LayerType
}

// OpenReader opens a capture file and parses the session metadata carried in
// the pcapng Section Header Block comment.
func OpenReader(path string) (*Reader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open capture file: %w", err)
	}

	ng, err := pcapgo.NewNgReader(f, pcapgo.NgReaderOptions{SkipUnknownVersion: true})
	if err != nil {
		_ = f.Close()

		return nil, fmt.Errorf("read pcapng header: %w", err)
	}

	r := &Reader{file: f, ng: ng}

	comment := ng.SectionInfo().Comment
	if comment == "" {
		_ = f.Close()

		return nil, ErrMissingMetadata
	}

	if err := json.Unmarshal([]byte(comment), &r.header); err != nil {
		_ = f.Close()

		return nil, fmt.Errorf("%w: %w", ErrMissingMetadata, err)
	}

	r.parser = gopacket.NewDecodingLayerParser(layers.LayerTypeEthernet, &r.eth, &r.ip4, &r.tcp)
	r.parser.IgnoreUnsupported = true

	return r, nil
}

// Header returns the session metadata.
func (r *Reader) Header() Header {
	return r.header
}

// ReadPacket returns the next application payload. Frames without a TCP payload
// are skipped. Returns io.EOF at the end of the capture.
func (r *Reader) ReadPacket() (*Packet, error) {
	for {
		data, ci, opts, err := r.ng.ReadPacketDataWithOptions()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil, io.EOF
			}

			return nil, fmt.Errorf("read packet block: %w", err)
		}

		r.decode = r.decode[:0]
		if err := r.parser.DecodeLayers(data, &r.decode); err != nil {
			// Frames dbbat did not synthesize (or truncated ones) are skipped
			// rather than aborting the whole capture.
			continue
		}

		if !hasLayer(r.decode, layers.LayerTypeTCP) || len(r.tcp.Payload) == 0 {
			continue
		}

		payload := make([]byte, len(r.tcp.Payload))
		copy(payload, r.tcp.Payload)

		return &Packet{
			RelativeNs: ci.Timestamp.Sub(r.header.StartTime).Nanoseconds(),
			Direction:  r.direction(opts),
			Data:       payload,
		}, nil
	}
}

// direction resolves the packet direction from the epb_flags option, falling
// back to the synthesized addressing when the option is absent.
func (r *Reader) direction(opts pcapgo.NgPacketOptions) byte {
	if opts.Flags != nil {
		switch opts.Flags.Direction {
		case pcapgo.NgEpbFlagDirectionInbound:
			return DirClientToServer
		case pcapgo.NgEpbFlagDirectionOutbound:
			return DirServerToClient
		}
	}

	if r.tcp.SrcPort == fakeClientPort {
		return DirClientToServer
	}

	return DirServerToClient
}

// Close closes the underlying file.
func (r *Reader) Close() error {
	if err := r.file.Close(); err != nil {
		return fmt.Errorf("close capture file: %w", err)
	}

	return nil
}

func hasLayer(decoded []gopacket.LayerType, want gopacket.LayerType) bool {
	for _, lt := range decoded {
		if lt == want {
			return true
		}
	}

	return false
}
