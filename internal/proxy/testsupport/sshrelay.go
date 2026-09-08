package testsupport

import (
	"io"
	"net"
	"sync"

	"golang.org/x/crypto/ssh"
)

// halfCloser is what a TCP connection offers to shut down its write side while
// leaving the read side open. net.TCPConn has it; a wrapper that does not gets
// the full-close fallback below.
type halfCloser interface {
	CloseWrite() error
}

// PipeSSHChannel relays an accepted direct-tcpip channel to its upstream
// connection, the way a real sshd does — and, crucially for the tests that use
// it, propagating each direction's EOF as a *half*-close rather than a full one.
//
// That distinction is the whole point of this helper existing. A relay that
// answers one direction's EOF by closing the opposite end tears down bytes that
// are still in flight: an upstream which writes a protocol-level refusal and
// then half-closes has its refusal truncated, and the client sees a bare EOF
// instead. The conncheck suite classifies exactly that difference
// (db_auth_failed vs db_handshake_failed), so a full-closing relay makes it
// flaky. Here each copy shuts down only the write side of the peer it was
// feeding, and both ends are closed for real only once both directions have
// finished.
//
// Test support only: production never relays an SSH channel like this. The real
// tunnel hands x/crypto/ssh's net.Conn straight to the database driver.
func PipeSSHChannel(ch ssh.Channel, upstream net.Conn) {
	var wg sync.WaitGroup

	wg.Add(2)

	go func() {
		defer wg.Done()

		_, _ = io.Copy(upstream, ch)

		// The client is done sending. Let the upstream see its EOF without
		// losing whatever it is still writing back.
		if hc, ok := upstream.(halfCloser); ok {
			_ = hc.CloseWrite()
		} else {
			_ = upstream.Close()
		}
	}()

	go func() {
		defer wg.Done()

		_, _ = io.Copy(ch, upstream)

		// ssh.Channel always provides CloseWrite: it sends the SSH EOF message,
		// which leaves the channel open for the other direction.
		_ = ch.CloseWrite()
	}()

	go func() {
		wg.Wait()

		_ = ch.Close()
		_ = upstream.Close()
	}()
}
