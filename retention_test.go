package main

import (
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/dump"
	"github.com/fclairamb/dbbat/internal/store"
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

// Retention expiry accounts for each capture: source "retention", no actor,
// the connection UID read from the file name and the size it had.
func TestDumpDeletedEventsFromRetention(t *testing.T) {
	t.Parallel()

	uid := uuid.New()
	events := dumpDeletedEvents([]dump.RemovedFile{
		{Name: uid.String() + dump.FileExt, Size: 42},
		{Name: "legacy-leftover.dump", Size: 7},
	})
	require.Len(t, events, 2)

	var first map[string]any
	require.NoError(t, json.Unmarshal(events[0].Details, &first))

	assert.Equal(t, store.AuditEventDumpDeleted, events[0].EventType)
	assert.Nil(t, events[0].PerformedBy)
	assert.Equal(t, "retention", first["source"])
	assert.Equal(t, uid.String(), first["connection_uid"])
	assert.InDelta(t, 42, first["size_bytes"], 0)
	assert.Equal(t, []any{"local"}, first["locations"])

	var second map[string]any
	require.NoError(t, json.Unmarshal(events[1].Details, &second))
	assert.NotContains(t, second, "connection_uid")
	assert.Equal(t, "legacy-leftover.dump", second["file"])
}
