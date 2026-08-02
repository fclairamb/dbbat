package api

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"github.com/fclairamb/dbbat/internal/approval"
	"github.com/fclairamb/dbbat/internal/events"
	"github.com/fclairamb/dbbat/internal/store"
)

// Stream transport tuning.
//
// A note on the transport choice, because it is the one debatable decision in
// this feature: the stream is ~99% server→client, and SSE would give us
// browser auto-reconnect, no upgrade handshake, no ping/pong to hand-roll and
// no load-balancer upgrade configuration. WebSocket was specified, so
// WebSocket is what ships — but the envelope below is deliberately
// transport-agnostic, so swapping is a change to this file and the frontend
// hook's internals, nothing else. See docs/approvals.md.
const (
	streamWriteWait      = 10 * time.Second
	streamPongWait       = 60 * time.Second
	streamPingPeriod     = 25 * time.Second
	streamMaxMessageSize = 4096
	streamReadBuffer     = 1024
	streamWriteBuffer    = 4096
)

// streamAuthSubprotocol prefixes the WebSocket subprotocol a browser uses to
// carry its bearer token.
//
// Browsers cannot set an Authorization header on a WebSocket handshake, and
// putting a session token in the query string would leak it into access logs,
// proxy logs and Referer headers. Sec-WebSocket-Protocol is the standard
// workaround: it is a real request header, it never appears in the URL, and the
// server must echo the selected value back.
const streamAuthSubprotocol = "dbbat.auth.bearer."

// streamAuthMiddleware promotes a bearer token carried in
// Sec-WebSocket-Protocol into a normal Authorization header, so the ordinary
// auth middleware handles the stream exactly like every other endpoint.
func (s *Server) streamAuthMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.GetHeader("Authorization") != "" {
			c.Next()

			return
		}

		for _, proto := range strings.Split(c.GetHeader("Sec-WebSocket-Protocol"), ",") {
			proto = strings.TrimSpace(proto)
			if token, ok := strings.CutPrefix(proto, streamAuthSubprotocol); ok && token != "" {
				c.Request.Header.Set("Authorization", "Bearer "+token)
				c.Set(contextKeyStreamSubprotocol, proto)

				break
			}
		}

		c.Next()
	}
}

// contextKeyStreamSubprotocol carries the subprotocol value that must be
// echoed back on a successful upgrade.
const contextKeyStreamSubprotocol = "stream_subprotocol"

// streamCommand is a client→server control message.
type streamCommand struct {
	Type  string `json:"type"`
	Topic string `json:"topic"`
}

// streamUpgrader upgrades /api/v1/stream. Same-origin is enforced by the
// browser for cookie-authenticated requests; the endpoint itself sits behind
// the normal auth middleware, so an unauthenticated upgrade never reaches
// here.
var streamUpgrader = websocket.Upgrader{
	ReadBufferSize:  streamReadBuffer,
	WriteBufferSize: streamWriteBuffer,
}

// handleStream upgrades to a WebSocket and pumps authorized topic events.
func (s *Server) handleStream(c *gin.Context) {
	user := getCurrentUser(c)
	if user == nil {
		writeError(c, http.StatusUnauthorized, ErrCodeUnauthorized, "authentication required")

		return
	}

	// Echo the auth subprotocol back; a browser closes the socket if the
	// server selects nothing when the client offered a protocol.
	var respHeader http.Header

	if proto, ok := c.Get(contextKeyStreamSubprotocol); ok {
		if value, ok := proto.(string); ok {
			respHeader = http.Header{"Sec-WebSocket-Protocol": []string{value}}
		}
	}

	conn, err := streamUpgrader.Upgrade(c.Writer, c.Request, respHeader)
	if err != nil {
		// Upgrade already wrote a response.
		s.logger.DebugContext(c.Request.Context(), "stream upgrade failed", slog.Any("error", err))

		return
	}

	defer func() { _ = conn.Close() }()

	ctx, cancel := context.WithCancel(context.WithoutCancel(c.Request.Context()))
	defer cancel()

	// Authorization is re-evaluated on every send, not just at subscribe:
	// sql_text routinely carries PII, and the stream must never become a
	// wider read path than GET /api/v1/queries.
	sub := s.broker.Subscribe(func(topic string) bool {
		return s.mayReadTopic(ctx, user, topic)
	}, events.DefaultBuffer)
	defer sub.Close()

	go s.streamReadLoop(ctx, cancel, conn, sub)

	s.streamWriteLoop(ctx, conn, sub)
}

// streamReadLoop handles subscribe/unsubscribe control messages and the pong
// keepalive. It cancels the session context when the client goes away.
func (s *Server) streamReadLoop(ctx context.Context, cancel context.CancelFunc, conn *websocket.Conn, sub *events.Subscriber) {
	defer cancel()

	conn.SetReadLimit(streamMaxMessageSize)
	_ = conn.SetReadDeadline(time.Now().Add(streamPongWait))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(streamPongWait))
	})

	for {
		var cmd streamCommand
		if err := conn.ReadJSON(&cmd); err != nil {
			return
		}

		switch cmd.Type {
		case "subscribe":
			ok := sub.Subscribe(cmd.Topic)

			ack := map[string]any{"type": "subscribed", "topic": cmd.Topic, "error": nil}
			if !ok {
				ack["error"] = "forbidden"
			}

			if err := writeStreamJSON(conn, ack); err != nil {
				return
			}

		case "unsubscribe":
			sub.Unsubscribe(cmd.Topic)

			if err := writeStreamJSON(conn, map[string]any{"type": "unsubscribed", "topic": cmd.Topic}); err != nil {
				return
			}

		default:
			if err := writeStreamJSON(conn, map[string]any{"type": "error", "error": "unknown command"}); err != nil {
				return
			}
		}

		if ctx.Err() != nil {
			return
		}
	}
}

// streamWriteLoop forwards events, drop notices and pings.
func (s *Server) streamWriteLoop(ctx context.Context, conn *websocket.Conn, sub *events.Subscriber) {
	ticker := time.NewTicker(streamPingPeriod)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case ev, ok := <-sub.Events():
			if !ok {
				return
			}

			if !s.flushStream(conn, sub, ev) {
				return
			}

		case <-ticker.C:
			_ = conn.SetWriteDeadline(time.Now().Add(streamWriteWait))

			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// flushStream writes one event plus anything that overflowed into the
// drop-exempt priority queue, and reports whether the socket is still usable.
func (s *Server) flushStream(conn *websocket.Conn, sub *events.Subscriber, ev events.Event) bool {
	// A gap is worse than a delay: tell the client it lost events so it
	// refetches from REST rather than assuming continuity.
	if dropped := sub.Dropped(); dropped > 0 {
		if err := writeStreamJSON(conn, map[string]any{"type": "lagged", "dropped": dropped}); err != nil {
			return false
		}
	}

	if err := writeStreamEvent(conn, ev); err != nil {
		return false
	}

	for _, extra := range sub.TakePriority() {
		if err := writeStreamEvent(conn, extra); err != nil {
			return false
		}
	}

	return true
}

func writeStreamEvent(conn *websocket.Conn, ev events.Event) error {
	return writeStreamJSON(conn, map[string]any{
		"type":  "event",
		"topic": ev.Topic,
		"seq":   ev.Seq,
		"event": ev.Type,
		"at":    ev.At,
		"data":  ev.Data,
	})
}

func writeStreamJSON(conn *websocket.Conn, payload any) error {
	_ = conn.SetWriteDeadline(time.Now().Add(streamWriteWait))

	return conn.WriteJSON(payload)
}

// mayReadTopic implements the per-topic authorization table. A topic
// subscription grants exactly what the equivalent REST endpoint would, no more.
func (s *Server) mayReadTopic(ctx context.Context, user *store.User, topic string) bool {
	if user == nil {
		return false
	}

	switch topic {
	case events.TopicConnections:
		// Connection open/close — admin only.
		return user.IsAdmin()

	case events.TopicApprovalsPending:
		// Admins, plus anybody who is an approver on at least one live grant.
		// Membership is what the approve endpoint checks too.
		return user.IsAdmin() || s.isApproverSomewhere(ctx, user)

	default:
		connUID, ok := events.ConnectionUIDFromTopic(topic)
		if !ok {
			return false
		}

		uid, err := uuid.Parse(connUID)
		if err != nil {
			return false
		}

		if user.IsAdmin() || user.IsViewer() {
			return true
		}

		// The connection's own owner may watch it — same rule as
		// GET /api/v1/connections/:uid.
		conn, err := s.store.GetConnectionByUID(ctx, uid)
		if err != nil {
			return false
		}

		return conn.UserID == user.UID
	}
}

// isApproverSomewhere reports whether the user belongs to any group named as
// an approver group on a live grant. Cheap enough for a subscribe check and
// for the per-send re-check: it is one membership lookup plus one indexed
// grant scan, and it only runs for the low-volume approvals topic.
func (s *Server) isApproverSomewhere(ctx context.Context, user *store.User) bool {
	groups, err := s.store.ListUserGroupUIDs(ctx, user.UID)
	if err != nil || len(groups) == 0 {
		return false
	}

	ok, err := s.store.HasApproverGroups(ctx, groups)
	if err != nil {
		return false
	}

	return ok
}

// StartEventListener subscribes to the cross-replica notification channels and
// republishes what arrives into the local broker, so an admin whose WebSocket
// landed on replica B still sees a query held on replica A.
//
// The payload carries only ids; the row is re-read here. That keeps SQL text
// out of the database's notification queue and means a late subscriber gets
// the current state rather than a stale snapshot.
func (s *Server) StartEventListener(ctx context.Context) {
	if s.store == nil || s.broker == nil {
		return
	}

	ctx, cancel := context.WithCancel(ctx)
	s.listenCancel = cancel

	go func() {
		err := s.store.ListenEvents(ctx, s.logger, func(n store.EventNotification) {
			s.republish(ctx, n)
		})
		if err != nil && ctx.Err() == nil {
			s.logger.WarnContext(ctx, "cross-replica event listener stopped", slog.Any("error", err))
		}
	}()
}

// republish turns a cross-replica notification into local events.
func (s *Server) republish(ctx context.Context, n store.EventNotification) {
	// Connection lifecycle notifications carry no query; republish them as-is.
	if n.QueryUID == uuid.Nil {
		if n.Type == events.EventConnection && n.ConnUID != uuid.Nil {
			s.broker.PublishLocal(events.TopicConnections, events.EventConnection, map[string]any{
				"connection_uid": n.ConnUID.String(),
			})
		}

		return
	}

	query, err := s.store.GetQueryWithOwner(ctx, n.QueryUID)
	if err != nil {
		return
	}

	data := map[string]any{
		"query_uid":         query.UID.String(),
		"connection_uid":    query.ConnectionID.String(),
		"sql_text":          query.SQLText,
		"executed_at":       query.ExecutedAt,
		"approval_required": query.ApprovalStatus != nil,
		"approval_status":   derefStr(query.ApprovalStatus),
		"approval_pattern":  derefStr(query.ApprovalPattern),
		"resolution_reason": derefStr(query.ResolutionReason),
	}

	if query.ResolvedAt != nil {
		data["resolved_at"] = *query.ResolvedAt
	}

	if query.ResolvedBy != nil {
		resolver := map[string]any{"uid": query.ResolvedBy.String()}
		if u, err := s.store.GetUserByUID(ctx, *query.ResolvedBy); err == nil {
			resolver["username"] = u.Username
			resolver["display_name"] = u.Username
		}

		data["resolved_by"] = resolver
	}

	eventType := n.Type
	if eventType == "" {
		eventType = events.EventQuery
	}

	// PublishLocal, never Publish: re-forwarding would loop between replicas.
	s.broker.PublishLocal(events.ConnectionQueriesTopic(query.ConnectionID.String()), eventType, data)
	s.broker.PublishLocal(events.TopicApprovalsPending, eventType, data)

	// A decision taken on another replica must also wake a session parked here.
	if eventType == events.EventApprovalResolved && query.ApprovalStatus != nil {
		decision := approval.Decision{
			QueryUID: query.UID,
			Status:   *query.ApprovalStatus,
			By:       query.ResolvedBy,
			Reason:   derefStr(query.ResolutionReason),
		}

		if query.ResolvedAt != nil {
			decision.At = *query.ResolvedAt
		}

		if r, ok := data["resolved_by"].(map[string]any); ok {
			if name, ok := r["username"].(string); ok {
				decision.ByName = name
			}
		}

		s.approvals.Resolve(decision)
	}
}
