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
	return s.LogAuditEvents(ctx, event)
}

// LogAuditEvents appends several audit entries as one chained batch: one
// transaction, one advisory lock, one bulk INSERT, and the entries linked to
// each other in the order they were given.
//
// It exists for the session reconcile, which closes an arbitrary number of
// crash-orphaned connections in one pass and owes each of them a
// `connection.closed` entry. Appending those one at a time would be one
// transaction and one round trip to the store-wide chain lock *per connection*,
// which is exactly the shape the row-chain batching exists to avoid.
//
// The batch is all-or-nothing. A chain is a sequence, so a partially applied
// batch would either leave a gap or force the survivors to be re-linked; the
// transaction is what makes "some of these landed" not a state.
func (s *Store) LogAuditEvents(ctx context.Context, events ...*AuditEvent) error {
	if len(events) == 0 {
		return nil
	}

	entries := make([]*AuditLog, 0, len(events))

	for _, event := range events {
		entries = append(entries, &AuditLog{
			UID:         newUIDv7(), // Generate UUIDv7 for time-ordered inserts
			EventType:   event.EventType,
			UserID:      event.UserID,
			PerformedBy: event.PerformedBy,
			Details:     event.Details,
			// Truncated to what timestamptz can store, because this exact value
			// is what the MAC covers and what verification must read back.
			CreatedAt: normalizeStoredTime(time.Now()),
		})
	}

	if !s.ChainEnabled() {
		if _, err := s.db.NewInsert().Model(&entries).Exec(ctx); err != nil {
			return fmt.Errorf("failed to log audit event: %w", err)
		}

		return nil
	}

	return s.appendAuditChain(ctx, entries)
}

// appendAuditChain inserts one sealed batch of audit rows, retrying with a
// re-read head when the cached one turns out to be stale.
//
// A stale head is the normal multi-replica case: this process appended seq 5,
// a peer appended seq 6, and this process still believes the head is 5. The
// insert then collides with the unique index on chain_seq. The first attempt
// therefore trusts the cache and every later attempt re-reads under the lock,
// which is the whole reason the cache is safe to keep.
//
// The retry is also why the append owns its transaction rather than joining a
// caller's: a colliding INSERT aborts the enclosing transaction, so recovering
// from it needs a fresh one.
func (s *Store) appendAuditChain(ctx context.Context, entries []*AuditLog) error {
	s.auditChain.mu.Lock()
	defer s.auditChain.mu.Unlock()

	var err error

	for attempt := range chainAppendAttempts {
		if attempt > 0 {
			s.auditChain.invalidate()

			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
		}

		if err = s.appendAuditChainOnce(ctx, entries); err == nil {
			return nil
		}
	}

	return err
}

func (s *Store) appendAuditChainOnce(ctx context.Context, entries []*AuditLog) error {
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

		seq := s.auditChain.seq

		prevMAC := s.auditChain.mac
		if prevMAC == nil {
			prevMAC = s.auditGenesisMAC()
		}

		for _, entry := range entries {
			seq++

			payload, err := auditChainPayload(
				seq, entry.UID, entry.EventType, entry.UserID, entry.PerformedBy,
				entry.Details, entry.CreatedAt, prevMAC,
			)
			if err != nil {
				return err
			}

			// Copied per entry: ChainSeq is a pointer, so every row needs its
			// own storage rather than a view of the loop's cursor.
			entrySeq := seq

			entry.ChainSeq = &entrySeq
			entry.PrevMAC = prevMAC
			entry.MAC = s.chainMAC(payload)

			prevMAC = entry.MAC
		}

		if _, err := tx.NewInsert().Model(&entries).Exec(ctx); err != nil {
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

	head := entries[len(entries)-1]

	s.auditChain.seq = *head.ChainSeq
	s.auditChain.mac = head.MAC

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

// ListLatestEventPerUser returns, for each user that has one, the newest audit
// entry of eventType — one row per user, whatever the volume of the audit log.
//
// This exists because the alternative the UI used first — fetch the newest N
// entries of a type and keep the first one seen per user — degrades silently:
// a user whose last sync fell outside that window becomes indistinguishable
// from a user who never had one. Raising N moves the cliff instead of removing
// it. DISTINCT ON does the picking in the database, so the answer is exact.
//
// Ordering is `user_id, uid DESC`: `uid` is a UUIDv7 assigned at insert, so
// descending is newest-first, and DISTINCT ON keeps the first row of each
// group. The join to `users` is what supplies the username and, incidentally,
// drops entries whose user has since been deleted — including soft-deleted
// ones, filtered here explicitly because bun only applies soft-delete rules to
// the model's own table.
func (s *Store) ListLatestEventPerUser(ctx context.Context, eventType string) ([]UserRoleSync, error) {
	syncs := []UserRoleSync{}

	err := s.db.NewSelect().
		Model(&syncs).
		DistinctOn("al.user_id").
		ColumnExpr("al.uid, al.event_type, al.user_id, al.details, al.created_at").
		ColumnExpr("u.username").
		Join("JOIN users AS u ON u.uid = al.user_id AND u.deleted_at IS NULL").
		Where("al.event_type = ?", eventType).
		Order("al.user_id", "al.uid DESC").
		Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list the latest %s per user: %w", eventType, err)
	}

	return syncs, nil
}

// ListAuditEvents retrieves audit events with optional filters.
//
// The session events (connection.opened / connection.closed) are left out of an
// unfiltered listing — see SessionAuditEventTypes for why, and ask for one by
// name to get it back.
func (s *Store) ListAuditEvents(ctx context.Context, filter AuditFilter) ([]AuditEvent, error) {
	var events []AuditLog
	q := s.db.NewSelect().Model(&events)

	switch {
	case filter.EventType != nil:
		q = q.Where("event_type = ?", *filter.EventType)
	case !filter.IncludeSessionEvents:
		q = q.Where("event_type NOT IN (?)", bun.List(SessionAuditEventTypes))
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
