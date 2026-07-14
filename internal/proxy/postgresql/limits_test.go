package postgresql

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/fclairamb/dbbat/internal/cache"
	"github.com/fclairamb/dbbat/internal/proxy/shared"
	"github.com/fclairamb/dbbat/internal/store"
)

// assertPeerClosed reads from one end of a net.Pipe and requires the read to
// observe the peer being force-closed (io.EOF / closed pipe) within the
// timeout, i.e. the session tore the connection down.
func assertPeerClosed(t *testing.T, c net.Conn, name string) {
	t.Helper()

	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))

	buf := make([]byte, 1)

	_, err := c.Read(buf)
	if err == nil {
		t.Fatalf("%s: read succeeded, want the conn to be closed", name)
	}

	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrClosedPipe) {
		return
	}

	t.Fatalf("%s: read err = %v, want EOF/closed-pipe (conn not torn down in time)", name, err)
}

// TestSession_CheckQuotas_Revoked asserts a revoked grant blocks the next
// command on a live connection.
func TestSession_CheckQuotas_Revoked(t *testing.T) {
	t.Parallel()

	reg := cache.NewRevocationRegistry()
	grant := &store.Grant{UID: uuid.New(), ExpiresAt: time.Now().Add(time.Hour)}
	h := reg.Register(grant.UID)

	s := &Session{grant: grant, revocation: h}

	if err := s.checkQuotas(); err != nil {
		t.Fatalf("checkQuotas() before revoke = %v, want nil", err)
	}

	reg.Revoke(grant.UID)

	if err := s.checkQuotas(); !errors.Is(err, shared.ErrGrantRevoked) {
		t.Fatalf("checkQuotas() after revoke = %v, want ErrGrantRevoked", err)
	}
}

// TestSession_Revocation_DisconnectsLiveSession drives the real registry →
// guard watchdog → onLimitViolation seam: revoking a grant while the session's
// watchdog is running must force-close both conns.
func TestSession_Revocation_DisconnectsLiveSession(t *testing.T) {
	t.Parallel()

	reg := cache.NewRevocationRegistry()
	grant := &store.Grant{UID: uuid.New(), ExpiresAt: time.Now().Add(time.Hour)}
	h := reg.Register(grant.UID)

	clientProxyEnd, clientTestEnd := net.Pipe()
	upstreamProxyEnd, upstreamTestEnd := net.Pipe()

	var from, to atomic.Int64

	s := &Session{
		clientConn:   clientProxyEnd,
		upstreamConn: upstreamProxyEnd,
		grant:        grant,
		revocation:   h,
		guard:        shared.NewLimitGuard(grant, &from, &to).WithRevocation(h.Flag()),
		logger:       slog.New(slog.DiscardHandler),
		ctx:          context.Background(),
	}

	go s.guard.Watch(context.Background(), 5*time.Millisecond, s.onLimitViolation)

	// Revoke after the watchdog is running: it must trip and tear both conns
	// down without waiting for a client query.
	reg.Revoke(grant.UID)

	assertPeerClosed(t, clientTestEnd, "client conn")
	assertPeerClosed(t, upstreamTestEnd, "upstream conn")
}

// TestSession_CheckQuotas_Expiry asserts the between-commands expiry gap is
// closed: a command issued after the grant's ExpiresAt is rejected.
func TestSession_CheckQuotas_Expiry(t *testing.T) {
	t.Parallel()

	expired := &Session{grant: &store.Grant{ExpiresAt: time.Now().Add(-time.Minute)}}
	if err := expired.checkQuotas(); !errors.Is(err, shared.ErrGrantExpired) {
		t.Fatalf("checkQuotas() with expired grant = %v, want ErrGrantExpired", err)
	}

	live := &Session{grant: &store.Grant{ExpiresAt: time.Now().Add(time.Hour)}}
	if err := live.checkQuotas(); err != nil {
		t.Fatalf("checkQuotas() with live grant = %v, want nil", err)
	}
}

// TestSession_ProxyUpstreamToClient_ByteLimitAbort drives the real
// upstream→client relay: a grant with a tiny byte cap must cut a streaming
// result off mid-flight and deliver the client a clean ErrorResponse
// (SQLSTATE 53400) rather than the whole result.
func TestSession_ProxyUpstreamToClient_ByteLimitAbort(t *testing.T) {
	t.Parallel()

	clientProxyEnd, clientTestEnd := net.Pipe()
	upstreamProxyEnd, upstreamTestEnd := net.Pipe()

	var fromClient, toClient atomic.Int64

	countedClient := shared.NewCountingConn(clientProxyEnd, &fromClient, &toClient)

	maxBytes := int64(150)
	grant := &store.Grant{
		MaxBytesTransferred: &maxBytes,
		ExpiresAt:           time.Now().Add(time.Hour),
	}

	s := &Session{
		clientConn:       countedClient,
		clientBackend:    pgproto3.NewBackend(countedClient, countedClient),
		upstreamFrontend: pgproto3.NewFrontend(upstreamProxyEnd, upstreamProxyEnd),
		grant:            grant,
		bytesFromClient:  &fromClient,
		bytesToClient:    &toClient,
		guard:            shared.NewLimitGuard(grant, &fromClient, &toClient),
		currentQuery:     &pendingQuery{sql: "SELECT * FROM big"},
		extendedState: &extendedQueryState{
			preparedStatements: make(map[string]*preparedStatement),
			portals:            make(map[string]*portalState),
		},
		logger: slog.New(slog.DiscardHandler),
		ctx:    context.Background(),
	}

	// Simulated upstream server: send a RowDescription then stream DataRows
	// until the pipe errors (the proxy stops reading once it aborts).
	go func() {
		rd := &pgproto3.RowDescription{Fields: []pgproto3.FieldDescription{{Name: []byte("col")}}}

		buf, err := rd.Encode(nil)
		if err != nil {
			return
		}

		if _, err := upstreamTestEnd.Write(buf); err != nil {
			return
		}

		row := &pgproto3.DataRow{Values: [][]byte{[]byte("0123456789ABCDEF")}}

		for i := 0; i < 100000; i++ {
			b, err := row.Encode(nil)
			if err != nil {
				return
			}

			if _, err := upstreamTestEnd.Write(b); err != nil {
				return
			}
		}
	}()

	// Simulated client: read forwarded messages and capture the terminating
	// ErrorResponse. It must keep draining after the ErrorResponse so the
	// trailing ReadyForQuery abortStream writes doesn't block the (unbuffered)
	// pipe and wedge proxyUpstreamToClient.
	type clientResult struct {
		errResp *pgproto3.ErrorResponse
		rows    int
	}

	resCh := make(chan clientResult, 1)

	go func() {
		fe := pgproto3.NewFrontend(clientTestEnd, clientTestEnd)

		var (
			rows    int
			errResp *pgproto3.ErrorResponse
		)

		for {
			msg, err := fe.Receive()
			if err != nil {
				resCh <- clientResult{errResp: errResp, rows: rows}

				return
			}

			switch m := msg.(type) {
			case *pgproto3.DataRow:
				rows++
			case *pgproto3.ErrorResponse:
				errResp = m
			}
		}
	}()

	relayErr := s.proxyUpstreamToClient()

	// Unblock the still-writing upstream goroutine and let the client reader
	// observe EOF so it reports what it captured.
	_ = upstreamTestEnd.Close()
	_ = upstreamProxyEnd.Close()
	_ = clientTestEnd.Close()
	_ = clientProxyEnd.Close()

	if !errors.Is(relayErr, shared.ErrByteQuotaExceeded) {
		t.Fatalf("proxyUpstreamToClient() = %v, want ErrByteQuotaExceeded", relayErr)
	}

	select {
	case res := <-resCh:
		if res.errResp == nil {
			t.Fatalf("client never received an ErrorResponse (saw %d rows)", res.rows)
		}

		if res.errResp.Code != "53400" {
			t.Fatalf("ErrorResponse.Code = %q, want 53400", res.errResp.Code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for client ErrorResponse")
	}
}
