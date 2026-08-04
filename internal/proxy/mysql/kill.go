package mysql

import (
	"regexp"
	"strconv"
	"sync"
)

// killQueryPattern matches MySQL's KILL [CONNECTION|QUERY] <id>. It runs on
// every statement the proxy sees, so it is anchored and cheap.
var killQueryPattern = regexp.MustCompile(`(?i)^\s*KILL\s+(?:CONNECTION\s+|QUERY\s+)?(\d+)\s*;?\s*$`)

// sessionRegistry maps the connection ids dbbat hands MySQL clients to the
// sessions behind them, so a KILL QUERY issued from a *second* connection can
// reach a statement parked on a human.
//
// This is the MySQL counterpart of PostgreSQL's CancelRequest: a held query
// must remain cancellable, and the held session's own socket is by definition
// not going to carry the cancel.
type sessionRegistry struct {
	mu       sync.Mutex
	sessions map[uint32]*Session
}

func newSessionRegistry() *sessionRegistry {
	return &sessionRegistry{sessions: make(map[uint32]*Session)}
}

func (r *sessionRegistry) register(id uint32, s *Session) {
	if r == nil {
		return
	}

	r.mu.Lock()
	r.sessions[id] = s
	r.mu.Unlock()
}

func (r *sessionRegistry) unregister(id uint32) {
	if r == nil {
		return
	}

	r.mu.Lock()
	delete(r.sessions, id)
	r.mu.Unlock()
}

func (r *sessionRegistry) lookup(id uint32) (*Session, bool) {
	if r == nil {
		return nil, false
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	s, ok := r.sessions[id]

	return s, ok
}

// parseKillTarget returns the connection id a KILL statement targets.
func parseKillTarget(sql string) (uint32, bool) {
	m := killQueryPattern.FindStringSubmatch(sql)
	if m == nil {
		return 0, false
	}

	id, err := strconv.ParseUint(m[1], 10, 32)
	if err != nil {
		return 0, false
	}

	return uint32(id), true
}

// releaseHoldForKill ends any approval hold on the targeted session. It returns
// whether a hold was released; the KILL statement itself still travels upstream
// so a genuinely running query is killed too.
func (s *Session) releaseHoldForKill(sql string) bool {
	id, ok := parseKillTarget(sql)
	if !ok {
		return false
	}

	if s.server == nil {
		return false
	}

	target, ok := s.server.sessions.lookup(id)
	if !ok {
		return false
	}

	return target.KillHeldQuery()
}
