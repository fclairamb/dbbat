package dump

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Anonymise reads a dump file and writes an anonymised copy.
// The anonymised dump preserves packet data but strips connection metadata
// from the header, keeping only the session ID and protocol.
func Anonymise(inputPath, outputPath string) error {
	reader, err := OpenReader(inputPath)
	if err != nil {
		return fmt.Errorf("open input: %w", err)
	}
	defer func() { _ = reader.Close() }()

	header := reader.Header()

	writer, err := NewWriter(outputPath, Header{
		SessionID:  header.SessionID,
		Protocol:   header.Protocol,
		Connection: map[string]any{},
	}, 0)
	if err != nil {
		return fmt.Errorf("create output: %w", err)
	}

	for {
		pkt, err := reader.ReadPacket()
		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			_ = writer.Close()

			return fmt.Errorf("read packet: %w", err)
		}

		var relBuf [8]byte
		binary.BigEndian.PutUint64(relBuf[:], uint64(pkt.RelativeNs))

		if err := writer.writePacketRaw(relBuf[:], pkt.Direction, pkt.Data); err != nil {
			_ = writer.Close()

			return fmt.Errorf("write packet: %w", err)
		}
	}

	return writer.Close()
}
