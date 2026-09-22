package decode

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/fclairamb/dbbat/internal/dump"
)

// PostgreSQL framing constants.
const (
	// pgTypedHeaderLen is one message-type byte plus an int32 length that
	// counts itself but not the type byte.
	pgTypedHeaderLen = 5

	// pgLengthLen is the int32 length prefix on its own, which is all the
	// untyped startup packets carry.
	pgLengthLen = 4

	// pgMinStartupLen / pgMaxStartupLen mirror what pgproto3 (and the server)
	// accept for an untyped startup packet, so a desynchronised stream is
	// caught here rather than producing an absurd allocation.
	pgMinStartupLen = 8
	pgMaxStartupLen = 10004
)

// postgresSplitter cuts both directions of a PostgreSQL session into messages.
//
// Each direction is buffered independently because a TCP payload is not a
// message: one packet may carry several messages, and one message may span
// several packets. Framing is done here — one type byte plus an int32 length,
// with the untyped StartupMessage/SSLRequest and the one-byte SSL answer
// handled first — and only a complete message is ever handed to pgproto3, so a
// Receive never runs off the end of what has been read.
type postgresSplitter struct {
	opts Options

	clientBuf []byte
	serverBuf []byte

	// clientFeed/serverFeed hold the complete messages handed to pgproto3.
	// pgproto3 wants an io.Reader; a bytes.Buffer returns io.EOF when drained,
	// which never happens mid-message because only whole messages are written.
	clientFeed *bytes.Buffer
	serverFeed *bytes.Buffer

	backend  *pgproto3.Backend  // decodes what the client sends
	frontend *pgproto3.Frontend // decodes what the server sends

	// startupDone marks the client stream as having reached the typed messages.
	// Until then every client message is an untyped startup packet.
	startupDone bool

	// pendingSSLReplies counts the one-byte answers the server still owes for
	// SSLRequest/GSSEncRequest packets the client has sent.
	pendingSSLReplies int

	// encrypted is set once the server accepted a TLS upgrade inside the
	// capture: everything after that is TLS records, not protocol messages.
	// dbbat's own taps sit above TLS so this should not happen on a dbbat
	// capture, but a capture made any other way is still read honestly.
	encrypted bool
}

func newPostgresSplitter(opts Options) *postgresSplitter {
	clientFeed := &bytes.Buffer{}
	serverFeed := &bytes.Buffer{}

	return &postgresSplitter{
		opts:       opts,
		clientFeed: clientFeed,
		serverFeed: serverFeed,
		// Nothing is ever sent: the write side exists only because pgproto3's
		// constructors take one.
		backend:  pgproto3.NewBackend(clientFeed, io.Discard),
		frontend: pgproto3.NewFrontend(serverFeed, io.Discard),
	}
}

// Feed consumes one packet and returns the messages it completed.
func (p *postgresSplitter) Feed(packet *dump.Packet) ([]Message, error) {
	if p.encrypted {
		return nil, nil
	}

	if packet.Direction == dump.DirServerToClient {
		p.serverBuf = append(p.serverBuf, packet.Data...)

		return p.drainServer(packet.RelativeNs)
	}

	p.clientBuf = append(p.clientBuf, packet.Data...)

	return p.drainClient(packet.RelativeNs)
}

// drainClient cuts every complete message out of the client buffer.
func (p *postgresSplitter) drainClient(relativeNs int64) ([]Message, error) {
	var out []Message

	for {
		if !p.startupDone {
			frame, ok, err := cutUntyped(&p.clientBuf)
			if err != nil {
				return out, err
			}

			if !ok {
				return out, nil
			}

			p.clientFeed.Write(frame)

			msg, err := p.backend.ReceiveStartupMessage()
			if err != nil {
				return out, fmt.Errorf("decode startup packet: %w", err)
			}

			p.noteStartup(msg)
			out = append(out, Message{relativeNs, dump.DirClientToServer, formatFrontend(msg, p.opts)})

			continue
		}

		frame, ok, err := cutTyped(&p.clientBuf)
		if err != nil {
			return out, err
		}

		if !ok {
			return out, nil
		}

		p.clientFeed.Write(frame)

		msg, err := p.backend.Receive()
		if err != nil {
			return out, fmt.Errorf("decode client message: %w", err)
		}

		out = append(out, Message{relativeNs, dump.DirClientToServer, formatFrontend(msg, p.opts)})
	}
}

// noteStartup records what an untyped client packet implies for the rest of the
// stream: an SSL/GSS request is answered with a bare byte and is followed by a
// second startup packet, anything else opens the typed stream.
func (p *postgresSplitter) noteStartup(msg pgproto3.FrontendMessage) {
	switch msg.(type) {
	case *pgproto3.SSLRequest, *pgproto3.GSSEncRequest:
		p.pendingSSLReplies++
	case *pgproto3.CancelRequest:
		// A cancel connection carries nothing else; the socket closes next.
	default:
		p.startupDone = true
	}
}

// drainServer cuts every complete message out of the server buffer.
func (p *postgresSplitter) drainServer(relativeNs int64) ([]Message, error) {
	var out []Message

	for {
		if p.pendingSSLReplies > 0 {
			if len(p.serverBuf) < 1 {
				return out, nil
			}

			answer := p.serverBuf[0]
			p.serverBuf = p.serverBuf[1:]
			p.pendingSSLReplies--

			out = append(out, Message{relativeNs, dump.DirServerToClient, formatSSLAnswer(answer)})

			if answer == 'S' || answer == 'G' {
				// The upgrade was accepted: the rest of this capture is TLS
				// records and cannot be decoded.
				p.encrypted = true

				return out, nil
			}

			continue
		}

		frame, ok, err := cutTyped(&p.serverBuf)
		if err != nil {
			return out, err
		}

		if !ok {
			return out, nil
		}

		p.serverFeed.Write(frame)

		msg, err := p.frontend.Receive()
		if err != nil {
			return out, fmt.Errorf("decode server message: %w", err)
		}

		// The client's next 'p' message is only decodable once the backend
		// knows which authentication exchange it belongs to.
		if authType, ok := authTypeOf(msg); ok {
			_ = p.backend.SetAuthType(authType)
		}

		out = append(out, Message{relativeNs, dump.DirServerToClient, formatBackend(msg, p.opts)})
	}
}

// cutUntyped cuts one untyped startup packet (an int32 length that counts
// itself, then the body) off the front of buf. ok is false when the packet is
// not fully buffered yet.
func cutUntyped(buf *[]byte) ([]byte, bool, error) {
	if len(*buf) < pgLengthLen {
		return nil, false, nil
	}

	length := int(int32(binary.BigEndian.Uint32((*buf)[:pgLengthLen])))
	if length < pgMinStartupLen || length > pgMaxStartupLen {
		return nil, false, fmt.Errorf("%w: startup packet length %d", ErrOutOfSync, length)
	}

	if len(*buf) < length {
		return nil, false, nil
	}

	frame := (*buf)[:length]
	*buf = (*buf)[length:]

	return frame, true, nil
}

// cutTyped cuts one typed message (a type byte, then an int32 length that
// counts itself but not the type byte, then the body) off the front of buf.
func cutTyped(buf *[]byte) ([]byte, bool, error) {
	if len(*buf) < pgTypedHeaderLen {
		return nil, false, nil
	}

	length := int(int32(binary.BigEndian.Uint32((*buf)[1:pgTypedHeaderLen])))
	if length < pgLengthLen {
		return nil, false, fmt.Errorf("%w: message length %d", ErrOutOfSync, length)
	}

	total := length + 1
	if len(*buf) < total {
		return nil, false, nil
	}

	frame := (*buf)[:total]
	*buf = (*buf)[total:]

	return frame, true, nil
}

// formatSSLAnswer names the server's one-byte answer to an SSL/GSS request.
func formatSSLAnswer(answer byte) string {
	switch answer {
	case 'S':
		return "SSLResponse S (accepted, rest of the capture is TLS)"
	case 'N':
		return "SSLResponse N (refused)"
	case 'G':
		return "GSSEncResponse G (accepted, rest of the capture is encrypted)"
	default:
		return fmt.Sprintf("SSLResponse %q", rune(answer))
	}
}

// authTypeOf maps a decoded authentication message back to its wire code, which
// is what pgproto3's Backend needs to decode the client's reply.
func authTypeOf(msg pgproto3.BackendMessage) (uint32, bool) {
	switch msg.(type) {
	case *pgproto3.AuthenticationOk:
		return pgproto3.AuthTypeOk, true
	case *pgproto3.AuthenticationCleartextPassword:
		return pgproto3.AuthTypeCleartextPassword, true
	case *pgproto3.AuthenticationMD5Password:
		return pgproto3.AuthTypeMD5Password, true
	case *pgproto3.AuthenticationGSS:
		return pgproto3.AuthTypeGSS, true
	case *pgproto3.AuthenticationGSSContinue:
		return pgproto3.AuthTypeGSSCont, true
	case *pgproto3.AuthenticationSASL:
		return pgproto3.AuthTypeSASL, true
	case *pgproto3.AuthenticationSASLContinue:
		return pgproto3.AuthTypeSASLContinue, true
	case *pgproto3.AuthenticationSASLFinal:
		return pgproto3.AuthTypeSASLFinal, true
	default:
		return 0, false
	}
}
