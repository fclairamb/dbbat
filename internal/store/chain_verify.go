package store

import (
	"context"
	"crypto/hmac"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// Chain verification.
//
// A walk goes oldest to newest and stops at the first thing that does not add
// up, because after a break nothing downstream means anything. Four things are
// hard breaks:
//
//   - a row whose own MAC does not match its content (the row was edited);
//   - a row whose prev_mac is not the previous row's MAC (a row was removed
//     from the middle, or two rows were swapped);
//   - a gap in chain_seq (a row was removed);
//   - a connection whose stamped chain head is not what its surviving queries
//     compute (rows were removed from the end).
//
// One thing is not a break, and is reported separately: a chain whose *prefix*
// is missing. On the queries side that is what DBB_QUERY_STORAGE_RETENTION does
// to a long-lived session, so treating it as tampering would cry wolf on every
// deployment that sets a retention. Everything after the truncation is still
// verified; what is lost is only the proof that the removed prefix existed. On
// the audit side retention never runs, so a truncated prefix there *is* a
// break.

// ChainBreak names the first row that failed to verify.
type ChainBreak struct {
	// UID of the offending row, or of the connection when the break is a
	// stamped-head mismatch.
	UID uuid.UUID
	// ChainSeq is its position, or 0 when the break is about the chain as a
	// whole rather than one row.
	ChainSeq int64
	// ConnectionUID is set for query-chain breaks.
	ConnectionUID *uuid.UUID
	// Reason is a human-readable description of what did not add up.
	Reason string
}

func (b ChainBreak) String() string {
	if b.ConnectionUID != nil {
		return fmt.Sprintf("connection %s, chain_seq %d, row %s: %s",
			b.ConnectionUID, b.ChainSeq, b.UID, b.Reason)
	}

	return fmt.Sprintf("chain_seq %d, row %s: %s", b.ChainSeq, b.UID, b.Reason)
}

// AuditChainResult is the outcome of verifying the store-wide audit chain.
type AuditChainResult struct {
	// Verified is how many chained rows were checked.
	Verified int64
	// Unchained is how many audit rows predate the anchor. Those rows cannot
	// be verified — nothing sealed them — and are reported so the number is
	// visible rather than implied.
	Unchained int64
	// HeadSeq and HeadMAC are the head the chain ends on. An operator notes
	// the head down outside the database: comparing it against the next run is
	// what detects a chain that was truncated and re-sealed wholesale by
	// someone who did have the key.
	HeadSeq int64
	HeadMAC []byte
	// Break is the first failure, or nil when the chain is intact.
	Break *ChainBreak
}

// OK reports whether the chain verified.
func (r AuditChainResult) OK() bool { return r.Break == nil }

// HeadMACHex renders the head MAC for an operator to record.
func (r AuditChainResult) HeadMACHex() string { return hex.EncodeToString(r.HeadMAC) }

// QueryChainResult is the outcome of verifying one connection's query chain.
type QueryChainResult struct {
	ConnectionUID uuid.UUID
	// Verified is how many statements were checked.
	Verified int64
	// TruncatedPrefix is true when the chain does not start at 1 — the
	// expected shape for a connection whose oldest statements were reaped by
	// DBB_QUERY_STORAGE_RETENTION.
	TruncatedPrefix bool
	HeadSeq         int64
	HeadMAC         []byte
	Break           *ChainBreak
}

// QueryChainsResult aggregates a sweep over many connections.
type QueryChainsResult struct {
	// Connections is how many connections carried a chain and were walked.
	Connections int64
	// Verified is how many statements were checked across them.
	Verified int64
	// Truncated is how many of those chains were missing a prefix.
	Truncated int64
	// Break is the first failure found, or nil.
	Break *ChainBreak
}

// OK reports whether every chain walked verified.
func (r QueryChainsResult) OK() bool { return r.Break == nil }

// chainPageSize bounds how many rows one verification query pulls. The audit
// log is small; a busy connection's history is not.
const chainPageSize = 1000

// VerifyAuditChain walks the store-wide audit chain oldest to newest.
func (s *Store) VerifyAuditChain(ctx context.Context) (AuditChainResult, error) {
	var result AuditChainResult

	if !s.ChainEnabled() {
		return result, ErrChainKeyUnavailable
	}

	unchained, err := s.db.NewSelect().
		Model((*AuditLog)(nil)).
		Where("chain_seq IS NULL").
		Count(ctx)
	if err != nil {
		return result, fmt.Errorf("failed to count unchained audit rows: %w", err)
	}

	result.Unchained = int64(unchained)

	expectedSeq := int64(1)
	prevMAC := s.auditGenesisMAC()

	for {
		var rows []AuditLog

		err := s.db.NewSelect().
			Model(&rows).
			Where("chain_seq >= ?", expectedSeq).
			Order("chain_seq ASC").
			Limit(chainPageSize).
			Scan(ctx)
		if err != nil {
			return result, fmt.Errorf("failed to read audit chain rows: %w", err)
		}

		if len(rows) == 0 {
			break
		}

		for i := range rows {
			row := &rows[i]

			if brk := s.verifyAuditRow(row, expectedSeq, prevMAC); brk != nil {
				result.Break = brk

				return result, nil
			}

			result.Verified++
			result.HeadSeq = *row.ChainSeq
			result.HeadMAC = row.MAC
			prevMAC = row.MAC
			expectedSeq = *row.ChainSeq + 1
		}
	}

	return result, nil
}

// verifyAuditRow checks one link. expectedSeq is the position the walk is at,
// which is how a deleted row is caught before its MAC even matters.
func (s *Store) verifyAuditRow(row *AuditLog, expectedSeq int64, prevMAC []byte) *ChainBreak {
	if row.ChainSeq == nil {
		return &ChainBreak{UID: row.UID, ChainSeq: expectedSeq, Reason: "row has no chain position"}
	}

	seq := *row.ChainSeq

	// chain_seq 0 is the anchor: a marker, not a link. It is skipped by the
	// query above (which starts at 1) and only reachable if someone renumbered.
	if seq != expectedSeq {
		return &ChainBreak{
			UID:      row.UID,
			ChainSeq: seq,
			Reason: fmt.Sprintf(
				"expected chain_seq %d, found %d: %d entries missing or reordered",
				expectedSeq, seq, seq-expectedSeq),
		}
	}

	if !hmac.Equal(row.PrevMAC, prevMAC) {
		return &ChainBreak{
			UID: row.UID, ChainSeq: seq,
			Reason: "prev_mac does not match the previous entry: an entry was removed, reordered or rewritten",
		}
	}

	payload, err := auditChainPayload(
		seq, row.UID, row.EventType, row.UserID, row.PerformedBy,
		row.Details, row.CreatedAt, row.PrevMAC,
	)
	if err != nil {
		return &ChainBreak{
			UID: row.UID, ChainSeq: seq,
			Reason: fmt.Sprintf("entry could not be serialized for verification: %v", err),
		}
	}

	if !hmac.Equal(row.MAC, s.chainMAC(payload)) {
		return &ChainBreak{
			UID: row.UID, ChainSeq: seq,
			Reason: "mac does not match the entry's content: the entry was modified",
		}
	}

	return nil
}

// VerifyQueryChains walks the query chain of every connection that has one, or
// of one connection when connectionUID is set.
func (s *Store) VerifyQueryChains(ctx context.Context, connectionUID *uuid.UUID) (QueryChainsResult, error) {
	var result QueryChainsResult

	if !s.ChainEnabled() {
		return result, ErrChainKeyUnavailable
	}

	uids, err := s.chainedConnectionUIDs(ctx, connectionUID)
	if err != nil {
		return result, err
	}

	for _, uid := range uids {
		one, err := s.VerifyQueryChain(ctx, uid)
		if err != nil {
			return result, err
		}

		result.Connections++
		result.Verified += one.Verified

		if one.TruncatedPrefix {
			result.Truncated++
		}

		if one.Break != nil {
			result.Break = one.Break

			return result, nil
		}
	}

	return result, nil
}

// chainedConnectionUIDs lists the connections worth walking, oldest first. A
// connection with no chained query is skipped: there is nothing to verify, and
// a session that logged nothing is not evidence of anything.
func (s *Store) chainedConnectionUIDs(ctx context.Context, only *uuid.UUID) ([]uuid.UUID, error) {
	if only != nil {
		return []uuid.UUID{*only}, nil
	}

	var uids []uuid.UUID

	err := s.db.NewSelect().
		Model((*Query)(nil)).
		ColumnExpr("DISTINCT q.connection_id").
		Where("q.chain_seq IS NOT NULL").
		Order("q.connection_id ASC").
		Scan(ctx, &uids)
	if err != nil {
		return nil, fmt.Errorf("failed to list chained connections: %w", err)
	}

	return uids, nil
}

// VerifyQueryChain walks one connection's query chain.
func (s *Store) VerifyQueryChain(ctx context.Context, connectionUID uuid.UUID) (QueryChainResult, error) {
	result := QueryChainResult{ConnectionUID: connectionUID}

	if !s.ChainEnabled() {
		return result, ErrChainKeyUnavailable
	}

	expectedSeq := int64(1)
	prevMAC := s.queryGenesisMAC(connectionUID)
	first := true

	for {
		var rows []Query

		err := s.db.NewSelect().
			Model(&rows).
			Where("connection_id = ?", connectionUID).
			Where("chain_seq >= ?", expectedSeq).
			Order("chain_seq ASC").
			Limit(chainPageSize).
			Scan(ctx)
		if err != nil {
			return result, fmt.Errorf("failed to read query chain rows: %w", err)
		}

		if len(rows) == 0 {
			break
		}

		for i := range rows {
			row := &rows[i]

			// A chain that does not start at 1 lost its prefix. Retention does
			// exactly that to a long-lived session, so the walk resumes from
			// what survived instead of declaring the whole history forged.
			if first && row.ChainSeq != nil && *row.ChainSeq > 1 {
				result.TruncatedPrefix = true
				expectedSeq = *row.ChainSeq
				prevMAC = row.PrevMAC
			}

			first = false

			if brk := s.verifyQueryRow(row, connectionUID, expectedSeq, prevMAC); brk != nil {
				result.Break = brk

				return result, nil
			}

			result.Verified++
			result.HeadSeq = *row.ChainSeq
			result.HeadMAC = row.MAC
			prevMAC = row.MAC
			expectedSeq = *row.ChainSeq + 1
		}
	}

	if err := s.checkStampedHead(ctx, &result); err != nil {
		return result, err
	}

	return result, nil
}

func (s *Store) verifyQueryRow(row *Query, connectionUID uuid.UUID, expectedSeq int64, prevMAC []byte) *ChainBreak {
	if row.ChainSeq == nil {
		return &ChainBreak{
			UID: row.UID, ChainSeq: expectedSeq, ConnectionUID: &connectionUID,
			Reason: "statement has no chain position",
		}
	}

	seq := *row.ChainSeq

	if seq != expectedSeq {
		return &ChainBreak{
			UID: row.UID, ChainSeq: seq, ConnectionUID: &connectionUID,
			Reason: fmt.Sprintf(
				"expected chain_seq %d, found %d: %d statements missing or reordered",
				expectedSeq, seq, seq-expectedSeq),
		}
	}

	if !hmac.Equal(row.PrevMAC, prevMAC) {
		return &ChainBreak{
			UID: row.UID, ChainSeq: seq, ConnectionUID: &connectionUID,
			Reason: "prev_mac does not match the previous statement: a statement was removed, reordered or rewritten",
		}
	}

	payload, err := queryChainPayload(
		seq, row.UID, row.ConnectionID, row.SQLText,
		queryParametersJSON(row.Parameters), row.ExecutedAt, row.PrevMAC,
	)
	if err != nil {
		return &ChainBreak{
			UID: row.UID, ChainSeq: seq, ConnectionUID: &connectionUID,
			Reason: fmt.Sprintf("statement could not be serialized for verification: %v", err),
		}
	}

	if !hmac.Equal(row.MAC, s.chainMAC(payload)) {
		return &ChainBreak{
			UID: row.UID, ChainSeq: seq, ConnectionUID: &connectionUID,
			Reason: "mac does not match the statement's content: the statement was modified",
		}
	}

	return nil
}

// checkStampedHead compares the head a closed connection recorded against the
// head its surviving statements compute, and records a break on the result when
// they disagree. This is the only check that catches a deletion at the *end* of
// a chain.
func (s *Store) checkStampedHead(ctx context.Context, result *QueryChainResult) error {
	var conn Connection

	err := s.db.NewSelect().
		Model(&conn).
		Column("uid", "query_chain_mac", "query_chain_len").
		Where("uid = ?", result.ConnectionUID).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// A connection row that is gone is not a break: retention deletes
			// whole connections, queries and all, and the foreign key cascades,
			// so there is nothing left to compare against.
			return nil
		}

		return fmt.Errorf("failed to read the connection chain head: %w", err)
	}

	// Never stamped: the session is still open, or it died without a clean
	// close. Nothing to compare against.
	if conn.QueryChainMAC == nil {
		return nil
	}

	if hmac.Equal(conn.QueryChainMAC, result.HeadMAC) {
		return nil
	}

	connUID := result.ConnectionUID

	result.Break = &ChainBreak{
		UID:           connUID,
		ChainSeq:      conn.QueryChainLen,
		ConnectionUID: &connUID,
		Reason: fmt.Sprintf(
			"the connection recorded %d statements ending in %s, but the surviving statements end at %d/%s: "+
				"statements were removed from the end of the session",
			conn.QueryChainLen, hex.EncodeToString(conn.QueryChainMAC),
			result.HeadSeq, hex.EncodeToString(result.HeadMAC)),
	}

	return nil
}
