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
