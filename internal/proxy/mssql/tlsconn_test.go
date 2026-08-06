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
func TestEncapsulatedTLSHandshake(t *testing.T) {
	t.Parallel()

	serverTLS, err := generateSelfSignedTLS()
	require.NoError(t, err)

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
		MaxVersion:         tlsMaxVersion,
	})

	require.NoError(t, clientTLS.Handshake())
	require.NoError(t, clientAdapter.deactivate())

	res := <-serverDone
	require.NoError(t, res.err)

	assert.False(t, clientAdapter.isFramed())
	assert.Equal(t, uint16(tlsMaxVersion), clientTLS.ConnectionState().Version,
		"the encapsulated handshake is pinned to TLS 1.2 (see docs/mssql.md)")

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
