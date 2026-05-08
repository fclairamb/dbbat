package shared

import (
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
)

func TestCountingConn_ReadWriteCounts(t *testing.T) {
	t.Parallel()

	a, b := net.Pipe()

	t.Cleanup(func() {
		_ = a.Close()
		_ = b.Close()
	})

	var aRead, aWrote atomic.Int64

	wrapped := NewCountingConn(a, &aRead, &aWrote)

	const payload = "hello world"

	go func() {
		_, _ = b.Write([]byte(payload))
	}()

	buf := make([]byte, len(payload))
	if _, err := io.ReadFull(wrapped, buf); err != nil {
		t.Fatalf("read: %v", err)
	}

	if got := aRead.Load(); got != int64(len(payload)) {
		t.Fatalf("read counter: got %d, want %d", got, len(payload))
	}

	go func() {
		_, _ = io.ReadAll(b)
	}()

	if _, err := wrapped.Write([]byte(payload)); err != nil {
		t.Fatalf("write: %v", err)
	}

	if got := aWrote.Load(); got != int64(len(payload)) {
		t.Fatalf("write counter: got %d, want %d", got, len(payload))
	}
}

func TestCountingConn_NilCountersAreNoOp(t *testing.T) {
	t.Parallel()

	a, b := net.Pipe()

	t.Cleanup(func() {
		_ = a.Close()
		_ = b.Close()
	})

	wrapped := NewCountingConn(a, nil, nil)

	go func() {
		_, _ = b.Write([]byte("x"))
	}()

	buf := make([]byte, 1)
	if _, err := io.ReadFull(wrapped, buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	// No panic on nil counters is the assertion.
}

func TestCountingConn_ConcurrentReadWriteRace(t *testing.T) {
	t.Parallel()

	a, b := net.Pipe()

	t.Cleanup(func() {
		_ = a.Close()
		_ = b.Close()
	})

	var read, wrote atomic.Int64

	wrapped := NewCountingConn(a, &read, &wrote)

	const iterations = 200

	const payload = "abcdefgh"

	var wg sync.WaitGroup

	wg.Add(2)

	go func() {
		defer wg.Done()

		buf := make([]byte, len(payload))

		for i := 0; i < iterations; i++ {
			if _, err := io.ReadFull(wrapped, buf); err != nil {
				return
			}
		}
	}()

	go func() {
		defer wg.Done()

		for i := 0; i < iterations; i++ {
			if _, err := b.Write([]byte(payload)); err != nil {
				return
			}
		}
	}()

	wg.Wait()

	if got := read.Load(); got != int64(iterations*len(payload)) {
		t.Fatalf("read counter: got %d, want %d", got, iterations*len(payload))
	}
}
