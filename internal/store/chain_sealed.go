package store

import (
	"context"
	"crypto/hmac"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Sealed copies of the chain stamps.
//
// A stamp — connections.query_chain_mac, queries.row_chain_mac — is the only
// thing that catches records deleted from the *end* of a chain, and it lives on
// a plain row of an unchained table. Forging it needs the chain key; clearing
// it does not. Clearing the stamp *together with* everything it protects leaves
// a state no rule on the row alone can tell apart from "nothing was ever
// recorded": a session that logged nothing, a query that captured nothing.
//
// So each stamp also has a copy in `audit_log`, which is HMAC-chained and never
// reaped by retention:
//
//   - the query-chain stamp rides in the session's connection.closed entry, as
//     it always did (query_chain_mac / query_chain_len /
//     query_chain_stamp_version);
//   - the row-chain stamp rides in a query.rows_sealed entry written after the
//     capture's flush barrier.
//
// Verification compares each surviving row's stamp with its sealed copy: a
// stamp that is missing or differs is a break, and a row named by a sealed copy
// is walked even when it no longer carries a stamp or a single chained record.
// Editing the copy breaks the entry's own MAC, which is checked here; deleting
// it breaks the audit chain, which `dbbat audit verify` (no flag) catches. A
// copy whose row is gone altogether is not judged: retention deletes whole
// connections and whole queries, and that deletion is what the session entries'
// comparison, not this check, accounts for.

// AuditEventQueryRowsSealed records the final stamp of finished captures, so
// that clearing a query's row_chain_mac together with its rows is a break
// rather than a capture that never happened. One entry carries a batch of seals
// — see rowSealBatcher.
const AuditEventQueryRowsSealed = "query.rows_sealed"

const (
	// rowSealFlushDelay bounds how long a seal waits in memory before its
	// entry is written. A crash inside that window loses the sealed copy (the
	// stamp on the query row is already durable), which leaves that capture
	// exactly as protected as it was before sealed copies existed.
	rowSealFlushDelay = time.Second

	// rowSealBatchMax caps the seals one entry carries, and so the size of its
	// details document (~150 bytes a seal).
	rowSealBatchMax = 500
)

// rowSealRecord is one capture's sealed stamp, as written into a
// query.rows_sealed entry.
type rowSealRecord struct {
	ConnectionUID string `json:"connection_uid"`
	QueryUID      string `json:"query_uid"`
	RowChainLen   int64  `json:"row_chain_len"`
	RowChainMAC   string `json:"row_chain_mac"`
}

type rowSealsDetails struct {
	Seals []rowSealRecord `json:"seals"`
}

// rowSealBatcher collects seals and writes them as one chained entry per batch.
//
// Batched because every capture seals, and the audit chain is one store-wide
// sequence behind an advisory lock: a chained insert per captured statement
// would serialize every capture across every replica. One entry per second (or
// per rowSealBatchMax seals) keeps that cost flat.
type rowSealBatcher struct {
	mu      sync.Mutex
	pending []rowSealRecord
	timer   *time.Timer
	// inflight tracks a scheduled flush, so Close can wait for it rather than
	// race the database closing under it.
	inflight sync.WaitGroup
}

// queueRowSeal schedules the sealed copy of one capture's stamp.
func (s *Store) queueRowSeal(ctx context.Context, seal rowSealRecord) {
	b := &s.rowSeals

	// Detached: the copy is written after the capture that sealed it, possibly
	// by a timer that outlives the request. writeConnectionAudit bounds it.
	ctx = context.WithoutCancel(ctx)

	b.mu.Lock()
	b.pending = append(b.pending, seal)

	if len(b.pending) >= rowSealBatchMax {
		b.mu.Unlock()
		s.FlushRowSeals(ctx)

		return
	}

	if b.timer == nil {
		b.inflight.Add(1)
		b.timer = time.AfterFunc(rowSealFlushDelay, func() {
			defer b.inflight.Done()

			s.FlushRowSeals(ctx)
		})
	}

	b.mu.Unlock()
}

// FlushRowSeals writes every pending sealed row stamp now. Best effort like
// the session entries: a failure is logged, never returned, because the stamp
// on the query row is already durable and a capture must not fail over its
// evidence copy.
func (s *Store) FlushRowSeals(ctx context.Context) {
	b := &s.rowSeals

	b.mu.Lock()
	pending := b.pending
	b.pending = nil

	if b.timer != nil && b.timer.Stop() {
		// Stopped before it fired: its callback will never call Done.
		b.inflight.Done()
	}

	b.timer = nil
	b.mu.Unlock()

	for start := 0; start < len(pending); start += rowSealBatchMax {
		end := min(start+rowSealBatchMax, len(pending))

		encoded, err := json.Marshal(rowSealsDetails{Seals: pending[start:end]})
		if err != nil {
			slog.ErrorContext(ctx, "failed to encode the sealed row stamps", slog.Any("error", err))

			continue
		}

		s.writeConnectionAudit(ctx, &AuditEvent{EventType: AuditEventQueryRowsSealed, Details: encoded})
	}
}

// closeRowSeals flushes what is pending and waits for a scheduled flush, so the
// last second of seals is not lost on a clean shutdown.
func (s *Store) closeRowSeals() {
	s.FlushRowSeals(context.Background())
	s.rowSeals.inflight.Wait()
}

// sealedStamp is a stamp as its sealed copy records it.
type sealedStamp struct {
	entryUID uuid.UUID
	mac      []byte
	length   int64
	version  int16
}

// verifySealEntry checks a sealed copy's own MAC, so a copy edited to agree
// with a cleared stamp is a break rather than an excuse. It returns why the
// entry does not verify, or "" when it does. Its position in the
// chain is the audit walk's business: a deleted entry is caught there.
func (s *Store) verifySealEntry(row *AuditLog) string {
	if row.ChainSeq == nil {
		return "the entry is not chained"
	}

	if brk := s.verifyAuditRow(row, *row.ChainSeq, row.PrevMAC); brk != nil {
		return brk.Reason
	}

	return ""
}

// sealedQueryStamp returns the query-chain stamp the latest connection.closed
// entry of a session sealed, or nil when no entry carries one: a session that
// logged nothing, one closed before its entry could be written, or one whose
// row is not closed yet.
func (s *Store) sealedQueryStamp(ctx context.Context, connectionUID uuid.UUID) (*sealedStamp, string, error) {
	var row AuditLog

	err := s.db.NewSelect().
		Model(&row).
		Where("al.event_type = ?", AuditEventConnectionClosed).
		Where("al.details->>'connection_uid' = ?", connectionUID.String()).
		Where("al.details->>'query_chain_mac' IS NOT NULL").
		Order("al.chain_seq DESC").
		Limit(1).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, "", nil
		}

		return nil, "", fmt.Errorf("failed to read the session's sealed stamp: %w", err)
	}

	if reason := s.verifySealEntry(&row); reason != "" {
		return nil, fmt.Sprintf("its connection.closed entry %s does not verify: %s", row.UID, reason), nil
	}

	var details connectionAuditDetails
	if err := json.Unmarshal(row.Details, &details); err != nil {
		return nil, fmt.Sprintf("its connection.closed entry %s cannot be read: %v", row.UID, err), nil
	}

	mac, err := hex.DecodeString(details.QueryChainMAC)
	if err != nil {
		return nil, fmt.Sprintf("its connection.closed entry %s carries an unreadable stamp: %v", row.UID, err), nil
	}

	stamp := &sealedStamp{entryUID: row.UID, mac: mac}

	if details.QueryChainLen != nil {
		stamp.length = *details.QueryChainLen
	}

	if details.QueryChainStampVersion != nil {
		stamp.version = *details.QueryChainStampVersion
	}

	return stamp, "", nil
}

// checkSealedQueryStamp compares a closed session's stamp with the one its
// connection.closed entry sealed.
//
// It runs after every other stamp rule, and only when they found nothing, so a
// break they can name keeps its wording. It is the one rule that still holds
// when the stamp and the statements it protects were removed together: without
// it, a cleared stamp over a deleted history reads as a session that logged
// nothing.
func (s *Store) checkSealedQueryStamp(ctx context.Context, result *QueryChainResult) error {
	var conn Connection

	err := s.db.NewSelect().
		Model(&conn).
		Column("uid", "disconnected_at", "query_chain_mac", "query_chain_len", "query_chain_stamp_version").
		Where("uid = ?", result.ConnectionUID).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// The whole session is gone: see checkStampedHead.
			return nil
		}

		return fmt.Errorf("failed to read the connection chain head: %w", err)
	}

	if conn.DisconnectedAt == nil {
		return nil
	}

	sealed, unreadable, err := s.sealedQueryStamp(ctx, result.ConnectionUID)
	if err != nil {
		return err
	}

	if unreadable != "" {
		result.Break = stampBreak(result, conn, fmt.Sprintf(
			"the session's sealed stamp cannot be trusted: %s", unreadable))

		return nil
	}

	if sealed == nil {
		return nil
	}

	if conn.QueryChainMAC == nil {
		result.Break = stampBreak(result, conn, fmt.Sprintf(
			"the session's connection.closed entry %s sealed a chain of %d statements, "+
				"but the connection carries no head stamp: the stamp was cleared after the session closed",
			sealed.entryUID, sealed.length))

		return nil
	}

	if conn.QueryChainLen != sealed.length || conn.QueryChainStampVersion != sealed.version ||
		!hmac.Equal(conn.QueryChainMAC, sealed.mac) {
		result.Break = stampBreak(result, conn, fmt.Sprintf(
			"the connection's head stamp (%d statements, %s) differs from the one its connection.closed "+
				"entry %s sealed (%d statements, %s): the stamp was rewritten after the session closed",
			conn.QueryChainLen, hex.EncodeToString(conn.QueryChainMAC),
			sealed.entryUID, sealed.length, hex.EncodeToString(sealed.mac)))

		return nil
	}

	return nil
}

// sealedRowStamp returns the row-chain stamp the latest query.rows_sealed
// entry naming a query recorded, or nil when none does.
func (s *Store) sealedRowStamp(ctx context.Context, queryUID uuid.UUID) (*sealedStamp, string, error) {
	var row AuditLog

	err := s.db.NewSelect().
		Model(&row).
		Where("al.event_type = ?", AuditEventQueryRowsSealed).
		Where("al.details @> jsonb_build_object('seals', jsonb_build_array(jsonb_build_object('query_uid', ?::text)))",
			queryUID.String()).
		Order("al.chain_seq DESC").
		Limit(1).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, "", nil
		}

		return nil, "", fmt.Errorf("failed to read the capture's sealed stamp: %w", err)
	}

	if reason := s.verifySealEntry(&row); reason != "" {
		return nil, fmt.Sprintf("its query.rows_sealed entry %s does not verify: %s", row.UID, reason), nil
	}

	var details rowSealsDetails
	if err := json.Unmarshal(row.Details, &details); err != nil {
		return nil, fmt.Sprintf("its query.rows_sealed entry %s cannot be read: %v", row.UID, err), nil
	}

	for _, seal := range details.Seals {
		if seal.QueryUID != queryUID.String() {
			continue
		}

		mac, err := hex.DecodeString(seal.RowChainMAC)
		if err != nil {
			return nil, fmt.Sprintf("its query.rows_sealed entry %s carries an unreadable stamp: %v", row.UID, err), nil
		}

		return &sealedStamp{entryUID: row.UID, mac: mac, length: seal.RowChainLen}, "", nil
	}

	return nil, "", nil
}

// checkSealedRowStamp compares a query's row stamp with its sealed copy. Like
// checkSealedQueryStamp, it runs last.
func (s *Store) checkSealedRowStamp(ctx context.Context, result *RowChainResult) error {
	var query Query

	err := s.db.NewSelect().
		Model(&query).
		Column("uid", "row_chain_mac", "row_chain_len").
		Where("uid = ?", result.QueryUID).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// The query is gone, its rows with it: retention, not this check.
			return nil
		}

		return fmt.Errorf("failed to read the capture chain head: %w", err)
	}

	sealed, unreadable, err := s.sealedRowStamp(ctx, result.QueryUID)
	if err != nil {
		return err
	}

	queryUID := result.QueryUID

	rowBreak := func(reason string) {
		result.Break = &ChainBreak{UID: queryUID, ChainSeq: query.RowChainLen, QueryUID: &queryUID, Reason: reason}
	}

	switch {
	case unreadable != "":
		rowBreak(fmt.Sprintf("the capture's sealed stamp cannot be trusted: %s", unreadable))
	case sealed == nil:
		return nil
	case query.RowChainMAC == nil:
		rowBreak(fmt.Sprintf(
			"the capture's query.rows_sealed entry %s sealed %d rows, but the query carries no head stamp: "+
				"the stamp was cleared after the capture finished",
			sealed.entryUID, sealed.length))
	case query.RowChainLen != sealed.length || !hmac.Equal(query.RowChainMAC, sealed.mac):
		rowBreak(fmt.Sprintf(
			"the query's row stamp (%d rows, %s) differs from the one its query.rows_sealed entry %s "+
				"sealed (%d rows, %s): the stamp was rewritten after the capture finished",
			query.RowChainLen, hex.EncodeToString(query.RowChainMAC),
			sealed.entryUID, sealed.length, hex.EncodeToString(sealed.mac)))
	default:
		return nil
	}

	return nil
}
