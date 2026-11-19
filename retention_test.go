package main

import (
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
)

func newIdleSweeper() *retentionSweeper {
	return &retentionSweeper{
		logger: slog.New(slog.DiscardHandler),
		wakeCh: make(chan struct{}, 1),
		stop:   make(chan struct{}),
	}
}

// Any number of wakes collapse into one more pass, and a wake never blocks.
func TestSweeperWakeCoalesces(t *testing.T) {
	t.Parallel()

	s := newIdleSweeper()

	done := make(chan struct{})

	go func() {
		for range 1000 {
			s.Wake()
		}

		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Wake blocked")
	}

	assert.Len(t, s.wakeCh, 1)
}

// With no window set anywhere, a connection close does not wake the sweeper.
func TestSweeperStaysOffWithoutPolicy(t *testing.T) {
	t.Parallel()

	s := newIdleSweeper()

	s.onConnectionClosed(uuid.New(), time.Now())
	assert.Empty(t, s.wakeCh)

	s.activeMu.Lock()
	s.active = true
	s.activeMu.Unlock()

	// Active: the close wakes it (the timer lookup needs a store and is
	// recovered by safe.RunMaintenance when there is none).
	s.onConnectionClosed(uuid.New(), time.Now())
	assert.Len(t, s.wakeCh, 1)
}
