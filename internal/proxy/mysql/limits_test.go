package mysql

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync/atomic"
	"testing"
	"time"

	gomysql "github.com/go-mysql-org/go-mysql/mysql"
	"github.com/google/uuid"

	"github.com/fclairamb/dbbat/internal/cache"
	"github.com/fclairamb/dbbat/internal/proxy/shared"
	"github.com/fclairamb/dbbat/internal/store"
)

// TestRunIntercepted_Revoked asserts a revoked grant blocks the next command on
// a live MySQL connection: runIntercepted returns ErrGrantRevoked and never
// runs the exec.
func TestRunIntercepted_Revoked(t *testing.T) {
	t.Parallel()

	reg := cache.NewRevocationRegistry()
	grant := &store.Grant{UID: uuid.New(), ExpiresAt: time.Now().Add(time.Hour)}
	h := reg.Register(grant.UID)
	reg.Revoke(grant.UID)

	s := &Session{
		logger:     discardLogger(),
		ctx:        context.Background(),
		grant:      grant,
		revocation: h,
	}
	hnd := &handler{session: s}

	execRan := false

	_, err := hnd.runIntercepted("SELECT 1", nil, func() (*gomysql.Result, error) {
		execRan = true

		return &gomysql.Result{}, nil
	})

	if !errors.Is(err, shared.ErrGrantRevoked) {
		t.Fatalf("runIntercepted() after revoke = %v, want ErrGrantRevoked", err)
	}

	if execRan {
		t.Fatal("exec must not run for a revoked grant")
	}
}

// TestWatchdog_TearsDownOnRevocation exercises the real wiring seam for
// revocation: the guard is built with the session's revocation flag and the
// watchdog tears both conns down once the grant is revoked.
func TestWatchdog_TearsDownOnRevocation(t *testing.T) {
	t.Parallel()

	s := &Session{logger: discardLogger(), ctx: context.Background()}

	reg := cache.NewRevocationRegistry()
	grant := &store.Grant{UID: uuid.New(), ExpiresAt: time.Now().Add(time.Hour)}
	h := reg.Register(grant.UID)

	guard := shared.NewLimitGuard(grant, &atomic.Int64{}, &atomic.Int64{}).WithRevocation(h.Flag())

	clientConn := pipePair(t)
	upstreamConn := pipePair(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go guard.Watch(ctx, time.Millisecond, func(err error) {
		s.onLimitViolation(upstreamConn, clientConn, err)
	})

	reg.Revoke(grant.UID)

	assertConnClosed(t, clientConn, "client")
	assertConnClosed(t, upstreamConn, "upstream")
}

// TestCheckQuotas_Expiry asserts the between-commands expiry gap is closed: a
// command issued after the grant's ExpiresAt is rejected with ErrGrantExpired,
// while a live grant is accepted.
func TestCheckQuotas_Expiry(t *testing.T) {
	t.Parallel()

	if err := checkQuotas(&store.Grant{ExpiresAt: time.Now().Add(-time.Minute)}); !errors.Is(err, shared.ErrGrantExpired) {
		t.Fatalf("checkQuotas() expired grant = %v, want ErrGrantExpired", err)
	}

	if err := checkQuotas(&store.Grant{ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatalf("checkQuotas() live grant = %v, want nil", err)
	}
}

// discardLogger builds a no-op logger for session tests.
func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// pipePair returns a net.Pipe whose peer end is closed on test cleanup.
func pipePair(t *testing.T) net.Conn {
	t.Helper()

	conn, peer := net.Pipe()
	t.Cleanup(func() { _ = peer.Close() })

	return conn
}

// assertConnClosed asserts that conn has been closed: a Read on our own end of
// a net.Pipe returns io.ErrClosedPipe. A generous read deadline turns a "never
// closed" bug into a clear timeout failure instead of a hang.
func assertConnClosed(t *testing.T, conn net.Conn, name string) {
	t.Helper()

	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))

	buf := make([]byte, 1)
	if _, err := conn.Read(buf); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("%s conn: Read err = %v, want io.ErrClosedPipe (conn should be closed)", name, err)
	}
}

// TestOnLimitViolation_ClosesConns asserts the teardown callback force-closes
// both the client-facing and upstream conns — the mid-query enforcement
// mechanism for MySQL (the go-mysql library owns the wire, so closing the conns
// is the only way to abort an in-flight query).
func TestOnLimitViolation_ClosesConns(t *testing.T) {
	t.Parallel()

	s := &Session{logger: discardLogger(), ctx: context.Background()}

	clientConn := pipePair(t)
	upstreamConn := pipePair(t)

	s.onLimitViolation(upstreamConn, clientConn, shared.ErrByteQuotaExceeded)

	assertConnClosed(t, clientConn, "client")
	assertConnClosed(t, upstreamConn, "upstream")
}

// TestWatchdog_TearsDownOnByteQuota exercises the real wiring seam: it builds
// the guard the way mysql/auth.go does (from the session's grant) and starts
// the watchdog the way mysql/session.go's Run does (guard.Watch invoking
// onLimitViolation), then asserts both conns are torn down. The grant is
// pre-exhausted so the watchdog's immediate check trips without waiting.
func TestWatchdog_TearsDownOnByteQuota(t *testing.T) {
	t.Parallel()

	s := &Session{logger: discardLogger(), ctx: context.Background()}

	maxBytes := int64(50)
	grant := &store.Grant{BytesTransferred: 100, MaxBytesTransferred: &maxBytes}
	guard := shared.NewLimitGuard(grant, &atomic.Int64{}, &atomic.Int64{})

	clientConn := pipePair(t)
	upstreamConn := pipePair(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go guard.Watch(ctx, time.Millisecond, func(err error) {
		s.onLimitViolation(upstreamConn, clientConn, err)
	})

	assertConnClosed(t, clientConn, "client")
	assertConnClosed(t, upstreamConn, "upstream")
}

// TestWatchdog_TearsDownOnExpiry mirrors TestWatchdog_TearsDownOnByteQuota for
// the time-window limit: an already-expired grant trips the watchdog, which
// tears the session down mid-query.
func TestWatchdog_TearsDownOnExpiry(t *testing.T) {
	t.Parallel()

	s := &Session{logger: discardLogger(), ctx: context.Background()}

	grant := &store.Grant{ExpiresAt: time.Now().Add(-time.Minute)}
	guard := shared.NewLimitGuard(grant, &atomic.Int64{}, &atomic.Int64{})

	clientConn := pipePair(t)
	upstreamConn := pipePair(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go guard.Watch(ctx, time.Millisecond, func(err error) {
		s.onLimitViolation(upstreamConn, clientConn, err)
	})

	assertConnClosed(t, clientConn, "client")
	assertConnClosed(t, upstreamConn, "upstream")
}
