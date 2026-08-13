package oracle

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/store"
)

// errStoreBoom is raised as a panic value, not returned: the shape being pinned
// is the store call that never returns at all.
var errStoreBoom = errors.New("query record write blew up")

// panickingCompletionStore is the query-completion seam with every write
// replaced by a panic. It stands in for the class of failure the detached
// writers are actually exposed to — a decoded statement or a parameter blob
// lifted off the wire that a decoder accepted and an encoder then choked on.
type panickingCompletionStore struct{ creates atomic.Int32 }

func (p *panickingCompletionStore) CreateQuery(context.Context, *store.Query) (*store.Query, error) {
	p.creates.Add(1)

	panic(errStoreBoom)
}

func (p *panickingCompletionStore) UpdateQueryCompletion(
	context.Context, uuid.UUID, *float64, *int64, *string, bool, bool,
) error {
	panic(errStoreBoom)
}

func (p *panickingCompletionStore) IncrementConnectionStats(context.Context, uuid.UUID, int64) error {
	panic(errStoreBoom)
}

// TestDetachedQueryRecordPanicDoesNotEndTheProcess is the per-protocol half of
// the pin in internal/proxy/shared: the query record is written on a goroutine
// that outlives the call that spawned it — deliberately, so the store never
// stalls the wire — which is exactly why the recover on handleConnection cannot
// see it. Before safe.RunGuarded, a panic in that write ended the process:
// every live session, of every user, on every database, because one statement's
// record could not be encoded.
//
// The test binary staying alive is the assertion. The rest checks that the
// refusal itself still behaved: the session must not have waited on the write,
// and the in-session quota counter must still have moved.
func TestDetachedQueryRecordPanicDoesNotEndTheProcess(t *testing.T) {
	t.Parallel()

	s := newTestSession(&store.Grant{Definition: &store.GrantDefinition{Controls: []string{store.ControlReadOnly}}})
	panicker := new(panickingCompletionStore)
	s.completionStore = panicker

	require.Error(t, s.handleOALL8(buildOALL8("INSERT INTO emp (id) VALUES (1)", nil, 7)),
		"the write must still be refused")

	require.Equal(t, int64(1), s.grant.QueryCount,
		"the refusal must still count against the in-session quota")

	deadline := time.Now().Add(5 * time.Second)
	for panicker.creates.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	require.Positive(t, panicker.creates.Load(), "the record write never reached the store")

	// Give the panicking goroutine a moment to unwind. An unrecovered panic
	// kills the test binary here rather than failing this assertion.
	time.Sleep(50 * time.Millisecond)
}
