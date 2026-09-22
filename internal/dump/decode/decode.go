// Package decode turns a dbbat session capture into a readable protocol trace:
// one line per protocol message, both directions, with the offset from the
// start of the session.
//
//	234ms C> Execute portal="" maxRows=501
//	251ms <S RowDescription(8 fields)
//	329ms <S PortalSuspended
//	329ms <S ReadyForQuery I
//
// The capture is plain pcapng and Wireshark reads it, but Wireshark is not on a
// build host and not in a terminal-driven investigation. This package is that
// same view without a GUI.
//
// Captures may hold customer data, so a decoded line is redacted by default:
// result-row values and bind parameters collapse to their count. Options.ShowRows
// opts into the values. Authentication payloads are never printed, with or
// without that flag.
package decode

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/fclairamb/dbbat/internal/dump"
)

// ErrUnsupportedProtocol is returned for a capture whose protocol has no
// decoder yet. PostgreSQL is the only one implemented; the other four have
// frame parsers in their proxies and follow.
var ErrUnsupportedProtocol = errors.New("no message decoder for this capture's protocol")

// ErrOutOfSync is returned when a stream cannot be cut at the protocol's
// framing any more. The usual cause is a capture truncated by
// DBB_DUMP_MAX_SIZE, which drops whole packets mid-session.
var ErrOutOfSync = errors.New("protocol stream out of sync")

// Direction markers, chosen so the two directions are distinguishable at a
// glance in a column of otherwise similar lines.
const (
	markerClientToServer = "C>"
	markerServerToClient = "<S"
)

// Options controls how much a decoded line reveals.
type Options struct {
	// ShowRows prints result-row values, bind parameter values and column names
	// instead of their counts. Authentication payloads stay redacted either way.
	ShowRows bool
}

// Message is one decoded protocol message.
type Message struct {
	// RelativeNs is the offset from the session start of the packet that
	// *completed* the message. A message split across packets is timed by its
	// last byte, which is when the peer could act on it.
	RelativeNs int64
	Direction  byte
	Text       string
}

// String renders the message as one trace line.
func (m Message) String() string {
	return FormatOffset(m.RelativeNs) + " " + DirectionMarker(m.Direction) + " " + m.Text
}

// DirectionMarker returns the two-character marker for a packet direction.
func DirectionMarker(direction byte) string {
	if direction == dump.DirServerToClient {
		return markerServerToClient
	}

	return markerClientToServer
}

// FormatOffset renders a nanosecond offset as a compact duration: 234ms,
// 1.234s, 2m3.456s. Truncated to the millisecond, which is the resolution
// anything in a session trace is actually read at.
func FormatOffset(relativeNs int64) string {
	return time.Duration(relativeNs).Truncate(time.Millisecond).String()
}

// splitter cuts one protocol's byte streams into messages. Each protocol
// implements one; Feed is called with the capture's packets in order.
type splitter interface {
	// Feed consumes one packet and returns the messages it completed. A packet
	// may complete none (a partial message) or several (a batched flush).
	Feed(packet *dump.Packet) ([]Message, error)
}

// newSplitter picks the decoder for a capture's protocol. The protocol comes
// from the capture header, never from sniffing the bytes.
func newSplitter(protocol string, opts Options) (splitter, error) {
	if protocol == dump.ProtocolPostgreSQL {
		return newPostgresSplitter(opts), nil
	}

	return nil, fmt.Errorf("%w: %s", ErrUnsupportedProtocol, protocol)
}

// Supported reports whether a capture of the given protocol can be decoded.
func Supported(protocol string) bool {
	return protocol == dump.ProtocolPostgreSQL
}

// File decodes the capture at path and writes the trace to out.
//
// The first line names the session and protocol; every following line is one
// protocol message. Lines produced before a decoding failure are written out
// before the error is returned, so a truncated capture still yields everything
// that was readable.
func File(path string, opts Options, out io.Writer) error {
	reader, err := dump.OpenReader(path)
	if err != nil {
		return fmt.Errorf("open capture: %w", err)
	}

	defer func() { _ = reader.Close() }()

	header := reader.Header()

	split, err := newSplitter(header.Protocol, opts)
	if err != nil {
		return err
	}

	if _, err := fmt.Fprintf(out, "# %s session %s\n", header.Protocol, header.SessionID); err != nil {
		return fmt.Errorf("write trace: %w", err)
	}

	for packets := 0; ; packets++ {
		packet, err := reader.ReadPacket()
		if errors.Is(err, io.EOF) {
			return nil
		}

		if err != nil {
			return fmt.Errorf("read packet: %w", err)
		}

		messages, feedErr := split.Feed(packet)

		if err := writeMessages(out, messages); err != nil {
			return err
		}

		if feedErr != nil {
			return fmt.Errorf("packet %d at %s: %w", packets, FormatOffset(packet.RelativeNs), feedErr)
		}
	}
}

func writeMessages(out io.Writer, messages []Message) error {
	for _, msg := range messages {
		if _, err := io.WriteString(out, msg.String()+"\n"); err != nil {
			return fmt.Errorf("write trace: %w", err)
		}
	}

	return nil
}

// collapse folds a statement's whitespace onto one line: a trace is one message
// per line, and a multi-line statement would break that.
func collapse(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// quote renders a string as a Go-quoted literal, which is how a trace shows an
// empty portal name ("") as something rather than as nothing.
func quote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(collapse(s)) + `"`
}
