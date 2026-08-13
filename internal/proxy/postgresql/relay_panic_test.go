package postgresql

import (
	"context"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/fclairamb/dbbat/internal/safe"
	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/stretchr/testify/require"
)

// panicOnReadConn blows up the moment a relay reads it — standing in for any
// panic on the wire path that is not already inside a recover of its own.
type panicOnReadConn struct {
	net.Conn
}

func (*panicOnReadConn) Read([]byte) (int, error) { panic("read blew up") }

func (*panicOnReadConn) Write(p []byte) (int, error) { return len(p), nil }

func (*panicOnReadConn) Close() error { return nil }

// TestRelayPanicEndsTheSessionNotTheProcess wires the relay exactly as
// proxyMessages does and asserts both halves of the fix.
//
// The first is free: an unrecovered panic on a relay goroutine kills the
// process, so it would kill this test binary, and nothing would be left to
// report it. The second is the one worth writing down — the error has to be
// produced *outside* the recovered call, so `errChan <- safe.RunRelay(…)`
// sends on the panic path too. Fold it inward
// (`go func() { if err := safe.RunRelay(…); err != nil { … } }()`) and the
// relay dies silently, leaving the session half-open with no leg reading the
// client and nothing to end it.
//
// proxyMessages itself needs a live store to register its revocation handle, so
// this mirrors its wiring rather than calling it. The mirror is the shape under
// test: keep the send here in step with the one there.
func TestRelayPanicEndsTheSessionNotTheProcess(t *testing.T) {
	t.Parallel()

	clientConn := &panicOnReadConn{}

	s := &Session{
		ctx:           context.Background(),
		logger:        slog.New(slog.DiscardHandler),
		clientConn:    clientConn,
		clientBackend: pgproto3.NewBackend(clientConn, clientConn),
	}

	errChan := make(chan error, 2)

	go func() {
		errChan <- safe.RunRelay(s.ctx, s.logger, relayNameClientToUpstream, s.proxyClientToUpstream)
	}()

	select {
	case err := <-errChan:
		require.ErrorIs(t, err, safe.ErrRelayPanic,
			"the panic must come back as an error, not as a dead process")
	case <-time.After(5 * time.Second):
		t.Fatal("the relay panicked without reporting: proxyMessages would block here forever")
	}
}
