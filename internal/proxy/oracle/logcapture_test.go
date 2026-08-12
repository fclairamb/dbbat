package oracle

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/store"
)

// countingHandler counts slog records by message and keeps the `sql` attribute
// of each one, so the cursor-id learning measurement
// (cursor_learning_integration_test.go) can check not only *that* a
// re-execution resolved to a statement but that it resolved to the **right**
// one — a mis-learned cursor id is a silent wrong-SQL gate, which is worse than
// a miss.
//
// It lives here, untagged, rather than beside the measurement it serves,
// because the measurement's counters need a guard that runs in the ordinary
// `make test` — see TestCountingHandlerWatchesTheMessagesTheGateEmits below.
type countingHandler struct {
	mu     sync.Mutex
	counts map[string]int
	sqls   map[string][]string
	// ints keys on message+"\x00"+attr, so the cursor-close record's `tracked`
	// size can be read back: that is what the measurement asserts on to show
	// the tracker stays bounded without a cap.
	ints map[string][]int64
	// bools keys the same way, so a session's negotiated shape can be read
	// back: the synthetic-AUTH suite asserts on `wide_encoding` to prove an
	// OCI client really did drive the wide path rather than the thin one.
	bools map[string][]bool
	trace []string
}

func newCountingHandler() *countingHandler {
	return &countingHandler{
		counts: make(map[string]int),
		sqls:   make(map[string][]string),
		ints:   make(map[string][]int64),
		bools:  make(map[string][]bool),
	}
}

// teeLogsEnv makes the captured records visible. The handler swallows
// everything by design — the integration fixture hands it to the proxy as the
// *only* logger — so a failing session leaves no trace of what the proxy
// decoded. Setting it to 1 tees every record to stderr, which is how the
// bundled-OCI-client refusal was diagnosed:
//
//	ORACLE_TEST_LOG=1 ORACLE_TEST_OCI_CLIENT=container go test -tags integration ...
const teeLogsEnv = "ORACLE_TEST_LOG"

// teeLogs is read once: os.Getenv on every record would show up on a suite that
// logs a row per fetched row.
var teeLogs = os.Getenv(teeLogsEnv) == "1"

func (h *countingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *countingHandler) Handle(_ context.Context, rec slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	if teeLogs {
		out := rec.Level.String() + " " + rec.Message

		rec.Attrs(func(a slog.Attr) bool {
			out += " " + a.Key + "=" + a.Value.String()

			return true
		})

		fmt.Fprintln(os.Stderr, out)
	}

	h.counts[rec.Message]++

	line := rec.Message

	rec.Attrs(func(a slog.Attr) bool {
		if a.Key == "sql" {
			h.sqls[rec.Message] = append(h.sqls[rec.Message], a.Value.String())
		}

		if a.Value.Kind() == slog.KindInt64 {
			key := rec.Message + "\x00" + a.Key
			h.ints[key] = append(h.ints[key], a.Value.Int64())
		}

		if a.Value.Kind() == slog.KindBool {
			key := rec.Message + "\x00" + a.Key
			h.bools[key] = append(h.bools[key], a.Value.Bool())
		}

		if a.Key == "sql" || a.Key == "cursor_id" {
			line += " | " + a.Key + "=" + a.Value.String()
		}

		return true
	})

	switch rec.Message {
	case logMsgQueryIntercepted, logMsgLearnedCursorID, logMsgReexecGated,
		logMsgUntrackedCursorForwarded, logMsgUntrackedCursorRefused:
		h.trace = append(h.trace, line)
	}

	return nil
}

func (h *countingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *countingHandler) WithGroup(string) slog.Handler      { return h }

func (h *countingHandler) count(msg string) int {
	h.mu.Lock()
	defer h.mu.Unlock()

	return h.counts[msg]
}

func (h *countingHandler) sqlsFor(msg string) []string {
	h.mu.Lock()
	defer h.mu.Unlock()

	out := make([]string, len(h.sqls[msg]))
	copy(out, h.sqls[msg])

	return out
}

// intsFor returns every value the named integer attribute carried on the named
// message, in the order they were logged.
func (h *countingHandler) intsFor(msg, attr string) []int64 {
	h.mu.Lock()
	defer h.mu.Unlock()

	values := h.ints[msg+"\x00"+attr]
	out := make([]int64, len(values))
	copy(out, values)

	return out
}

// boolsFor returns every value the named boolean attribute carried on the named
// message, in the order they were logged.
func (h *countingHandler) boolsFor(msg, attr string) []bool {
	h.mu.Lock()
	defer h.mu.Unlock()

	values := h.bools[msg+"\x00"+attr]
	out := make([]bool, len(values))
	copy(out, values)

	return out
}

// maxIntFor returns the largest value the named integer attribute reached, and
// whether it was ever seen at all — an assertion on a bound that was never
// recorded would pass on nothing.
func (h *countingHandler) maxIntFor(msg, attr string) (int64, bool) {
	values := h.intsFor(msg, attr)
	if len(values) == 0 {
		return 0, false
	}

	most := values[0]
	for _, v := range values[1:] {
		if v > most {
			most = v
		}
	}

	return most, true
}

func (h *countingHandler) traceLines() []string {
	h.mu.Lock()
	defer h.mu.Unlock()

	out := make([]string, len(h.trace))
	copy(out, h.trace)

	return out
}

// multisetDiff returns the elements of a that b does not account for,
// respecting multiplicity.
func multisetDiff(a, b []string) []string {
	remaining := make(map[string]int, len(b))
	for _, s := range b {
		remaining[s]++
	}

	var out []string

	for _, s := range a {
		if remaining[s] > 0 {
			remaining[s]--

			continue
		}

		out = append(out, s)
	}

	sort.Strings(out)

	return out
}

// TestCountingHandlerWatchesTheMessagesTheGateEmits is the guard on the
// measurement, and it exists because the measurement already rotted once.
//
// The cursor-id learning numbers in docs/oracle.md rest on a counter reading
// zero untracked re-executions. When handlePiggybackReexec was routed through
// refuseUnknownCursor, its own "forwarding a piggyback re-execution…" WARN went
// away — and the counter kept watching for that dead string, so the headline
// metric became structurally incapable of being non-zero while still being
// cited as evidence. A metric that reports 0/N forever is worse than no metric.
//
// Naming the messages in intercept.go makes the compiler catch a reword. This
// catches the other half: that an untracked cursor really does produce a record
// the counter is watching, on both sides of the statement-controls branch.
func TestCountingHandlerWatchesTheMessagesTheGateEmits(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		grant    *store.Grant
		wantMsg  string
		wantErr  error
		otherMsg string
	}{
		{
			name:     "no statement-shaped control: forwarded, and counted",
			grant:    &store.Grant{Definition: &store.GrantDefinition{}},
			wantMsg:  logMsgUntrackedCursorForwarded,
			otherMsg: logMsgUntrackedCursorRefused,
		},
		{
			name: "read_only: refused, and counted",
			grant: &store.Grant{
				Definition: &store.GrantDefinition{Controls: []string{store.ControlReadOnly}},
			},
			wantMsg:  logMsgUntrackedCursorRefused,
			wantErr:  ErrUnknownCursor,
			otherMsg: logMsgUntrackedCursorForwarded,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			logs := newCountingHandler()

			s := newTestSession(tc.grant)
			s.logger = slog.New(logs)
			s.clientConn = drainedPipe(t)

			// A cursor id nothing on this session ever parsed.
			err := s.handlePiggybackReexec(buildPiggybackReexec(0xFFF0))

			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
			} else {
				require.NoError(t, err)
			}

			assert.Equal(t, 1, logs.count(tc.wantMsg),
				"the measurement counter must see an untracked cursor on this branch")
			assert.Zero(t, logs.count(tc.otherMsg), "and only on this branch")

			// The trace the measurement dumps must carry it too, or a
			// mis-resolution investigation would come up empty.
			assert.Len(t, logs.traceLines(), 1)
		})
	}
}

// TestCountingHandlerCapturesTheResolvedStatement guards the other counter the
// measurement leans on: the SQL a re-execution resolved to. That is what the
// mis-learning checks read — the interleaved-cursor histogram and the
// "resolved to a statement this client never ran" assertion — and both would go
// quietly vacuous if the `sql` attribute stopped being captured.
func TestCountingHandlerCapturesTheResolvedStatement(t *testing.T) {
	t.Parallel()

	logs := newCountingHandler()

	s := newTestSession(&store.Grant{Definition: &store.GrantDefinition{}})
	s.logger = slog.New(logs)
	s.clientConn = drainedPipe(t)
	s.tracker.cursors[42] = &trackedCursor{cursorID: 42, sql: "SELECT 1 AS n FROM dual"}

	require.NoError(t, s.handlePiggybackReexec(buildPiggybackReexec(42)))

	assert.Equal(t, 1, logs.count(logMsgReexecGated))
	assert.Equal(t, []string{"SELECT 1 AS n FROM dual"}, logs.sqlsFor(logMsgReexecGated),
		"the measurement resolves mis-learned cursor ids by reading this back")
	assert.Zero(t, logs.count(logMsgUntrackedCursorForwarded), "the cursor was tracked")
}

// TestMultisetDiff pins the helper the measurement uses to name the parses that
// never learned a cursor id — the assertion that catches a learning miss even
// when a recycled cursor id hides it behind a stale tracker entry.
func TestMultisetDiff(t *testing.T) {
	t.Parallel()

	assert.Equal(t,
		[]string{"a", "b"},
		multisetDiff([]string{"a", "b", "c", "c"}, []string{"c", "c"}))
	assert.Empty(t, multisetDiff([]string{"a"}, []string{"a", "a"}))
	assert.Equal(t, []string{"a"}, multisetDiff([]string{"a", "a"}, []string{"a"}),
		"multiplicity matters: two parses and one learn is one miss")
}
