package shared_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/fclairamb/dbbat/internal/proxy/shared"
	"github.com/fclairamb/dbbat/internal/store"
)

// errBoom is what a panicking fake raises. A panic value, not a returned error:
// the whole point is the path where the store call never returns.
var errBoom = errors.New("store write blew up") //nolint:err113 // test-local sentinel

// panickingStore panics on every write. It satisfies both store slices the
// detached writers use, so one fake drives the whole table.
type panickingStore struct{ calls atomic.Int32 }

func (p *panickingStore) IncrementAPIKeyUsage(context.Context, uuid.UUID) error {
	p.calls.Add(1)

	panic(errBoom)
}

func (p *panickingStore) StoreQueryRows(context.Context, []store.PendingQueryRow) error {
	p.calls.Add(1)

	panic(errBoom)
}

func (p *panickingStore) SealQueryRowChain(context.Context, uuid.UUID) error {
	p.calls.Add(1)

	panic(errBoom)
}

// TestDetachedStoreWritePanicsDoNotEscape is the pin for the whole class of
// goroutine this package's helpers exist for.
//
// Every proxy fires store writes on goroutines that deliberately outlive the
// call that spawned them — a query record must not block the wire — so the
// recover on handleConnection never sees them. Unrecovered, a panic in any one
// of them ends the *process*: every live session, of every user, on every
// database. Reaching the end of a subtest without the binary dying is the
// assertion; the extra checks are that recovering did not merely swap process
// death for a hang.
//
// The two entries are the shapes reachable without a real store. The
// per-protocol writers go through the same RunGuarded and are pinned where they
// have a seam of their own (see the Oracle package).
func TestDetachedStoreWritePanicsDoNotEscape(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		run  func(t *testing.T, st *panickingStore)
	}{
		{
			// The one-liner all five proxies and the REST middleware share.
			// Nothing waits on it, so surviving is the whole contract.
			name: "api key usage bump",
			run: func(t *testing.T, st *panickingStore) {
				t.Helper()

				shared.BumpAPIKeyUsage(context.Background(), discardLogger(), st, uuid.New())

				waitFor(t, func() bool { return st.calls.Load() > 0 },
					"the usage bump never reached the store")
			},
		},
		{
			// The process-wide drain. Here recovering is not enough on its
			// own: producers park in Flush, and it is run's own
			// `defer close(w.done)` that releases them. A hang here would be
			// the regression, not a crash.
			name: "captured row writer drain",
			run: func(t *testing.T, st *panickingStore) {
				t.Helper()

				w := shared.NewRowWriter(st, discardLogger())
				sink := w.NewSinkFor(uuid.New())

				sink.Add(store.QueryRow{RowNumber: 1, RowData: []byte("{}"), RowSizeBytes: 2})

				done := make(chan struct{})
				go func() {
					defer close(done)

					sink.Flush(context.Background())
				}()

				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Fatal("a panicking drain left a producer parked in Flush forever")
				}

				if !sink.Dropped() {
					t.Fatal("a capture the drain never wrote must report itself as dropped")
				}

				// The writer is stopped, so a later capture is refused rather
				// than queued into a channel nobody drains.
				if later := w.NewSinkFor(uuid.New()); later.Add(store.QueryRow{RowSizeBytes: 1}) {
					t.Fatal("a dead drain must stop accepting rows")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			tt.run(t, new(panickingStore))
		})
	}
}

// TestRunMaintenanceKeepsTheLoopAlive is the difference between RunMaintenance
// and wrapping a background loop whole: a retention sweep, a cache eviction or
// an upload that panics must cost one turn, not the loop. Silently retiring the
// sweep for the life of the process is the failure this guards against — it
// looks exactly like health from the outside.
func TestRunMaintenanceKeepsTheLoopAlive(t *testing.T) {
	t.Parallel()

	turns := 0

	for i := range 3 {
		shared.RunMaintenance(context.Background(), discardLogger(), "retention sweep", func() {
			turns++

			if i == 0 {
				panic(errBoom)
			}
		})
	}

	if turns != 3 {
		t.Fatalf("every turn must still run after a panicking one, got %d of 3", turns)
	}
}

// waitFor polls until cond holds or the test's patience runs out.
func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)

	for time.Now().Before(deadline) {
		if cond() {
			return
		}

		time.Sleep(5 * time.Millisecond)
	}

	t.Fatal(msg)
}
