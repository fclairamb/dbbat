package mcp

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/fclairamb/dbbat/internal/events"
)

// Execution lifetime bounds.
const (
	// DefaultGraceWindow is how long `query` waits before handing the agent an
	// execution id instead of rows. The spec's "~10s": long enough that an
	// ordinary statement never pays the extra round-trip, short enough that a
	// held one does not look like a hang.
	DefaultGraceWindow = 10 * time.Second

	// DefaultAwaitWindow is how long `await_approval` blocks per call before
	// answering "still pending". It must stay comfortably under any
	// intermediary's idle timeout: an answered poll is cheap, a dropped
	// request is the silent timeout this whole mechanism exists to avoid.
	DefaultAwaitWindow = 55 * time.Second

	// MaxAwaitWindow bounds what an agent may ask for in one await call.
	MaxAwaitWindow = 120 * time.Second

	// ExecutionMaxLifetime is the hard bound on a backgrounded execution. A
	// hold has no timeout by design (docs/approvals.md), but an MCP execution
	// nobody ever comes back for must not park an upstream connection
	// forever; when it fires, the loopback socket closes and the hold ends as
	// `abandoned` — the ordinary "the client gave up" outcome.
	ExecutionMaxLifetime = 30 * time.Minute

	// executionReapGrace is how long a *finished* execution stays collectable
	// so a late await_approval still gets its rows instead of a mystery.
	executionReapGrace = 10 * time.Minute
)

// ErrUnknownExecution means the execution id is not (or no longer) on this
// replica. Returned rather than guessed at: an agent must be told to look the
// query up rather than be handed a wrong answer.
var ErrUnknownExecution = errors.New("unknown execution id")

// execution is one statement the MCP layer started on a loopback connection
// and may still be waiting on.
//
// It outlives the HTTP request that started it. That is the whole point: an
// approval hold blocks the wire connection for as long as it takes a human to
// decide, and the MCP request cannot stay open that long.
type execution struct {
	id        string
	ownerUID  uuid.UUID
	database  string
	sql       string
	startedAt time.Time

	cancel context.CancelFunc

	// done is closed once result/err are final. Everything below it is
	// written exactly once, before the close, and read only after.
	done     chan struct{}
	result   *QueryResult
	err      error
	duration time.Duration

	// finishedAt is when done was closed, used for reaping. Guarded by the
	// registry's mutex.
	finishedAt time.Time

	mu sync.Mutex
	// queryUID and pattern are filled in by the hold correlator when the
	// statement is parked. Best effort: they name the hold for a human, they
	// are never what decides whether the statement ran.
	queryUID string
	pattern  string
	held     bool
}

// finish records the outcome exactly once.
func (e *execution) finish(result *QueryResult, err error) {
	e.result = result
	e.err = err
	e.duration = time.Since(e.startedAt)
	close(e.done)
}

// durationMs is how long the statement actually took, including any time it
// spent parked on a human. Only meaningful once the execution has finished.
func (e *execution) durationMs() int64 {
	return e.duration.Milliseconds()
}

// noteHold records that this execution's statement was parked.
func (e *execution) noteHold(queryUID, pattern string) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.held {
		return
	}

	e.held = true
	e.queryUID = queryUID
	e.pattern = pattern
}

// holdInfo is what the correlator discovered about a parked statement.
type holdInfo struct {
	QueryUID string
	Pattern  string
	Held     bool
}

// hold reports the parked-statement identity discovered so far.
func (e *execution) hold() holdInfo {
	e.mu.Lock()
	defer e.mu.Unlock()

	return holdInfo{QueryUID: e.queryUID, Pattern: e.pattern, Held: e.held}
}

// executions is the process-local registry of backgrounded executions.
type executions struct {
	mu sync.Mutex
	m  map[string]*execution
	// claimed prevents two concurrent executions of the same statement by the
	// same user from both adopting one hold's query uid.
	claimed map[string]struct{}
}

func newExecutions() *executions {
	return &executions{m: make(map[string]*execution), claimed: make(map[string]struct{})}
}

func (x *executions) add(e *execution) {
	x.mu.Lock()
	defer x.mu.Unlock()

	x.m[e.id] = e
}

// get returns an execution owned by ownerUID. An execution belonging to
// somebody else is reported as unknown, not as forbidden: the id space must
// not be probeable.
func (x *executions) get(id string, ownerUID uuid.UUID) (*execution, error) {
	x.mu.Lock()
	defer x.mu.Unlock()

	e, ok := x.m[id]
	if !ok || e.ownerUID != ownerUID {
		return nil, ErrUnknownExecution
	}

	return e, nil
}

// claim records a hold uid against an execution, refusing a uid another
// execution already adopted.
func (x *executions) claim(queryUID string) bool {
	x.mu.Lock()
	defer x.mu.Unlock()

	if _, dup := x.claimed[queryUID]; dup {
		return false
	}

	x.claimed[queryUID] = struct{}{}

	return true
}

// markFinished timestamps a completed execution for reaping.
func (x *executions) markFinished(e *execution) {
	x.mu.Lock()
	defer x.mu.Unlock()

	e.finishedAt = time.Now()
}

// reap drops finished executions past the collection grace period. Live
// executions are left alone — their own context deadline
// (ExecutionMaxLifetime) is what ends them.
func (x *executions) reap(now time.Time) {
	x.mu.Lock()
	defer x.mu.Unlock()

	for id, e := range x.m {
		if e.finishedAt.IsZero() || now.Sub(e.finishedAt) < executionReapGrace {
			continue
		}

		delete(x.m, id)

		e.mu.Lock()
		uid := e.queryUID
		e.mu.Unlock()

		if uid != "" {
			delete(x.claimed, uid)
		}
	}
}

// watchHold subscribes to the approval broker and adopts the first pending
// hold that matches this execution.
//
// Correlation is by (user, database, statement) rather than by connection uid,
// because the loopback client never learns the connection uid dbbat assigned
// it — the proxy mints it server-side and nothing on the wire carries it back.
// The window is narrow (this execution's own lifetime), the claim is exclusive
// (see executions.claim), and being wrong costs an agent a mislabelled query
// uid in an advisory message — never a statement that runs when it should not.
func watchHold(ctx context.Context, broker *events.Broker, x *executions, e *execution) {
	if broker == nil {
		return
	}

	sub := broker.Subscribe(func(string, *events.Event) bool { return true }, 0)
	defer sub.Close()

	if !sub.Subscribe(events.TopicApprovalsPending) {
		return
	}

	want := strings.TrimSpace(e.sql)

	for {
		select {
		case <-ctx.Done():
			return
		case <-e.done:
			return
		case ev, ok := <-sub.Events():
			if !ok {
				return
			}

			if !matchesHold(ev, e, want) {
				continue
			}

			queryUID, _ := ev.Data["query_uid"].(string)
			if queryUID == "" || !x.claim(queryUID) {
				continue
			}

			pattern, _ := ev.Data["approval_pattern"].(string)
			e.noteHold(queryUID, pattern)

			return
		}
	}
}

// matchesHold reports whether a pending-approval event describes this
// execution's statement.
func matchesHold(ev events.Event, e *execution, wantSQL string) bool {
	if ev.Type != events.EventApprovalPending {
		return false
	}

	if userUID, _ := ev.Data["user_uid"].(string); userUID != e.ownerUID.String() {
		return false
	}

	if db, _ := ev.Data["database_name"].(string); db != e.database {
		return false
	}

	sqlText, _ := ev.Data["sql_text"].(string)

	return strings.TrimSpace(sqlText) == wantSQL
}
