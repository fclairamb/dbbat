package shared

import (
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

func tcpPair(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	type res struct {
		c   net.Conn
		err error
	}

	ch := make(chan res, 1)

	go func() {
		c, err := ln.Accept()
		ch <- res{c, err}
	}()

	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	r := <-ch
	if r.err != nil {
		t.Fatalf("accept: %v", r.err)
	}

	t.Cleanup(func() {
		_ = client.Close()
		_ = r.c.Close()
	})

	return client, r.c
}

func TestParkDetectsClientDisconnect(t *testing.T) {
	t.Parallel()

	client, server := tcpPair(t)

	w := NewWatchedConn(server)
	gone := w.Park()

	_ = client.Close()

	select {
	case <-gone:
	case <-time.After(3 * time.Second):
		t.Fatal("client disconnect not detected — a hold would park forever")
	}
}

func TestParkQueuesPipelinedBytesForReplay(t *testing.T) {
	t.Parallel()

	client, server := tcpPair(t)

	w := NewWatchedConn(server)
	gone := w.Park()

	if _, err := client.Write([]byte("PIPELINED")); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Give the watcher a moment to pick the bytes up.
	deadline := time.Now().Add(2 * time.Second)
	for w.Buffered() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	select {
	case <-gone:
		t.Fatal("client is alive; gone must not fire")
	default:
	}

	w.Unpark()

	buf := make([]byte, 32)

	n, err := w.Read(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	if string(buf[:n]) != "PIPELINED" {
		t.Fatalf("replay lost bytes: got %q", buf[:n])
	}
}

func TestReadPreservesStreamOrderAcrossPark(t *testing.T) {
	t.Parallel()

	client, server := tcpPair(t)

	w := NewWatchedConn(server)

	if _, err := client.Write([]byte("BEFORE")); err != nil {
		t.Fatalf("write: %v", err)
	}

	buf := make([]byte, 6)
	if _, err := io.ReadFull(w, buf); err != nil {
		t.Fatalf("read before: %v", err)
	}

	if string(buf) != "BEFORE" {
		t.Fatalf("got %q", buf)
	}

	w.Park()

	if _, err := client.Write([]byte("DURING")); err != nil {
		t.Fatalf("write during: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for w.Buffered() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	w.Unpark()

	if _, err := client.Write([]byte("AFTER")); err != nil {
		t.Fatalf("write after: %v", err)
	}

	out := make([]byte, 11)
	if _, err := io.ReadFull(w, out); err != nil {
		t.Fatalf("read after: %v", err)
	}

	if string(out) != "DURINGAFTER" {
		t.Fatalf("stream order broken: %q", out)
	}
}

func TestReadReportsEOFOnlyAfterReplayDrained(t *testing.T) {
	t.Parallel()

	client, server := tcpPair(t)

	w := NewWatchedConn(server)
	gone := w.Park()

	if _, err := client.Write([]byte("LAST")); err != nil {
		t.Fatalf("write: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for w.Buffered() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	_ = client.Close()
	<-gone

	buf := make([]byte, 16)

	n, err := w.Read(buf)
	if err != nil || string(buf[:n]) != "LAST" {
		t.Fatalf("queued bytes lost on disconnect: n=%d err=%v got=%q", n, err, buf[:n])
	}

	if _, err := w.Read(buf); err == nil {
		t.Fatal("expected terminal error after replay drained")
	}
}

func TestParkOnAlreadyDeadConnFiresImmediately(t *testing.T) {
	t.Parallel()

	client, server := tcpPair(t)

	w := NewWatchedConn(server)
	gone := w.Park()
	_ = client.Close()
	<-gone

	// A second Park must not hang.
	select {
	case <-w.Park():
	case <-time.After(time.Second):
		t.Fatal("Park on a dead conn did not fire")
	}
}

func TestUnparkWithoutParkIsNoop(t *testing.T) {
	t.Parallel()

	_, server := tcpPair(t)

	w := NewWatchedConn(server)
	w.Unpark()
}

func TestEnableClientKeepAliveOnTCP(t *testing.T) {
	t.Parallel()

	_, server := tcpPair(t)

	if err := EnableClientKeepAlive(server); err != nil {
		t.Fatalf("keepalive: %v", err)
	}

	// Through a WatchedConn wrapper too — that is how the proxies call it.
	if err := EnableClientKeepAlive(NewWatchedConn(server)); err != nil {
		t.Fatalf("keepalive through wrapper: %v", err)
	}
}

func TestEnableClientKeepAliveOnNonTCPIsNoop(t *testing.T) {
	t.Parallel()

	c1, c2 := net.Pipe()
	defer func() { _ = c1.Close(); _ = c2.Close() }()

	if err := EnableClientKeepAlive(c1); err != nil {
		t.Fatalf("expected no-op, got %v", err)
	}
}

func TestConcurrentReadWhileParkedIsRejected(t *testing.T) {
	t.Parallel()

	_, server := tcpPair(t)

	w := NewWatchedConn(server)
	w.Park()

	defer w.Unpark()

	if _, err := w.Read(make([]byte, 4)); !errors.Is(err, ErrConnParked) {
		t.Fatalf("got %v, want ErrConnParked", err)
	}
}
