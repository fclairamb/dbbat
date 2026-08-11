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
	rowChainDomain   = "dbbat-row-chain-v1"
	rowStampDomain   = "dbbat-row-chain-stamp-v1"
	queryStampDomain = "dbbat-query-chain-stamp-v1"

	auditGenesisLabel = "dbbat-audit-chain-genesis-v1"
	queryGenesisLabel = "dbbat-query-chain-genesis-v1:"
	rowGenesisLabel   = "dbbat-row-chain-genesis-v1:"
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

// rowChainAdvisoryLockClass is the same idea for the per-query chains over
// captured result rows. Its own class, so a query uid and a connection uid
// mapping to the same object id never serialize against each other.
//
// A collision between two captures costs them a little serialization against
// each other. It is not *entirely* free the way the query-chain one is: a row
// batch takes several of these locks at once, in query-uid order, and a
// collision can make two batches disagree about the effective order of the ids
// they are taking — so two overlapping batches could in principle deadlock.
// PostgreSQL's deadlock detector aborts one of them and chainAppendAttempts
// retries it, so the outcome is a retry rather than a lost batch.
const rowChainAdvisoryLockClass int32 = 0x44425251

// chainAppendAttempts bounds the retries an append makes when its cached head
// turns out to be stale — the multi-replica case, where a peer extended the
// chain between two of this process's appends. Each retry re-reads the head
// under the advisory lock, so one is normally enough; the extra attempts cover
// a burst of peers appending at once.
const chainAppendAttempts = 3

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

	// count is how many rows the chain holds. For the audit and query chains
	// it is the same number as seq — those positions are dense — so only the
	// row chains, whose position (row_number) may have gaps, read it. It is
	// what row_chain_len is stamped from.
	count int64
}

// invalidate forgets the cached head so the next append re-reads it. Called
// whenever an append fails: after a failed transaction we no longer know
// whether the row landed.
func (c *chainState) invalidate() {
	c.loaded = false
	c.seq = 0
	c.mac = nil
	c.count = 0
}

// queryChains holds one chainState per key: per live connection for the
// statement chains, per capturing query for the result-row chains. Entries are
// dropped when the chain is sealed (Store.CloseConnection,
// Store.SealQueryRowChain); an entry left behind by a session that died is a
// few dozen bytes, and the next process does not inherit it.
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

// lookup finds an existing chain without creating one. It is what lets a
// sealing path answer "did this process write anything to that chain?" without
// a database round trip: no entry means nothing to seal.
func (q *queryChains) lookup(key uuid.UUID) (*chainState, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()

	state, ok := q.m[key]

	return state, ok
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

// rowGenesisMAC is the prev_mac of the first stored row of a capture, bound to
// the query so a chain cannot be transplanted from one capture to another.
//
// Unlike the query chain, a missing genesis link *is* a break: retention never
// deletes individual query_rows (it deletes whole queries, which cascades), so
// nothing legitimate ever removes a capture's earliest rows.
func (s *Store) rowGenesisMAC(queryUID uuid.UUID) []byte {
	return s.chainMAC([]byte(rowGenesisLabel + queryUID.String()))
}

// rowChainPayload is the canonical serialization the per-query result-row MAC
// covers. A query_rows row is insert-only — nothing is ever written to it after
// the INSERT — so the MAC covers every column of it.
//
// What it deliberately does not reach is the capture's own after-the-fact
// metadata on the parent query: results_truncated and results_dropped are
// written by UpdateQueryCompletion once the result set has been read, exactly
// like duration_ms and rows_affected, and sealing them would mean recomputing a
// MAC whose successors already point at it. That is the same call
// queryChainPayload made for the query outcome columns. So the row chain proves
// **which rows were captured, with what content, in what order**; it does not
// seal the claim that the capture was complete.
func rowChainPayload(
	queryUID, uid uuid.UUID,
	rowNumber int,
	rowData []byte,
	rowSizeBytes int64,
	prevMAC []byte,
) ([]byte, error) {
	canonicalData, err := canonicalJSON(rowData)
	if err != nil {
		return nil, err
	}

	p := newCanonicalPayload(rowChainDomain)
	p.writeString("query_id", queryUID.String())
	p.writeInt("row_number", int64(rowNumber))
	p.writeString("uid", uid.String())
	p.writeOptional("row_data", canonicalData)
	p.writeInt("row_size_bytes", rowSizeBytes)
	p.writeBytes("prev_mac", prevMAC)

	return p.bytes(), nil
}

// rowChainStampMAC seals a finished capture's head: how many rows the chain
// holds and the MAC it ends on, bound to the query.
//
// It is a *keyed* stamp, and that is the whole point. Storing the head MAC
// verbatim — which is what connections.query_chain_mac did before 0.24 — would
// not survive the threat model this feature exists for: the head MAC is readable straight
// out of query_rows, so an attacker with write access to the store could delete
// the last captured rows and then copy the new last row's MAC into the stamp,
// with no key and no break reported. Sealing the stamp instead means correcting
// it after a deletion requires the chain key, exactly like forging a row.
//
// The per-connection stamp is sealed the same way, one version later — see
// queryChainStampMAC.
func (s *Store) rowChainStampMAC(queryUID uuid.UUID, count int64, headMAC []byte) []byte {
	p := newCanonicalPayload(rowStampDomain)
	p.writeString("query_id", queryUID.String())
	p.writeInt("row_chain_len", count)
	p.writeBytes("head_mac", headMAC)

	return s.chainMAC(p.bytes())
}

// Stamp formats for connections.query_chain_mac.
//
// 0.23.x wrote the head MAC verbatim, which defends against nothing: the head
// is readable out of `queries`, so an attacker who deleted the tail of a
// session could copy the new last statement's MAC over the stamp and get a
// clean `dbbat audit verify --queries`, with no key. Version 1 is a keyed
// stamp, so correcting it after a deletion needs what an attacker does not
// have.
//
// Version 0 rows still exist — the chain key lives only in this process, so no
// migration can re-seal what is already stored — but since 0.24 they are a
// break, not a weaker pass. Accepting them was a standing downgrade path: the
// stamp is readable out of `queries`, so replacing a sealed row with a raw head
// MAC and version `0` needed no key at all. `--allow-legacy-stamps`
// (store.AllowLegacyStamps) restores the counting behaviour for the one upgrade
// that has such rows, and goes away in 0.25.
const (
	// queryChainStampLegacy is the pre-0.24 verbatim head copy. Unkeyed, and
	// forgeable by anyone who can write to the store.
	queryChainStampLegacy int16 = 0

	// queryChainStampKeyed is the keyed stamp, and the version every writer
	// produces from now on.
	queryChainStampKeyed int16 = 1
)

// queryChainStampMAC seals a finished session's head: which stamp format it is
// in, how many statements the chain holds, and the MAC it ends on, bound to the
// connection.
//
// The version is *inside* the MAC, and that is the reason the column exists at
// all rather than the verifier simply accepting either format. A stamp that did
// not cover its own version could be relabelled as legacy — one UPDATE, no key
// — and would then be checked against the unkeyed rule it was written to
// replace. Covering it means a keyed stamp only ever verifies as the version it
// was sealed at.
//
// count is the head's chain_seq, not the number of surviving statements.
// chain_seq is dense from 1, so it is the number of statements the session
// logged — and unlike a survivor count it does not move when
// DBB_QUERY_STORAGE_RETENTION reaps the oldest statements of a long-lived
// session. Sealing a survivor count would report a break on every
// retention-truncated session, which is precisely what QueryChainResult
// .TruncatedPrefix exists to avoid. (The row-chain stamp can seal a true count
// because retention never deletes individual captured rows.)
func (s *Store) queryChainStampMAC(connectionUID uuid.UUID, version int16, count int64, headMAC []byte) []byte {
	p := newCanonicalPayload(queryStampDomain)
	p.writeString("connection_id", connectionUID.String())
	p.writeInt("stamp_version", int64(version))
	p.writeInt("query_chain_len", count)
	p.writeBytes("head_mac", headMAC)

	return s.chainMAC(p.bytes())
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

// rowChainLockID is the same mapping for a capture's row chain, in its own lock
// class. A collision costs two unrelated captures a little serialization.
func rowChainLockID(queryUID uuid.UUID) int32 {
	return int32(binary.BigEndian.Uint32(queryUID[8:12]))
}
