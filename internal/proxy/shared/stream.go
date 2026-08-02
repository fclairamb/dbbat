package shared

import (
	"context"
	"log/slog"

	"github.com/google/uuid"

	"github.com/fclairamb/dbbat/internal/events"
	"github.com/fclairamb/dbbat/internal/store"
)

// StreamPublisher publishes a session's observable activity — every query on
// the connection, plus connection open/close — to the live event stream.
//
// It is separate from ApprovalGate on purpose: streaming is unconditional and
// harmless, approval holds are opt-in and block a database connection. They
// share nothing but the connection identity.
//
// Every method is nil-safe and non-blocking. A broken stream must never break
// a database connection, which is why nothing here returns an error.
type StreamPublisher struct {
	broker        *events.Broker
	store         ApprovalStore
	logger        *slog.Logger
	connectionUID uuid.UUID
	userUID       uuid.UUID
	username      string
	databaseName  string
}

// NewStreamPublisher builds a publisher for one session. A nil broker yields a
// publisher whose methods are no-ops.
func NewStreamPublisher(
	deps ApprovalDeps,
	connectionUID uuid.UUID,
	user *store.User,
	databaseName string,
) *StreamPublisher {
	p := &StreamPublisher{
		broker:        deps.Broker,
		store:         deps.Store,
		logger:        deps.Logger,
		connectionUID: connectionUID,
		databaseName:  databaseName,
	}

	if user != nil {
		p.userUID = user.UID
		p.username = user.Username
	}

	return p
}

// Query announces one executed statement on connection/<uid>/queries.
//
// There is deliberately no global all-queries topic: a busy proxy issues
// thousands of statements a second, each carrying full SQL text, and
// firehosing that would be both a throughput problem and a data-exposure one.
// Watching is per-connection and opt-in.
func (p *StreamPublisher) Query(queryUID uuid.UUID, q *store.Query) {
	if p == nil || p.broker == nil || q == nil {
		return
	}

	data := map[string]any{
		"query_uid":         queryUID.String(),
		"connection_uid":    p.connectionUID.String(),
		"user_uid":          p.userUID.String(),
		"username":          p.username,
		"database_name":     p.databaseName,
		"sql_text":          q.SQLText,
		"executed_at":       q.ExecutedAt,
		"approval_required": q.ApprovalStatus != nil,
		"approval_status":   derefString(q.ApprovalStatus),
	}

	if q.DurationMs != nil {
		data["duration_ms"] = *q.DurationMs
	}

	if q.RowsAffected != nil {
		data["rows_affected"] = *q.RowsAffected
	}

	if q.Error != nil {
		data["error"] = *q.Error
	}

	p.broker.Publish(events.ConnectionQueriesTopic(p.connectionUID.String()), events.EventQuery, data)
}

// Connection announces a connection lifecycle change on the admin-only
// connections topic.
func (p *StreamPublisher) Connection(ctx context.Context, state string) {
	if p == nil || p.broker == nil || p.connectionUID == uuid.Nil {
		return
	}

	p.broker.Publish(events.TopicConnections, events.EventConnection, map[string]any{
		"connection_uid": p.connectionUID.String(),
		"user_uid":       p.userUID.String(),
		"username":       p.username,
		"database_name":  p.databaseName,
		"state":          state,
	})

	if p.store == nil {
		return
	}

	err := p.store.NotifyEvent(ctx, store.NotifyChannelQueries, store.EventNotification{
		Topic:   events.TopicConnections,
		Type:    events.EventConnection,
		ConnUID: p.connectionUID,
	})
	if err != nil && p.logger != nil {
		p.logger.DebugContext(ctx, "failed to fan connection event to other replicas", slog.Any("error", err))
	}
}

// Connection lifecycle states published on the connections topic.
const (
	// ConnectionOpened marks a session that finished authenticating.
	ConnectionOpened = "opened"
	// ConnectionClosed marks a session that ended.
	ConnectionClosed = "closed"
)

func derefString(s *string) string {
	if s == nil {
		return ""
	}

	return *s
}
