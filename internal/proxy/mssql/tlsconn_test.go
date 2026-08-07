package mssql

import (
	"bytes"
	"crypto/tls"
	"io"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pipeConn adapts an io.ReadWriter to net.Conn so handshakeConn can be built
// over a buffer in the byte-level tests.
type pipeConn struct {
	io.ReadWriter
}

func (pipeConn) Close() error                       { return nil }
func (pipeConn) LocalAddr() net.Addr                { return nil }
func (pipeConn) RemoteAddr() net.Addr               { return nil }
func (pipeConn) SetDeadline(_ time.Time) error      { return nil }
func (pipeConn) SetReadDeadline(_ time.Time) error  { return nil }
func (pipeConn) SetWriteDeadline(_ time.Time) error { return nil }

func TestHandshakeConnFramesWrites(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer

	// The read side stays empty; the write must be flushed by deactivate.
	rw := struct {
		io.Reader
		io.Writer
	}{Reader: &bytes.Buffer{}, Writer: &out}

	pkt := newPacketRW(rw)
	conn := newHandshakeConn(pipeConn{rw}, pkt)

	n, err := conn.Write([]byte{0x16, 0x03, 0x01, 0x00, 0x05})
	require.NoError(t, err)
	assert.Equal(t, 5, n)
	assert.Zero(t, out.Len(), "the packet stays open until a read or deactivate closes it")

	require.NoError(t, conn.deactivate())
	assert.False(t, conn.isFramed())

	hdr, err := decodeHeader(out.Bytes())
	require.NoError(t, err)
	assert.Equal(t, packetTypePrelogin, hdr.Type, "handshake bytes ride inside PRELOGIN packets")
	assert.True(t, hdr.isEOM())
	assert.Equal(t, []byte{0x16, 0x03, 0x01, 0x00, 0x05}, out.Bytes()[packetHeaderSize:])
}

func TestHandshakeConnFinishesThePendingPacketBeforeReading(t *testing.T) {
	t.Parallel()

	inbound := synthPacket(packetTypePrelogin, statusEOM, 1, []byte("server-flight"))

	var out bytes.Buffer

	rw := struct {
		io.Reader
		io.Writer
	}{Reader: bytes.NewReader(inbound), Writer: &out}

	pkt := newPacketRW(rw)
	conn := newHandshakeConn(pipeConn{rw}, pkt)

	_, err := conn.Write([]byte("client-flight"))
	require.NoError(t, err)
	assert.Zero(t, out.Len())

	buf := make([]byte, 64)

	n, err := conn.Read(buf)
	require.NoError(t, err)
	assert.Equal(t, "server-flight", string(buf[:n]))

	// The read is what closed the outbound flight — this is the rule that
	// keeps the peer from waiting forever.
	require.NotZero(t, out.Len())

	hdr, err := decodeHeader(out.Bytes())
	require.NoError(t, err)
	assert.True(t, hdr.isEOM())
	assert.Equal(t, "client-flight", string(out.Bytes()[packetHeaderSize:]))
}

func TestHandshakeConnUnframesReadsAcrossPackets(t *testing.T) {
	t.Parallel()

	// One TLS flight split over two TDS packets, as a large ServerHello +
	// Certificate flight really is.
	inbound := bytes.Join([][]byte{
		synthPacket(packetTypePrelogin, statusNormal, 1, []byte("part-one:")),
		synthPacket(packetTypePrelogin, statusEOM, 2, []byte("part-two")),
		synthPacket(packetTypePrelogin, statusEOM, 1, []byte("second-flight")),
	}, nil)

	rw := struct {
		io.Reader
		io.Writer
	}{Reader: bytes.NewReader(inbound), Writer: io.Discard}

	pkt := newPacketRW(rw)
	conn := newHandshakeConn(pipeConn{rw}, pkt)

	got := make([]byte, 0, 32)
	buf := make([]byte, 5)

	for len(got) < len("part-one:part-two") {
		n, err := conn.Read(buf)
		require.NoError(t, err)

		got = append(got, buf[:n]...)
	}

	assert.Equal(t, "part-one:part-two", string(got))

	// The next message is served independently.
	next := make([]byte, 32)

	n, err := conn.Read(next)
	require.NoError(t, err)
	assert.Equal(t, "second-flight", string(next[:n]))
}

func TestHandshakeConnRejectsNonPreloginPackets(t *testing.T) {
	t.Parallel()

	inbound := synthPacket(packetTypeLogin7, statusEOM, 1, []byte("nope"))

	rw := struct {
		io.Reader
		io.Writer
	}{Reader: bytes.NewReader(inbound), Writer: io.Discard}

	conn := newHandshakeConn(pipeConn{rw}, newPacketRW(rw))

	_, err := conn.Read(make([]byte, 16))
	require.ErrorIs(t, err, ErrHandshakeWrongPacketType)
}

// TestHandshakeConnDeactivateReportsUnconsumedBytes pins the invariant that
// makes the framed -> passthrough switch safe: when the handshake ends there
// must be nothing left in the inbound PRELOGIN message. Bytes still sitting
// there belong to the TLS session and pass-through mode would never see them,
// so losing them silently would corrupt the stream somewhere unrelated.
func TestHandshakeConnDeactivateReportsUnconsumedBytes(t *testing.T) {
	t.Parallel()

	inbound := synthPacket(packetTypePrelogin, statusEOM, 1, []byte("flight-plus-leftovers"))

	rw := struct {
		io.Reader
		io.Writer
	}{Reader: bytes.NewReader(inbound), Writer: io.Discard}

	conn := newHandshakeConn(pipeConn{rw}, newPacketRW(rw))

	// Consume only part of the message, as an over-packed peer would leave it.
	n, err := conn.Read(make([]byte, 6))
	require.NoError(t, err)
	require.Equal(t, 6, n)

	err = conn.deactivate()
	require.ErrorIs(t, err, ErrHandshakeBytesUnconsumed)
	assert.Contains(t, err.Error(), "15 bytes")

	// The state change still happened: the adapter is no longer framing, so a
	// caller that chooses to continue is not left half-switched.
	assert.False(t, conn.isFramed())
}

func TestHandshakeConnPassesThroughAfterDeactivate(t *testing.T) {
	t.Parallel()

	client, server := net.Pipe()
	t.Cleanup(func() {
		_ = client.Close()
		_ = server.Close()
	})

	conn := newHandshakeConn(server, newPacketRW(server))
	require.NoError(t, conn.deactivate())
	assert.False(t, conn.isFramed())

	// deactivate is idempotent.
	require.NoError(t, conn.deactivate())

	done := make(chan error, 1)

	go func() {
		_, err := conn.Write([]byte("raw bytes"))
		done <- err
	}()

	buf := make([]byte, 9)
	_, err := io.ReadFull(client, buf)
	require.NoError(t, err)
	require.NoError(t, <-done)
	assert.Equal(t, "raw bytes", string(buf),
		"after deactivate nothing is wrapped in a TDS packet any more")
}

// TestEncapsulatedTLSHandshake drives a genuine crypto/tls handshake through
// two handshakeConns facing each other, which is the only way to prove the
// framing rules are right: a mistake here shows up as a hang, not a diff.
//
// It runs at both negotiable ceilings. TLS 1.3 is the interesting one: the
// client's handshake ends on a *write*, so the framed→raw switch no longer
// lands on the same byte for both peers, and the server emits its session
// tickets before it reads the client's Finished (which is why the proxy turns
// tickets off at 1.3 — see applyMaxVersion).
func TestEncapsulatedTLSHandshake(t *testing.T) {
	t.Parallel()

	versions := []struct {
		name string
		max  uint16
	}{
		{name: "tls1.2", max: tls.VersionTLS12},
		{name: "tls1.3", max: tls.VersionTLS13},
	}

	for _, version := range versions {
		t.Run(version.name, func(t *testing.T) {
			t.Parallel()

			base, err := generateSelfSignedTLS()
			require.NoError(t, err)

			serverTLS := applyMaxVersion(base, version.max)

			clientRaw, serverRaw := net.Pipe()
			t.Cleanup(func() {
				_ = clientRaw.Close()
				_ = serverRaw.Close()
			})

			type result struct {
				conn *tls.Conn
				err  error
			}

			serverDone := make(chan result, 1)

			go func() {
				adapter := newHandshakeConn(serverRaw, newPacketRW(serverRaw))
				tlsConn := tls.Server(adapter, serverTLS)

				if err := tlsConn.Handshake(); err != nil {
					serverDone <- result{err: err}

					return
				}

				if err := adapter.deactivate(); err != nil {
					serverDone <- result{err: err}

					return
				}

				serverDone <- result{conn: tlsConn}
			}()

			clientAdapter := newHandshakeConn(clientRaw, newPacketRW(clientRaw))
			clientTLS := tls.Client(clientAdapter, &tls.Config{
				InsecureSkipVerify: true, // self-signed cert generated in this test
				MinVersion:         tls.VersionTLS12,
				MaxVersion:         version.max,
				// Resumption-capable, like every non-Go TLS client: at 1.3
				// that is what would make the server send session tickets, so
				// the proxy's own config has to hold up against it.
				ClientSessionCache: tls.NewLRUClientSessionCache(1),
			})

			require.NoError(t, clientTLS.Handshake())
			// Under 1.3 this is what flushes the client's final flight: its
			// handshake ended on a write, with no read to close the packet.
			require.NoError(t, clientAdapter.deactivate())

			res := <-serverDone
			require.NoError(t, res.err)

			assert.False(t, clientAdapter.isFramed())
			assert.Equal(t, version.max, clientTLS.ConnectionState().Version)

			// Application data now flows as raw TLS records, no TDS wrapper.
			appDone := make(chan error, 1)

			go func() {
				_, err := clientTLS.Write([]byte("post-handshake"))
				appDone <- err
			}()

			buf := make([]byte, len("post-handshake"))
			_, err = io.ReadFull(res.conn, buf)
			require.NoError(t, err)
			require.NoError(t, <-appDone)
			assert.Equal(t, "post-handshake", string(buf))
		})
	}
}

// TestHandshakeConnUnframesOnARawRecord pins the read-side half of TLS 1.3
// tolerance: a peer that stops encapsulating before dbbat does must not
// desynchronise the stream. The adapter sniffs the first byte — TDS packet
// types and TLS content types do not overlap — and follows the peer into
// pass-through, serving the sniffed byte first.
func TestHandshakeConnUnframesOnARawRecord(t *testing.T) {
	t.Parallel()

	// A framed flight, then a bare TLS record: exactly what a driver that
	// switches to raw records after reading the server's flight produces.
	rawRecord := []byte{0x16, 0x03, 0x03, 0x00, 0x04, 'f', 'i', 'n', '!'}
	inbound := bytes.Join([][]byte{
		synthPacket(packetTypePrelogin, statusEOM, 1, []byte("framed-flight")),
		rawRecord,
	}, nil)

	rw := struct {
		io.Reader
		io.Writer
	}{Reader: bytes.NewReader(inbound), Writer: io.Discard}

	conn := newHandshakeConn(pipeConn{rw}, newPacketRW(rw))

	buf := make([]byte, 32)

	n, err := conn.Read(buf)
	require.NoError(t, err)
	assert.Equal(t, "framed-flight", string(buf[:n]))
	assert.True(t, conn.isFramed())

	// The raw record comes back byte for byte, content type included.
	got := make([]byte, 0, len(rawRecord))
	for len(got) < len(rawRecord) {
		n, err = conn.Read(buf)
		require.NoError(t, err)

		got = append(got, buf[:n]...)
	}

	assert.Equal(t, rawRecord, got, "the sniffed content-type byte must not be swallowed")
	assert.False(t, conn.isFramed())

	// deactivate is then a no-op rather than a complaint about leftovers.
	require.NoError(t, conn.deactivate())
}

// stingyConn is a handshakeConn that never returns more than one byte per Read.
//
// crypto/tls normally over-reads — it grows its record buffer generously and
// takes whatever the transport offers — which accidentally hides trailing bytes
// in an encapsulated flight by hoovering them into its own buffer. A client
// that reads exactly what it needs does not, and that is a legitimate thing for
// a driver to do. It is the case TestSessionTicketsWouldStrandBytesAtTLS13
// needs in order to see the hazard at all.
type stingyConn struct {
	*handshakeConn
}

func (c *stingyConn) Read(b []byte) (int, error) {
	if len(b) > 1 {
		b = b[:1]
	}

	return c.handshakeConn.Read(b)
}

// TestSessionTicketsWouldStrandBytesAtTLS13 is why applyMaxVersion turns
// session tickets off once TLS 1.3 is reachable.
//
// At 1.3 the Go server emits NewSessionTicket after its own Finished and before
// it reads the client's, so the tickets travel in the same encapsulated flight
// — but the client's handshake returns as soon as it has processed Finished. A
// client that does not over-read is then left with ticket bytes sitting in a
// PRELOGIN message it will never look at again, which corrupts the stream a few
// reads later. The first subtest shows that; the second shows the proxy's
// actual configuration avoiding it.
func TestSessionTicketsWouldStrandBytesAtTLS13(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		disableTickets bool
		wantStranded   bool
	}{
		{name: "tickets enabled strands bytes", disableTickets: false, wantStranded: true},
		{name: "proxy configuration is clean", disableTickets: true, wantStranded: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			serverTLS, err := generateSelfSignedTLS()
			require.NoError(t, err)

			serverTLS.MaxVersion = tls.VersionTLS13
			serverTLS.SessionTicketsDisabled = tc.disableTickets

			clientRaw, serverRaw := net.Pipe()
			t.Cleanup(func() {
				_ = clientRaw.Close()
				_ = serverRaw.Close()
			})

			deadline := time.Now().Add(30 * time.Second)
			require.NoError(t, clientRaw.SetDeadline(deadline))
			require.NoError(t, serverRaw.SetDeadline(deadline))

			serverDone := make(chan error, 1)

			go func() {
				adapter := newHandshakeConn(serverRaw, newPacketRW(serverRaw))
				tlsConn := tls.Server(adapter, serverTLS)

				if err := tlsConn.Handshake(); err != nil {
					serverDone <- err

					return
				}

				serverDone <- adapter.deactivate()
			}()

			clientAdapter := newHandshakeConn(clientRaw, newPacketRW(clientRaw))
			clientTLS := tls.Client(&stingyConn{clientAdapter}, &tls.Config{
				InsecureSkipVerify: true, // self-signed cert generated in this test
				MinVersion:         tls.VersionTLS13,
				MaxVersion:         tls.VersionTLS13,
				// A resumption-capable client — which is what advertising
				// psk_key_exchange_modes means, and what makes the server
				// bother sending tickets. OpenSSL and SChannel clients (so the
				// ODBC and JDBC drivers) advertise it unconditionally; a Go
				// client only does so with a session cache, which is why
				// go-mssqldb never provoked this.
				ClientSessionCache: tls.NewLRUClientSessionCache(1),
			})

			require.NoError(t, clientTLS.Handshake())

			err = clientAdapter.deactivate()
			if tc.wantStranded {
				require.ErrorIs(t, err, ErrHandshakeBytesUnconsumed,
					"the server's session tickets are stuck in the encapsulated flight")
			} else {
				require.NoError(t, err)
			}

			require.NoError(t, <-serverDone)
		})
	}
}

// earlyUnframingConn models the driver behaviour the spec worries about: a
// client that considers the encapsulation over as soon as it has read the
// server's handshake flight, and therefore writes its final TLS 1.3
// CCS+Finished as raw records instead of inside a PRELOGIN packet.
//
// go-mssqldb does not do this (it flushes its pending packet after Handshake
// returns, staying framed), but the Microsoft ODBC and JDBC drivers could not
// be captured here, so the proxy has to survive both choices.
type earlyUnframingConn struct {
	net.Conn

	pkt     *packetRW
	inbound []byte
	pos     int
	raw     bool
}

func (c *earlyUnframingConn) Read(b []byte) (int, error) {
	for c.pos >= len(c.inbound) {
		if c.pkt.packetOpen() {
			if err := c.pkt.finishPacket(); err != nil {
				return 0, err
			}
		}

		_, payload, err := c.pkt.ReadMessage()
		if err != nil {
			return 0, err
		}

		c.inbound = payload
		c.pos = 0
	}

	n := copy(b, c.inbound[c.pos:])
	c.pos += n

	// The server's whole flight is in: from here this client writes raw.
	if c.pos >= len(c.inbound) {
		c.raw = true
	}

	return n, nil
}

func (c *earlyUnframingConn) Write(b []byte) (int, error) {
	if c.raw {
		return c.Conn.Write(b)
	}

	if !c.pkt.packetOpen() {
		c.pkt.beginPacket(packetTypePrelogin)
	}

	return c.pkt.WriteStream(b)
}

// TestEncapsulatedTLS13WithAnEarlyUnframingClient is the end-to-end version of
// the tolerance test: a real crypto/tls 1.3 handshake where the client's last
// flight arrives as raw TLS records rather than framed. Before the read-side
// sniff this hung (or died parsing a TLS record as a TDS header), which is the
// failure mode that made stage 1 pin the version.
func TestEncapsulatedTLS13WithAnEarlyUnframingClient(t *testing.T) {
	t.Parallel()

	base, err := generateSelfSignedTLS()
	require.NoError(t, err)

	serverTLS := applyMaxVersion(base, tls.VersionTLS13)

	clientRaw, serverRaw := net.Pipe()
	t.Cleanup(func() {
		_ = clientRaw.Close()
		_ = serverRaw.Close()
	})

	// A hang is the failure this test exists to catch, so bound it.
	deadline := time.Now().Add(30 * time.Second)
	require.NoError(t, clientRaw.SetDeadline(deadline))
	require.NoError(t, serverRaw.SetDeadline(deadline))

	type result struct {
		conn    *tls.Conn
		framed  bool
		version uint16
		err     error
	}

	serverDone := make(chan result, 1)

	go func() {
		adapter := newHandshakeConn(serverRaw, newPacketRW(serverRaw))
		tlsConn := tls.Server(adapter, serverTLS)

		if err := tlsConn.Handshake(); err != nil {
			serverDone <- result{err: err}

			return
		}

		// Sampled *before* deactivate, which would set it false regardless:
		// what this proves is that the adapter followed the client out of
		// encapsulation on its own, from the sniffed content-type byte.
		framed := adapter.isFramed()

		if err := adapter.deactivate(); err != nil {
			serverDone <- result{err: err}

			return
		}

		serverDone <- result{
			conn:    tlsConn,
			framed:  framed,
			version: tlsConn.ConnectionState().Version,
		}
	}()

	clientAdapter := &earlyUnframingConn{Conn: clientRaw, pkt: newPacketRW(clientRaw)}
	clientTLS := tls.Client(clientAdapter, &tls.Config{
		InsecureSkipVerify: true, // self-signed cert generated in this test
		MinVersion:         tls.VersionTLS13,
		MaxVersion:         tls.VersionTLS13,
	})

	require.NoError(t, clientTLS.Handshake())
	require.True(t, clientAdapter.raw, "the test client must have unframed before its final flight")

	res := <-serverDone
	require.NoError(t, res.err)
	assert.False(t, res.framed,
		"the proxy must have unframed from the sniffed record byte, before deactivate")
	assert.Equal(t, uint16(tls.VersionTLS13), res.version)

	// And the session works afterwards, which is what proves the switch landed
	// on the right byte rather than merely not erroring.
	appDone := make(chan error, 1)

	go func() {
		_, err := clientTLS.Write([]byte("post-handshake"))
		appDone <- err
	}()

	buf := make([]byte, len("post-handshake"))
	_, err = io.ReadFull(res.conn, buf)
	require.NoError(t, err)
	require.NoError(t, <-appDone)
	assert.Equal(t, "post-handshake", string(buf))
}

func TestRevertibleConnSwapsStreams(t *testing.T) {
	t.Parallel()

	rawClient, rawServer := net.Pipe()
	t.Cleanup(func() {
		_ = rawClient.Close()
		_ = rawServer.Close()
	})

	active := &bytes.Buffer{}
	active.WriteString("from the tls stream")

	conn := newRevertibleConn(rawServer)
	conn.switchTo(active)

	buf := make([]byte, len("from the tls stream"))
	_, err := io.ReadFull(conn, buf)
	require.NoError(t, err)
	assert.Equal(t, "from the tls stream", string(buf))

	conn.revert()

	go func() {
		_, _ = rawClient.Write([]byte("cleartext again"))
	}()

	buf = make([]byte, len("cleartext again"))
	_, err = io.ReadFull(conn, buf)
	require.NoError(t, err)
	assert.Equal(t, "cleartext again", string(buf))

	// Reverting twice changes nothing.
	conn.revert()

	writeDone := make(chan error, 1)

	go func() {
		_, err := conn.Write([]byte("out"))
		writeDone <- err
	}()

	out := make([]byte, 3)
	_, err = io.ReadFull(rawClient, out)
	require.NoError(t, err)
	require.NoError(t, <-writeDone)
	assert.Equal(t, "out", string(out))
}

func TestHandshakeConnReadPropagatesEOF(t *testing.T) {
	t.Parallel()

	rw := struct {
		io.Reader
		io.Writer
	}{Reader: bytes.NewReader(nil), Writer: io.Discard}

	conn := newHandshakeConn(pipeConn{rw}, newPacketRW(rw))

	_, err := conn.Read(make([]byte, 8))
	require.ErrorIs(t, err, io.EOF)
}
