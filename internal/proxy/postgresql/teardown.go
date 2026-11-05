package postgresql

import (
	"errors"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	// relayDrainTimeout bounds how long the teardown waits for the second relay
	// leg once the first has ended and both sockets are closed. A leg parked on
	// a socket returns at once; the bound only matters for one that is not (a
	// client leg waiting on an approval hold), and the teardown runs under
	// bookMu either way, so giving up on the wait is safe, merely less tidy.
	relayDrainTimeout = 2 * time.Second

	// persistDrainTimeout bounds how long cleanup waits for the background
	// query records before it closes the connection row. The records still
	// land after the bound; the wait only makes them land *first*.
	persistDrainTimeout = 5 * time.Second

	// errClientDisconnectedMidResponse completes a statement that was still in
	// flight when the client closed its socket.
	errClientDisconnectedMidResponse = "client disconnected before the response completed"

	// errSessionEndedMidResponse completes a statement that was still in
	// flight when the session ended for any other reason (the upstream went
	// away, a relay failed).
	errSessionEndedMidResponse = "aborted: session ended before the response completed"
)

// persistTracker counts the query records persistQueryAsync is still writing,
// so cleanup can let them land before it closes the connection row. A
// WaitGroup would do, except that a straggling relay may start a record while
// cleanup is already waiting, which WaitGroup forbids.
type persistTracker struct {
	mu      sync.Mutex
	n       int
	waiters []chan struct{}
}

func (p *persistTracker) start() {
	p.mu.Lock()
	p.n++
	p.mu.Unlock()
}

func (p *persistTracker) done() {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.n--
	if p.n > 0 {
		return
	}

	for _, c := range p.waiters {
		close(c)
	}

	p.waiters = nil
}

// wait returns once no record is being written, or after timeout, and reports
// which.
func (p *persistTracker) wait(timeout time.Duration) bool {
	p.mu.Lock()
	if p.n == 0 {
		p.mu.Unlock()

		return true
	}

	idle := make(chan struct{})
	p.waiters = append(p.waiters, idle)
	p.mu.Unlock()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-idle:
		return true
	case <-timer.C:
		return false
	}
}

// finishRelays runs once the first relay leg has ended with err: it ends the
// other leg, gives it a bounded moment to leave the bookkeeping, then
// completes whatever the session still had in flight and flushes the bytes no
// statement took.
//
// Without it a client that reads part of a large result and closes its socket
// left the statement reading as still running and its streamed bytes out of
// the store, so a reconnect recomputed the grant's byte budget without them:
// stream just under the cap, disconnect, reconnect, repeat.
func (s *Session) finishRelays(err error, errChan <-chan error) {
	s.closeConns()

	timer := time.NewTimer(relayDrainTimeout)
	defer timer.Stop()

	select {
	case second := <-errChan:
		// The second leg ends on the sockets the teardown just closed; its
		// error says nothing the first one did not.
		_ = second
	case <-timer.C:
		s.logger.WarnContext(s.ctx, "relay leg did not end after both sockets closed")
	}

	s.completeInFlightOnTeardown(err)
}

// completeInFlightOnTeardown completes every statement still in flight when the
// session ends, with an error naming why, and attributes the bytes exchanged
// since the last completed statement — to the statement being answered, or
// straight to the connection when nothing was in flight (the last
// ReadyForQuery and the client's Terminate are always such trailing bytes).
func (s *Session) completeInFlightOnTeardown(cause error) {
	// A watchdog termination racing the hangup keeps its own text: it
	// completes everything itself, which leaves nothing for the rest of this.
	s.persistTerminatedQuery(s.recordedTermination())

	reason := errSessionEndedMidResponse
	if errors.Is(cause, errClientHungUp) {
		reason = errClientDisconnectedMidResponse
	}

	s.bookMu.Lock()

	if oldest := s.oldestPendingQuery(); oldest != nil {
		s.completeInFlightLocked(oldest, reason, reason)
		s.bookMu.Unlock()

		return
	}

	total := s.bytesFromClient.Load() + s.bytesToClient.Load()

	trailing := total - s.lastBytesSnapshot
	if trailing > 0 {
		s.lastBytesSnapshot = total

		if s.grant != nil {
			s.grant.BytesTransferred += trailing
		}
	}

	s.bookMu.Unlock()

	if trailing <= 0 || s.store == nil || s.connectionUID == uuid.Nil {
		return
	}

	// Bytes only: a trailing flush is not a query and must not count as one.
	if err := s.store.IncrementConnectionBytes(s.ctx, s.connectionUID, trailing); err != nil {
		s.logger.ErrorContext(s.ctx, "failed to flush trailing connection bytes", slog.Any("error", err))
	}
}

// completeInFlightLocked completes every in-flight statement — the staged
// simple Query and every queued Execute — and drops the bookkeeping. primary
// is completed last with primaryErr, every other entry with othersErr. The
// bytes since the last completed statement go to the entry being answered
// (getCurrentPendingQuery), the one whose response was streaming.
//
// Callers hold bookMu.
func (s *Session) completeInFlightLocked(primary *pendingQuery, primaryErr, othersErr string) {
	total := s.bytesFromClient.Load() + s.bytesToClient.Load()

	bytesTransferred := total - s.lastBytesSnapshot
	if bytesTransferred > 0 {
		s.lastBytesSnapshot = total
	} else {
		// Nothing new to attribute. The entries are still completed — a row a
		// result capture already inserted must not read as still running —
		// they just carry no byte count.
		bytesTransferred = 0
	}

	answered := s.getCurrentPendingQuery()

	var entries []*pendingQuery

	if s.extendedState != nil {
		for _, q := range s.extendedState.pendingQueries {
			if q != nil {
				entries = append(entries, q)
			}
		}
	}

	if s.currentQuery != nil && !slices.Contains(entries, s.currentQuery) {
		entries = append(entries, s.currentQuery)
	}

	bytesFor := func(q *pendingQuery) int64 {
		if q == answered {
			return bytesTransferred
		}

		return 0
	}

	for _, q := range entries {
		if q == primary {
			continue
		}

		others := othersErr
		s.currentQuery = q
		s.logQuery(nil, &others, bytesFor(q))
	}

	s.currentQuery = primary
	s.logQuery(nil, &primaryErr, bytesFor(primary))

	s.currentQuery = nil
	s.copyState = nil

	if s.extendedState != nil {
		s.extendedState.pendingQueries = nil
	}

	s.refreshStatementClock()
}
