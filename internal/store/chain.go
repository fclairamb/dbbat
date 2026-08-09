package store

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"
)

// The tamper-evident chains.
//
// audit_log is one chain for the whole store; queries is one chain per
// connection. Both are HMAC-SHA256 over a canonical serialization of the row
// plus the previous row's MAC (see chain_canonical.go), keyed by a subkey
// derived from DBB_KEY with HKDF. The key lives only in this process: an
// attacker with full write access to PostgreSQL can still rewrite history, but
// cannot produce MACs that verify, so `dbbat audit verify` names the first row
// that was touched.
//
// What the chain does not do: it does not *prevent* tampering, it does not
// protect rows written before the anchor, and on the queries side it covers a
// statement's immutable identity only — see queryChainPayload.

const (
	// AuditChainAnchorUID is the fixed uid of the marker row the migration
	// inserts to record where chaining begins. It is chain_seq 0 and carries no
	// MAC: it is a signpost, not a link.
	AuditChainAnchorUID = "00000000-0000-7000-8000-0000dbba7000"

	// AuditChainAnchorEventType is that row's event_type.
	AuditChainAnchorEventType = "audit.chain_anchor"

	auditChainDomain = "dbbat-audit-chain-v1"
	queryChainDomain = "dbbat-query-chain-v1"

	auditGenesisLabel = "dbbat-audit-chain-genesis-v1"
	queryGenesisLabel = "dbbat-query-chain-genesis-v1:"
)

// auditChainAdvisoryLockKey serializes appends to the audit chain across every
// replica sharing this database. Deliberately distinct from
// migrationAdvisoryLockKey; both use the single-argument advisory lock space.
const auditChainAdvisoryLockKey int64 = 0x4442424154415544

// queryChainAdvisoryLockClass is the class id of the two-argument advisory
// locks that serialize appends to a connection's query chain. PostgreSQL keeps
// the two-argument locks in their own space, so this cannot collide with the
// keys above. The object id is derived from the connection uid: a collision
// between two connections only serializes them against each other, which is
// harmless.
const queryChainAdvisoryLockClass int32 = 0x44424251

// ErrChainKeyUnavailable is returned by the verifiers when the store has no
// chain key. Verification without the key is impossible by design.
var ErrChainKeyUnavailable = errors.New("audit chain key is not configured")

// chainState is one chain's head, cached so the common case does not re-read
// it. The mutex is held for the whole append — including the database round
// trip — because the head is a serialization point: two appends that read the
// same head would write the same chain_seq and the same prev_mac, and one of
// them would be lost. The advisory lock does the same job across processes.
type chainState struct {
	mu     sync.Mutex
	loaded bool
	seq    int64
	mac    []byte
}

// invalidate forgets the cached head so the next append re-reads it. Called
// whenever an append fails: after a failed transaction we no longer know
// whether the row landed.
func (c *chainState) invalidate() {
	c.loaded = false
	c.seq = 0
	c.mac = nil
}

// queryChains holds one chainState per live connection. Entries are dropped
// when the connection closes (Store.CloseConnection); an entry left behind by a
// session that died without closing is a few dozen bytes, and the next process
// does not inherit it.
type queryChains struct {
	mu sync.Mutex
	m  map[uuid.UUID]*chainState
}

func newQueryChains() *queryChains {
	return &queryChains{m: make(map[uuid.UUID]*chainState)}
}

func (q *queryChains) get(connectionUID uuid.UUID) *chainState {
	q.mu.Lock()
	defer q.mu.Unlock()

	state, ok := q.m[connectionUID]
	if !ok {
		state = &chainState{}
		q.m[connectionUID] = state
	}

	return state
}

func (q *queryChains) forget(connectionUID uuid.UUID) {
	q.mu.Lock()
	defer q.mu.Unlock()

	delete(q.m, connectionUID)
}

// ChainEnabled reports whether this store seals what it writes. It is false
// only when no encryption key was handed to the store, which in a served
// process cannot happen: config always resolves a key, creating ~/.dbbat/key if
// it has to.
func (s *Store) ChainEnabled() bool {
	return len(s.chainKey) > 0
}

// SetChainKey installs the HMAC key that seals the audit and query chains. It
// is the derived subkey, never the master key — see crypto.DeriveAuditChainKey.
// Passing nil disables chaining, which is what a store built without an
// encryption key does.
func (s *Store) SetChainKey(key []byte) {
	s.chainKey = key
}

func (s *Store) chainMAC(payload []byte) []byte {
	mac := hmac.New(sha256.New, s.chainKey)
	mac.Write(payload)

	return mac.Sum(nil)
}

// auditGenesisMAC is the prev_mac of chain_seq 1. Deriving it from the key
// rather than using NULL is what makes deleting the earliest chained entries
// detectable: the survivor's prev_mac would have to equal a value the attacker
// cannot compute.
func (s *Store) auditGenesisMAC() []byte {
	return s.chainMAC([]byte(auditGenesisLabel))
}

// queryGenesisMAC is the per-connection equivalent, bound to the connection so
// a chain cannot be transplanted from one connection to another.
func (s *Store) queryGenesisMAC(connectionUID uuid.UUID) []byte {
	return s.chainMAC([]byte(queryGenesisLabel + connectionUID.String()))
}

// auditChainPayload is the canonical serialization the audit MAC covers.
// audit_log rows are insert-only, so the MAC covers every column.
func auditChainPayload(
	seq int64,
	uid uuid.UUID,
	eventType string,
	userID, performedBy *uuid.UUID,
	details json.RawMessage,
	createdAt time.Time,
	prevMAC []byte,
) ([]byte, error) {
	canonicalDetails, err := canonicalJSON(details)
	if err != nil {
		return nil, err
	}

	p := newCanonicalPayload(auditChainDomain)
	p.writeInt("chain_seq", seq)
	p.writeString("uid", uid.String())
	p.writeString("event_type", eventType)
	p.writeOptional("user_id", optionalUUID(userID))
	p.writeOptional("performed_by", optionalUUID(performedBy))
	p.writeOptional("details", canonicalDetails)
	p.writeString("created_at", canonicalTime(createdAt))
	p.writeBytes("prev_mac", prevMAC)

	return p.bytes(), nil
}

// queryChainPayload is the canonical serialization the per-connection query MAC
// covers.
//
// It deliberately covers only the immutable identity of the statement: uid,
// connection, position, SQL text, bound parameters and execution time. The
// outcome columns (duration_ms, rows_affected, error, and the approval
// resolution) are written *after* the row is inserted, and a MAC that covered
// them would have to be recomputed — which would invalidate every successor's
// prev_mac. The chain therefore proves which statements ran, with which
// parameters, in which order; it does not seal their reported outcome. See
// docs/audit-chain.md.
func queryChainPayload(
	seq int64,
	uid, connectionUID uuid.UUID,
	sqlText string,
	parameters json.RawMessage,
	executedAt time.Time,
	prevMAC []byte,
) ([]byte, error) {
	canonicalParams, err := canonicalJSON(parameters)
	if err != nil {
		return nil, err
	}

	p := newCanonicalPayload(queryChainDomain)
	p.writeString("connection_id", connectionUID.String())
	p.writeInt("chain_seq", seq)
	p.writeString("uid", uid.String())
	p.writeString("sql_text", sqlText)
	p.writeOptional("parameters", canonicalParams)
	p.writeString("executed_at", canonicalTime(executedAt))
	p.writeBytes("prev_mac", prevMAC)

	return p.bytes(), nil
}

func optionalUUID(id *uuid.UUID) []byte {
	if id == nil {
		return nil
	}

	return []byte(id.String())
}

// queryChainLockID maps a connection uid onto the object id of its advisory
// lock. Only the first four bytes are used: the uid is a UUIDv7 whose leading
// bytes are a timestamp, so this is not a good hash — but it does not need to
// be, because a collision costs two unrelated connections a little serialization
// and nothing else.
func queryChainLockID(connectionUID uuid.UUID) int32 {
	return int32(binary.BigEndian.Uint32(connectionUID[8:12]))
}
