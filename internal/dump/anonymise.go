package dump

import (
	"errors"
	"fmt"
	"io"
	"time"
)

// Anonymise reads a capture and writes an anonymised copy.
//
// Packet payloads and their relative timing are preserved verbatim. The session
// metadata carried in the pcapng Section Header Block comment is reduced to the
// session ID and the protocol: the connection object (database, user, service
// name, upstream address…) is dropped, and the capture is rebased onto the Unix
// epoch so the wall-clock time of the session leaks nothing.
//
// When rewriteAddresses is true — the default for the CLI — the synthesized
// IPv4 addresses and TCP ports are re-generated from the fake endpoints too,
// since the capture's server-side addressing normally encodes the real upstream
// host and port. Pass false to keep the original addressing.
func Anonymise(inputPath, outputPath string, rewriteAddresses bool) error {
	reader, err := OpenReader(inputPath)
	if err != nil {
		return fmt.Errorf("open input: %w", err)
	}
	defer func() { _ = reader.Close() }()

	original := reader.Header()

	stripped := Header{
		SessionID:  original.SessionID,
		Protocol:   original.Protocol,
		StartTime:  time.Unix(0, 0).UTC(),
		Connection: map[string]any{},
	}

	ep := newEndpoints(original, false)
	if rewriteAddresses {
		ep = newEndpoints(stripped, true)
	}

	writer, err := newWriter(outputPath, stripped, 0, ep)
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

		if err := writer.writeAt(pkt.RelativeNs, pkt.Direction, pkt.Data); err != nil {
			_ = writer.Close()

			return fmt.Errorf("write packet: %w", err)
		}
	}

	if err := writer.Close(); err != nil {
		return fmt.Errorf("close output: %w", err)
	}

	return nil
}
