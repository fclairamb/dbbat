package store

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/uptrace/bun/driver/pgdriver"
)

// PostgreSQL LISTEN/NOTIFY channels used to fan events between dbbat replicas.
//
// dbbat runs several pods: the proxy session holding a parked query lives on
// replica A while the admin's stream socket lives on replica B, so the
// in-process broker is not enough on its own. The store connection we already
// have is the cheapest correct bus available — no new infrastructure, no
// at-most-once message broker to operate.
//
// The payload is deliberately just the topic plus the query uid: NOTIFY
// payloads are capped at 8000 bytes and, more importantly, SQL text has no
// business traveling through the database's notification queue where it would
// be visible to anything with LISTEN privileges. Receivers re-read the row.
const (
	// NotifyChannelQueries carries per-connection query events.
	NotifyChannelQueries = "dbbat_query_events"
	// NotifyChannelApprovals carries approval pending/resolved events.
	NotifyChannelApprovals = "dbbat_approval_events"
)

// EventNotification is the wire payload of a cross-replica notification.
type EventNotification struct {
	Topic    string    `json:"topic"`
	Type     string    `json:"type"`
	QueryUID uuid.UUID `json:"query_uid,omitempty"`
	ConnUID  uuid.UUID `json:"conn_uid,omitempty"`
}

// NotifyEvent publishes a cross-replica notification. Best-effort by design:
// every caller sits on (or near) the proxy hot path, and a failed NOTIFY must
// degrade the stream on other replicas, never the database session.
func (s *Store) NotifyEvent(ctx context.Context, channel string, payload EventNotification) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to encode notification: %w", err)
	}

	if _, err := s.db.ExecContext(ctx, "SELECT pg_notify(?, ?)", channel, string(body)); err != nil {
		return fmt.Errorf("failed to notify %s: %w", channel, err)
	}

	return nil
}

// ListenEvents subscribes to the cross-replica channels and invokes handler for
// every notification until ctx is canceled. It runs until the context ends;
// the underlying pgdriver listener reconnects on its own, so a database blip
// costs a gap in the live stream (which clients repair by refetching from
// REST) and nothing more.
func (s *Store) ListenEvents(ctx context.Context, logger *slog.Logger, handler func(EventNotification)) error {
	ln := pgdriver.NewListener(s.db)
	defer func() { _ = ln.Close() }()

	if err := ln.Listen(ctx, NotifyChannelQueries, NotifyChannelApprovals); err != nil {
		return fmt.Errorf("failed to LISTEN: %w", err)
	}

	for notif := range ln.Channel() {
		var payload EventNotification
		if err := json.Unmarshal([]byte(notif.Payload), &payload); err != nil {
			if logger != nil {
				logger.WarnContext(ctx, "malformed event notification", slog.Any("error", err))
			}

			continue
		}

		handler(payload)
	}

	return ctx.Err()
}
