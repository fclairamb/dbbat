package store

import (
	"context"
	"crypto/hmac"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/uptrace/bun"
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
	// QueryUID is set for result-row-chain breaks. ChainSeq then carries the
	// offending row's row_number.
	QueryUID *uuid.UUID
	// Reason is a human-readable description of what did not add up.
	Reason string
}

func (b ChainBreak) String() string {
	if b.QueryUID != nil {
		return fmt.Sprintf("query %s, row_number %d, row %s: %s",
			b.QueryUID, b.ChainSeq, b.UID, b.Reason)
	}

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
	// LegacyStamp is true when the session's head is sealed by the pre-0.24
	// stamp — a verbatim copy of the head MAC, which anyone who can write to
	// the store can also write. The chain still verified; what did not happen is
	// a *keyed* confirmation that nothing was removed from its end.
	LegacyStamp bool
	// FirstSeq is the oldest surviving statement's chain_seq, or 0 when nothing
	// survives. It is 1 unless retention reaped the chain's prefix, and it is
	// what tells a stamp whose sealed position was *reaped* from one that names
	// a position which never existed. See checkStampedHead.
	FirstSeq int64
	HeadSeq  int64
	HeadMAC  []byte
	Break    *ChainBreak
}

// QueryChainsResult aggregates a sweep over many connections.
type QueryChainsResult struct {
	// Connections is how many connections carried a chain and were walked.
	Connections int64
	// Verified is how many statements were checked across them.
	Verified int64
	// Truncated is how many of those chains were missing a prefix.
	Truncated int64
	// LegacyStamps is how many of those connections still carry the pre-0.24
	// forgeable head stamp. It is not a failure — it is the part of the store
	// whose *tail* nothing keyed vouches for. Sessions closed by this build are
	// sealed, so the number drains as old sessions age out of retention; a
	// number that stops falling, or rises again, is worth looking at.
	LegacyStamps int64
	// Break is the first failure found, or nil.
	Break *ChainBreak
}

// OK reports whether every chain walked verified.
func (r QueryChainsResult) OK() bool { return r.Break == nil }

// RowChainResult is the outcome of verifying one capture's result-row chain.
type RowChainResult struct {
	QueryUID uuid.UUID
	// Verified is how many captured rows were checked.
	Verified int64
	// HeadRowNumber and HeadMAC are where the surviving rows end.
	HeadRowNumber int64
	HeadMAC       []byte
	Break         *ChainBreak
}

// RowChainsResult aggregates a sweep over many captures.
type RowChainsResult struct {
	// Captures is how many queries carried a row chain and were walked.
	Captures int64
	// Verified is how many captured rows were checked across them.
	Verified int64
	// Unchained is how many query_rows predate the row chain migration.
	// Nothing sealed them, so they are reported rather than counted as
	// verified — the same treatment pre-anchor audit rows get.
	Unchained int64
	Break     *ChainBreak
}

// OK reports whether every capture walked verified.
func (r RowChainsResult) OK() bool { return r.Break == nil }

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

		if one.LegacyStamp {
			result.LegacyStamps++
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

			if result.FirstSeq == 0 {
				result.FirstSeq = *row.ChainSeq
			}

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

// checkStampedHead compares the head a connection recorded against what its
// surviving statements compute, and records a break on the result when they
// disagree. This is the only check that catches a deletion at the *end* of a
// chain.
//
// The stamp comes in two formats and the column says which (see
// queryChainStampMAC). The keyed one is checked first, and it is checked *with
// the version the row claims* — so a sealed row cannot be relabelled as legacy
// to get the weaker rule applied to it. Only when that fails, and only for a
// row that says it is legacy, is the pre-0.24 raw-head comparison tried; a row
// that passes that way is recorded as a legacy stamp rather than as a
// verification, because an unkeyed stamp proves nothing to anyone who can write
// to this database.
//
// # Exact for a closed session, a prefix for an open one
//
// A closed session's stamp is final: it must name exactly the head the
// survivors end on. An open session's is not. RefreshOpenChainStamps re-seals
// live sessions on the reclaim timer, so at any moment a busy session's stamp
// names an *older, smaller* chain_seq than its current head, and the MAC over
// the statement at that older position. Judging that by the exact rule would
// report a break on every session that ran a statement since the last sweep —
// which is worse than not refreshing at all. So while disconnected_at IS NULL
// the stamp is checked as a prefix: it must seal the statement at the position
// it names, and that position must be one the surviving chain reached.
//
// What that still catches on an open session: a stamp naming a position *newer*
// than anything that survives (retention only ever removes the oldest, so
// nothing legitimate makes the stamp outrun the survivors), and a stamp whose
// sealed statement has been rewritten — correcting it needs the chain key. What
// it does not catch, by construction, is a deletion of statements newer than
// the last sweep. That window is bounded by the sweep interval, and shrinking
// it is exactly what the refresh buys; it is not closed.
func (s *Store) checkStampedHead(ctx context.Context, result *QueryChainResult) error {
	var conn Connection

	err := s.db.NewSelect().
		Model(&conn).
		Column("uid", "disconnected_at", "query_chain_mac", "query_chain_len", "query_chain_stamp_version").
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

	// Never stamped: the session logged nothing, or it is open and no sweep has
	// reached it yet. Nothing to compare against.
	if conn.QueryChainMAC == nil {
		return nil
	}

	connUID := result.ConnectionUID
	version := conn.QueryChainStampVersion

	if version > queryChainStampKeyed {
		// Written by a build that knows a format this one does not. Saying so
		// is honest; calling it tampering would not be.
		result.Break = stampBreak(result, conn, fmt.Sprintf(
			"the connection's chain head is stamped in format version %d, "+
				"which this build cannot verify: it was written by a newer dbbat", version))

		return nil
	}

	sealedMAC := result.HeadMAC

	if version != queryChainStampLegacy && conn.QueryChainLen != result.HeadSeq {
		// Checked before the MAC purely for the message: the length is sealed
		// by the stamp either way, since the expected stamp below is computed
		// from what the surviving statements say, never from the column.
		lagging, brk, err := s.stampedPrefixMAC(ctx, result, conn)
		if err != nil {
			return err
		}

		if brk != nil {
			result.Break = brk

			return nil
		}

		if lagging == nil {
			// The sealed position is below everything that survives: retention
			// reaped it. Unverifiable, not evidence of anything — and already
			// visible as result.TruncatedPrefix whenever anything survives.
			return nil
		}

		sealedMAC = lagging
	}

	if hmac.Equal(conn.QueryChainMAC,
		s.queryChainStampMAC(connUID, version, conn.QueryChainLen, sealedMAC)) {
		return nil
	}

	// The pre-0.24 stamp: a verbatim copy of the head MAC. Accepted so that a
	// store upgraded from 0.23.x does not report a break on every session it
	// closed before the upgrade — but counted, never called verified, because
	// anyone who can write to `queries` can also write this.
	if version == queryChainStampLegacy && hmac.Equal(conn.QueryChainMAC, result.HeadMAC) {
		result.LegacyStamp = true

		return nil
	}

	result.Break = stampBreak(result, conn, fmt.Sprintf(
		"the connection's stamped head %s does not seal the statement at chain_seq %d (%s): "+
			"statements were removed from the end of the session, or the stamp was rewritten",
		hex.EncodeToString(conn.QueryChainMAC), conn.QueryChainLen, hex.EncodeToString(sealedMAC)))

	return nil
}

// stampedPrefixMAC resolves what a stamp that does not name the current head
// should be checked against.
//
// It returns either the MAC of the statement the stamp sealed (check it against
// that), nil with no break (the sealed position was reaped by retention —
// unverifiable), or a break.
//
// The MAC is read back raw, which is safe because the walk that precedes this
// verified every surviving statement from FirstSeq to HeadSeq against its own
// content and against its predecessor: a row inside that range whose MAC did not
// add up would have stopped the walk before it got here.
func (s *Store) stampedPrefixMAC(
	ctx context.Context, result *QueryChainResult, conn Connection,
) ([]byte, *ChainBreak, error) {
	// A closed session's stamp is final and must name the head exactly. So must
	// an open session's when the stamp claims a position the survivors never
	// reached: retention removes the *oldest* statements, so nothing legitimate
	// leaves a stamp ahead of the head.
	if conn.DisconnectedAt != nil || conn.QueryChainLen > result.HeadSeq {
		// Nothing survives at all. For an open session that is what retention
		// does to a session that has been idle longer than the retention window
		// (CleanupOldQueryRows never reaps the open connection row itself, only
		// its statements), so it proves nothing either way. A closed session
		// keeps the break: its statements and its row are reaped together.
		if result.Verified == 0 && conn.DisconnectedAt == nil {
			return nil, nil, nil
		}

		return nil, stampBreak(result, conn, fmt.Sprintf(
			"the connection recorded %d statements but the surviving statements end at %d: "+
				"statements were removed from the end of the session",
			conn.QueryChainLen, result.HeadSeq)), nil
	}

	// Below the oldest survivor: DBB_QUERY_STORAGE_RETENTION reaped the
	// statement the stamp sealed. Nothing left to check it against.
	if result.Verified == 0 || conn.QueryChainLen < result.FirstSeq {
		return nil, nil, nil
	}

	var row struct {
		MAC []byte `bun:"mac"`
	}

	err := s.db.NewSelect().
		Model((*Query)(nil)).
		Column("mac").
		Where("connection_id = ?", result.ConnectionUID).
		Where("chain_seq = ?", conn.QueryChainLen).
		Scan(ctx, &row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// Inside the surviving range yet absent: the walk would have caught
			// the gap, so this is unreachable in practice. Reported rather than
			// silently accepted.
			return nil, stampBreak(result, conn, fmt.Sprintf(
				"the connection's stamp seals the statement at chain_seq %d, "+
					"which is missing from the surviving statements %d..%d",
				conn.QueryChainLen, result.FirstSeq, result.HeadSeq)), nil
		}

		return nil, nil, fmt.Errorf("failed to read the stamped statement: %w", err)
	}

	return row.MAC, nil, nil
}

// stampBreak builds the break a stamped-head mismatch reports. The offending
// "row" is the connection itself: the statements that are left all verified.
func stampBreak(result *QueryChainResult, conn Connection, reason string) *ChainBreak {
	connUID := result.ConnectionUID

	return &ChainBreak{
		UID:           connUID,
		ChainSeq:      conn.QueryChainLen,
		ConnectionUID: &connUID,
		Reason:        reason,
	}
}

// VerifyRowChains walks the result-row chain of every capture that has one, or
// of one connection's captures when connectionUID is set.
//
// The row chains differ from the query chains in one way that matters to a
// walk: retention never deletes an individual captured row. It deletes whole
// queries (and whole connections), and query_rows.query_id cascades — so unlike
// a session's statements, a capture has no legitimate reason to be missing its
// prefix. A first row whose prev_mac is not the capture's genesis MAC is a
// break, not housekeeping.
func (s *Store) VerifyRowChains(ctx context.Context, connectionUID *uuid.UUID) (RowChainsResult, error) {
	var result RowChainsResult

	if !s.ChainEnabled() {
		return result, ErrChainKeyUnavailable
	}

	unchained, err := s.countUnchainedRows(ctx, connectionUID)
	if err != nil {
		return result, err
	}

	result.Unchained = unchained

	uids, err := s.capturedQueryUIDs(ctx, connectionUID)
	if err != nil {
		return result, err
	}

	for _, uid := range uids {
		one, err := s.VerifyRowChain(ctx, uid)
		if err != nil {
			return result, err
		}

		result.Captures++
		result.Verified += one.Verified

		if one.Break != nil {
			result.Break = one.Break

			return result, nil
		}
	}

	return result, nil
}

// countUnchainedRows counts captured rows written before the row chain
// migration. They carry no MAC and none can be created after the fact.
func (s *Store) countUnchainedRows(ctx context.Context, connectionUID *uuid.UUID) (int64, error) {
	q := s.db.NewSelect().
		Model((*QueryRowModel)(nil)).
		Where("qr.mac IS NULL")

	if connectionUID != nil {
		q = q.Where("qr.query_id IN (SELECT uid FROM queries WHERE connection_id = ?)", *connectionUID)
	}

	count, err := q.Count(ctx)
	if err != nil {
		return 0, fmt.Errorf("failed to count unchained captured rows: %w", err)
	}

	return int64(count), nil
}

// capturedQueryUIDs lists the captures worth walking, oldest first.
//
// A query is in scope when it still has chained rows *or* when it carries a
// stamped head. The second half is what makes deleting a capture wholesale
// detectable: a query whose rows were all removed has nothing left to enumerate
// from query_rows, but its stamp still claims a head.
func (s *Store) capturedQueryUIDs(ctx context.Context, connectionUID *uuid.UUID) ([]uuid.UUID, error) {
	var uids []uuid.UUID

	q := s.db.NewSelect().
		Model((*Query)(nil)).
		Column("uid").
		WhereGroup(" AND ", func(q *bun.SelectQuery) *bun.SelectQuery {
			return q.
				Where("q.row_chain_mac IS NOT NULL").
				WhereOr("EXISTS (SELECT 1 FROM query_rows r WHERE r.query_id = q.uid AND r.mac IS NOT NULL)")
		}).
		Order("q.uid ASC")

	if connectionUID != nil {
		q = q.Where("q.connection_id = ?", *connectionUID)
	}

	if err := q.Scan(ctx, &uids); err != nil {
		return nil, fmt.Errorf("failed to list captures: %w", err)
	}

	return uids, nil
}

// VerifyRowChain walks one capture's result-row chain.
//
// The position is row_number, and it is deliberately not required to be dense:
// a row the batched writer had to drop, or one that failed to encode, leaves a
// gap that is not evidence of anything. Density is not what makes a deletion
// detectable — the prev_mac linkage is. Removing a row from the middle leaves
// its successor pointing at a MAC no surviving row has.
func (s *Store) VerifyRowChain(ctx context.Context, queryUID uuid.UUID) (RowChainResult, error) {
	result := RowChainResult{QueryUID: queryUID}

	if !s.ChainEnabled() {
		return result, ErrChainKeyUnavailable
	}

	prevMAC := s.rowGenesisMAC(queryUID)

	var (
		started    bool
		lastNumber int
		lastUID    uuid.UUID
	)

	for {
		var rows []QueryRowModel

		q := s.db.NewSelect().
			Model(&rows).
			Where("qr.query_id = ?", queryUID).
			Where("qr.mac IS NOT NULL").
			Order("qr.row_number ASC", "qr.uid ASC").
			Limit(chainPageSize)

		if started {
			q = q.Where("(qr.row_number, qr.uid) > (?, ?)", lastNumber, lastUID)
		}

		if err := q.Scan(ctx); err != nil {
			return result, fmt.Errorf("failed to read captured row chain: %w", err)
		}

		if len(rows) == 0 {
			break
		}

		for i := range rows {
			row := &rows[i]

			if brk := s.verifyCapturedRow(row, queryUID, started, lastNumber, prevMAC); brk != nil {
				result.Break = brk

				return result, nil
			}

			result.Verified++
			result.HeadRowNumber = int64(row.RowNumber)
			result.HeadMAC = row.MAC
			prevMAC = row.MAC
			lastNumber = row.RowNumber
			lastUID = row.UID
			started = true
		}
	}

	if err := s.checkStampedRowHead(ctx, &result); err != nil {
		return result, err
	}

	return result, nil
}

func (s *Store) verifyCapturedRow(
	row *QueryRowModel, queryUID uuid.UUID, started bool, lastNumber int, prevMAC []byte,
) *ChainBreak {
	position := int64(row.RowNumber)

	if started && row.RowNumber <= lastNumber {
		return &ChainBreak{
			UID: row.UID, ChainSeq: position, QueryUID: &queryUID,
			Reason: fmt.Sprintf(
				"row_number %d does not follow %d: captured rows were renumbered or reordered",
				row.RowNumber, lastNumber),
		}
	}

	if !hmac.Equal(row.PrevMAC, prevMAC) {
		reason := "prev_mac does not match the previous captured row: a row was removed, reordered or rewritten"
		if !started {
			reason = "prev_mac is not this capture's genesis: rows were removed from the start of the capture"
		}

		return &ChainBreak{UID: row.UID, ChainSeq: position, QueryUID: &queryUID, Reason: reason}
	}

	payload, err := rowChainPayload(
		row.QueryID, row.UID, row.RowNumber, row.RowData, row.RowSizeBytes, row.PrevMAC,
	)
	if err != nil {
		return &ChainBreak{
			UID: row.UID, ChainSeq: position, QueryUID: &queryUID,
			Reason: fmt.Sprintf("captured row could not be serialized for verification: %v", err),
		}
	}

	if !hmac.Equal(row.MAC, s.chainMAC(payload)) {
		return &ChainBreak{
			UID: row.UID, ChainSeq: position, QueryUID: &queryUID,
			Reason: "mac does not match the captured row's content: the row was modified",
		}
	}

	return nil
}

// checkStampedRowHead compares the head a finished capture recorded against the
// head its surviving rows compute. This is the only check that catches rows
// deleted from the *end* of a capture — or a capture deleted outright while its
// query survived.
//
// The stamp is a keyed MAC over (query, length, head MAC), not a copy of the
// head MAC, so recomputing it after a deletion needs the chain key. A stamp
// that stored the head verbatim would defend against nothing: the head is
// readable from query_rows, so the attacker deleting the last rows could simply
// copy the new last row's MAC over it. Both halves are therefore checked
// against what the surviving rows compute — the recorded length as well as the
// MAC — so an edit to either one is a break.
func (s *Store) checkStampedRowHead(ctx context.Context, result *RowChainResult) error {
	var query Query

	err := s.db.NewSelect().
		Model(&query).
		Column("uid", "row_chain_mac", "row_chain_len").
		Where("uid = ?", result.QueryUID).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// The query is gone, so retention took its rows with it. Nothing
			// left to compare against.
			return nil
		}

		return fmt.Errorf("failed to read the capture chain head: %w", err)
	}

	// Never stamped: the capture is still running, it captured nothing, or the
	// process died before the flush barrier.
	if query.RowChainMAC == nil {
		return nil
	}

	queryUID := result.QueryUID

	if query.RowChainLen != result.Verified {
		result.Break = &ChainBreak{
			UID:      queryUID,
			ChainSeq: query.RowChainLen,
			QueryUID: &queryUID,
			Reason: fmt.Sprintf(
				"the query recorded %d captured rows but %d survive: "+
					"rows were removed from the end of the capture",
				query.RowChainLen, result.Verified),
		}

		return nil
	}

	expected := s.rowChainStampMAC(queryUID, result.Verified, result.HeadMAC)
	if hmac.Equal(query.RowChainMAC, expected) {
		return nil
	}

	result.Break = &ChainBreak{
		UID:      queryUID,
		ChainSeq: query.RowChainLen,
		QueryUID: &queryUID,
		Reason: fmt.Sprintf(
			"the query's stamped head %s does not seal the %d surviving rows ending at %d/%s: "+
				"rows were removed from the end of the capture, or the stamp was rewritten",
			hex.EncodeToString(query.RowChainMAC), result.Verified,
			result.HeadRowNumber, hex.EncodeToString(result.HeadMAC)),
	}

	return nil
}
