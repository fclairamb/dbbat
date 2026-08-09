package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/uptrace/bun"
)

// LogAuditEvent creates a new audit log entry.
//
// When the store has a chain key, the row is appended to the tamper-evident
// audit chain: it is inserted inside a transaction holding a PostgreSQL
// advisory lock, carries the previous row's MAC, and carries its own MAC over
// (chain_seq, uid, event_type, user_id, performed_by, details, created_at,
// prev_mac). The lock plus the in-process mutex make the head a real
// serialization point — two concurrent appends must not read the same head, or
// one of them would be silently overwritten. Volume here is admin actions, so
// the contention that buys is negligible.
func (s *Store) LogAuditEvent(ctx context.Context, event *AuditEvent) error {
	logEntry := &AuditLog{
		UID:         newUIDv7(), // Generate UUIDv7 for time-ordered inserts
		EventType:   event.EventType,
		UserID:      event.UserID,
		PerformedBy: event.PerformedBy,
		Details:     event.Details,
		// Truncated to what timestamptz can store, because this exact value is
		// what the MAC covers and what verification must read back.
		CreatedAt: normalizeStoredTime(time.Now()),
	}

	if !s.ChainEnabled() {
		if _, err := s.db.NewInsert().Model(logEntry).Exec(ctx); err != nil {
			return fmt.Errorf("failed to log audit event: %w", err)
		}

		return nil
	}

	return s.appendAuditChain(ctx, logEntry)
}

// appendAuditChain inserts one sealed audit row.
func (s *Store) appendAuditChain(ctx context.Context, entry *AuditLog) error {
	s.auditChain.mu.Lock()
	defer s.auditChain.mu.Unlock()

	err := s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		// Released at commit or rollback, so a crashed process never strands it.
		if _, err := tx.ExecContext(ctx,
			"SELECT pg_advisory_xact_lock(?)", auditChainAdvisoryLockKey); err != nil {
			return fmt.Errorf("failed to take the audit chain lock: %w", err)
		}

		if !s.auditChain.loaded {
			seq, mac, err := readAuditChainHead(ctx, tx)
			if err != nil {
				return err
			}

			s.auditChain.seq = seq
			s.auditChain.mac = mac
			s.auditChain.loaded = true
		}

		seq := s.auditChain.seq + 1

		prevMAC := s.auditChain.mac
		if prevMAC == nil {
			prevMAC = s.auditGenesisMAC()
		}

		payload, err := auditChainPayload(
			seq, entry.UID, entry.EventType, entry.UserID, entry.PerformedBy,
			entry.Details, entry.CreatedAt, prevMAC,
		)
		if err != nil {
			return err
		}

		entry.ChainSeq = &seq
		entry.PrevMAC = prevMAC
		entry.MAC = s.chainMAC(payload)

		if _, err := tx.NewInsert().Model(entry).Exec(ctx); err != nil {
			return fmt.Errorf("failed to log audit event: %w", err)
		}

		return nil
	})
	if err != nil {
		// The transaction may have failed before or after the insert; either
		// way the cached head can no longer be trusted.
		s.auditChain.invalidate()

		return err
	}

	s.auditChain.seq = *entry.ChainSeq
	s.auditChain.mac = entry.MAC

	return nil
}

// readAuditChainHead returns the highest chain_seq and its MAC. It returns
// (0, nil) on a store where nothing has been chained yet — the anchor row is
// chain_seq 0 with a NULL mac, so it answers "nothing chained" too.
func readAuditChainHead(ctx context.Context, db bun.IDB) (int64, []byte, error) {
	var row struct {
		ChainSeq int64  `bun:"chain_seq"`
		MAC      []byte `bun:"mac"`
	}

	err := db.NewSelect().
		Model((*AuditLog)(nil)).
		Column("chain_seq", "mac").
		Where("chain_seq IS NOT NULL").
		Order("chain_seq DESC").
		Limit(1).
		Scan(ctx, &row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil, nil
		}

		return 0, nil, fmt.Errorf("failed to read the audit chain head: %w", err)
	}

	return row.ChainSeq, row.MAC, nil
}

// ListAuditEvents retrieves audit events with optional filters
func (s *Store) ListAuditEvents(ctx context.Context, filter AuditFilter) ([]AuditEvent, error) {
	var events []AuditLog
	q := s.db.NewSelect().Model(&events)

	if filter.EventType != nil {
		q = q.Where("event_type = ?", *filter.EventType)
	}

	if filter.UserID != nil {
		q = q.Where("user_id = ?", *filter.UserID)
	}

	if filter.PerformedBy != nil {
		q = q.Where("performed_by = ?", *filter.PerformedBy)
	}

	if filter.StartTime != nil {
		q = q.Where("created_at >= ?", *filter.StartTime)
	}

	if filter.EndTime != nil {
		q = q.Where("created_at <= ?", *filter.EndTime)
	}

	if filter.BeforeUID != nil {
		q = q.Where("uid < ?", *filter.BeforeUID)
	}

	q = q.Order("uid DESC")

	if filter.Limit > 0 {
		q = q.Limit(filter.Limit)
	}

	if filter.Offset > 0 {
		q = q.Offset(filter.Offset)
	}

	err := q.Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list audit events: %w", err)
	}

	if events == nil {
		events = []AuditLog{}
	}
	return events, nil
}
