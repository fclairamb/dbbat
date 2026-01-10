package store

import (
	"context"
	"fmt"
	"time"
)

// LogAuditEvent creates a new audit log entry
func (s *Store) LogAuditEvent(ctx context.Context, event *AuditEvent) error {
	logEntry := &AuditLog{
		UID:         newUIDv7(), // Generate UUIDv7 for time-ordered inserts
		EventType:   event.EventType,
		UserID:      event.UserID,
		PerformedBy: event.PerformedBy,
		Details:     event.Details,
		CreatedAt:   time.Now(),
	}

	_, err := s.db.NewInsert().
		Model(logEntry).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("failed to log audit event: %w", err)
	}
	return nil
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

	q = q.Order("created_at DESC")

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
