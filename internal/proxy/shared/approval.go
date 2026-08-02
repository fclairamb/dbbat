package shared

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/fclairamb/dbbat/internal/approval"
	"github.com/fclairamb/dbbat/internal/events"
	"github.com/fclairamb/dbbat/internal/store"
)

// Approval hold errors. They travel back through each protocol's existing
// blocked-query path (the same one ErrDDLBlocked uses), so a denied statement
// reaches the client as a protocol-native error rather than a dropped socket.
var (
	// ErrApprovalAbandoned means the hold ended without a human decision —
	// the client disconnected, the query was cancelled, the grant expired, or
	// the server drained. Nothing was ever forwarded upstream. It is rendered
	// distinctly from "denied" everywhere: to whoever finally looks at it, a
	// query nobody is waiting for anymore is a different thing from a query a
	// human rejected.
	ErrApprovalAbandoned = errors.New("approval hold abandoned: client is gone")
	// ErrApprovalUnavailable means the hold could not even be registered
	// (the pending row failed to persist). It fails **closed**: a statement
	// matching an approval pattern is never forwarded just because the
	// bookkeeping broke.
	ErrApprovalUnavailable = errors.New("approval required but the approval system is unavailable")
)

// ApprovalDeniedError is returned when a human explicitly denied the
// statement. It carries the approver's reason so each protocol can surface it
// verbatim to the client.
type ApprovalDeniedError struct {
	Reason string
	By     string
}

func (e *ApprovalDeniedError) Error() string {
	switch {
	case e.Reason != "" && e.By != "":
		return fmt.Sprintf("query denied by %s: %s", e.By, e.Reason)
	case e.Reason != "":
		return "query denied by approver: " + e.Reason
	case e.By != "":
		return "query denied by " + e.By
	default:
		return "query denied by approver"
	}
}

// Is makes errors.Is(err, ErrApprovalDenied) work for the sentinel below.
func (e *ApprovalDeniedError) Is(target error) bool { return target == ErrApprovalDenied }

// ErrApprovalDenied is the sentinel every ApprovalDeniedError matches, so
// callers can branch with errors.Is without unwrapping.
var ErrApprovalDenied = errors.New("query denied by approver")

// ApprovalStore is the slice of the store the gate needs. An interface rather
// than *store.Store so protocol tests can drive a hold end-to-end without a
// database.
type ApprovalStore interface {
	CreatePendingQuery(ctx context.Context, query *store.Query, pattern string) (*store.Query, error)
	ResolveQueryApproval(ctx context.Context, uid uuid.UUID, status string, resolvedBy *uuid.UUID, reason string) error
	NotifyEvent(ctx context.Context, channel string, payload store.EventNotification) error
}

// ApprovalEscalator fires the delayed Slack notification for a hold and
// updates it in place once the hold resolves by any route. Optional: a nil
// escalator simply means no Slack.
type ApprovalEscalator interface {
	// Schedule arms the escalation timer for a hold.
	Schedule(ctx context.Context, hold ApprovalHoldInfo)
	// Resolved cancels a not-yet-fired escalation, or updates the posted
	// message in place if it already fired. Because there is no approval
	// timeout, a posted message stays actionable indefinitely — a stale
	// Approve button that silently no-ops is worse than no button.
	Resolved(ctx context.Context, queryUID uuid.UUID, status, byName, reason string)
}

// ApprovalHoldInfo describes a parked statement to the escalator.
type ApprovalHoldInfo struct {
	QueryUID      uuid.UUID
	ConnectionUID uuid.UUID
	UserUID       uuid.UUID
	Username      string
	DatabaseName  string
	SQL           string
	Pattern       string
	StartedAt     time.Time
}

// ApprovalDeps are the collaborators every gate in the process shares.
type ApprovalDeps struct {
	Enabled   bool
	Store     ApprovalStore
	Registry  *approval.Registry
	Broker    *events.Broker
	Escalator ApprovalEscalator
	Logger    *slog.Logger
	// PollInterval is how often quotas/expiry/revocation are re-evaluated
	// while a statement is parked. With no approval timeout, the LimitGuard
	// is the one remaining server-side bound, so it must keep running.
	PollInterval time.Duration
}

// ApprovalGate decides whether a statement needs a human, and parks the
// session while one is found.
//
// It is constructed once per session: the grant's RE2 patterns are compiled
// here and reused for every statement, so the per-statement cost of the common
// (no-pattern) case is a nil check.
type ApprovalGate struct {
	deps     ApprovalDeps
	patterns []*regexp.Regexp
	sources  []string

	connectionUID uuid.UUID
	userUID       uuid.UUID
	username      string
	databaseName  string
}

// NewApprovalGate compiles the grant's approval patterns. A gate is returned
// even when nothing is configured — callers check Active() (or just call
// Match, which reports false) rather than nil-checking.
//
// Patterns that fail to compile are skipped with a loud log rather than
// failing the session: they are validated at definition-save time, so a bad
// one here means data predating validation, and refusing every connection over
// it would be a worse failure than gating one statement less.
func NewApprovalGate(deps ApprovalDeps, grant *store.Grant, connectionUID uuid.UUID, user *store.User, databaseName string) *ApprovalGate {
	g := &ApprovalGate{
		deps:          deps,
		connectionUID: connectionUID,
		databaseName:  databaseName,
	}

	if user != nil {
		g.userUID = user.UID
		g.username = user.Username
	}

	if !deps.Enabled || grant == nil {
		return g
	}

	for _, src := range grant.ApprovalPatterns {
		re, err := regexp.Compile(src)
		if err != nil {
			if deps.Logger != nil {
				deps.Logger.Warn("skipping invalid approval pattern",
					slog.String("pattern", src), slog.Any("error", err))
			}

			continue
		}

		g.patterns = append(g.patterns, re)
		g.sources = append(g.sources, src)
	}

	return g
}

// Active reports whether this gate can hold anything at all.
func (g *ApprovalGate) Active() bool {
	return g != nil && g.deps.Enabled && len(g.patterns) > 0
}

// Match reports the first pattern the statement matches. Matching runs on the
// same normalized SQL text the static validators use, so an operator writing a
// pattern does not have to reason about a second normalization.
func (g *ApprovalGate) Match(sql string) (string, bool) {
	if !g.Active() {
		return "", false
	}

	normalized := NormalizeSQL(sql)

	for i, re := range g.patterns {
		if re.MatchString(normalized) {
			return g.sources[i], true
		}
	}

	return "", false
}

// NormalizeSQL is the canonical normalization applied before pattern matching.
// Kept deliberately minimal — the static validators only trim before their own
// prefix/regex checks, and silently rewriting a statement before matching it
// would make patterns behave differently from what an operator can read in
// /queries.
func NormalizeSQL(sql string) string {
	return strings.TrimSpace(sql)
}

// HoldRequest is one parked statement.
type HoldRequest struct {
	// SQL is the statement text, as it will be persisted and shown.
	SQL string
	// Params are the bind parameters, known at Execute time (not at Parse
	// time — which is exactly why the PostgreSQL gate hooks Execute).
	Params *store.QueryParameters
	// Pattern is the pattern that matched, recorded on the row.
	Pattern string
	// StartedAt is when the statement arrived.
	StartedAt time.Time
	// ClientGone fires when the parked client's socket dies. This is the
	// sole liveness bound on a hold: without it, "until disconnect" means
	// forever.
	ClientGone <-chan struct{}
	// Guard keeps quotas, expiry and revocation running while parked.
	Guard *LimitGuard
}

// Hold persists the statement as pending, announces it, and blocks until a
// human resolves it or the hold dies. It returns the query uid it created (so
// the caller can complete that row instead of inserting a second one) and nil
// only on an explicit approval.
//
// Every other exit — deny, disconnect, quota/expiry/revocation, shutdown —
// returns an error, and no path returns nil without an approval decision whose
// query uid equals the one this call created. That last clause is the TOCTOU
// guard: a client able to influence timing must not be able to have somebody
// else's approval released against its statement.
//
//nolint:funlen,gocognit // the select loop is the feature; splitting it hides the exit conditions
func (g *ApprovalGate) Hold(ctx context.Context, req HoldRequest) (uuid.UUID, error) {
	if g.deps.Store == nil || g.deps.Registry == nil {
		return uuid.Nil, ErrApprovalUnavailable
	}

	startedAt := req.StartedAt
	if startedAt.IsZero() {
		startedAt = time.Now()
	}

	// Persist first: the held statement must be visible in /queries and
	// addressable by UID *while* it hangs, not only once it resolves.
	pending, err := g.deps.Store.CreatePendingQuery(ctx, &store.Query{
		ConnectionID: g.connectionUID,
		SQLText:      req.SQL,
		Parameters:   req.Params,
		ExecutedAt:   startedAt,
	}, req.Pattern)
	if err != nil {
		g.log(ctx, "failed to persist pending approval", slog.Any("error", err))

		return uuid.Nil, ErrApprovalUnavailable
	}

	decisions, release := g.deps.Registry.Register(pending.UID)
	defer release()

	g.publishPending(ctx, pending, req)

	if g.deps.Escalator != nil {
		g.deps.Escalator.Schedule(ctx, ApprovalHoldInfo{
			QueryUID:      pending.UID,
			ConnectionUID: g.connectionUID,
			UserUID:       g.userUID,
			Username:      g.username,
			DatabaseName:  g.databaseName,
			SQL:           req.SQL,
			Pattern:       req.Pattern,
			StartedAt:     startedAt,
		})
	}

	interval := g.deps.PollInterval
	if interval <= 0 {
		interval = DefaultLimitPollInterval
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case d := <-decisions:
			// TOCTOU: only a decision naming *this* statement counts.
			if d.QueryUID != pending.UID {
				g.log(ctx, "ignoring approval decision for a different query",
					slog.String("expected", pending.UID.String()),
					slog.String("got", d.QueryUID.String()))

				continue
			}

			switch d.Status {
			case store.ApprovalApproved:
				g.announceResolved(ctx, pending.UID, store.ApprovalApproved, d)

				return pending.UID, nil
			case store.ApprovalDenied:
				g.announceResolved(ctx, pending.UID, store.ApprovalDenied, d)

				return pending.UID, &ApprovalDeniedError{Reason: d.Reason, By: d.ByName}
			default:
				// abandoned, or anything unexpected: fail closed.
				g.announceResolved(ctx, pending.UID, store.ApprovalAbandoned, d)

				return pending.UID, ErrApprovalAbandoned
			}

		case <-req.ClientGone:
			g.abandon(ctx, pending.UID, "client disconnected while awaiting approval")

			return pending.UID, ErrApprovalAbandoned

		case <-ctx.Done():
			g.abandon(ctx, pending.UID, "session ended while awaiting approval")

			return pending.UID, ErrApprovalAbandoned

		case <-ticker.C:
			// Quotas, expiry and revocation keep running while parked — with
			// no approval timeout this is the only remaining server-side
			// bound, so the parked state must not bypass it.
			if err := req.Guard.Check(); err != nil {
				g.abandon(ctx, pending.UID, err.Error())

				return pending.UID, err
			}
		}
	}
}

// abandon records and announces a hold that ended without a human decision.
func (g *ApprovalGate) abandon(ctx context.Context, queryUID uuid.UUID, reason string) {
	// The session's own context may already be canceled; use a detached one
	// so the row never stays 'pending' forever after a disconnect.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()

	if err := g.deps.Store.ResolveQueryApproval(writeCtx, queryUID, store.ApprovalAbandoned, nil, reason); err != nil &&
		!errors.Is(err, store.ErrQueryNotPending) {
		g.log(ctx, "failed to mark approval abandoned", slog.Any("error", err))
	}

	g.announceResolved(writeCtx, queryUID, store.ApprovalAbandoned, approval.Decision{
		QueryUID: queryUID,
		Status:   store.ApprovalAbandoned,
		Reason:   reason,
		At:       time.Now(),
	})
}

// publishPending announces a newly parked statement on both topics and to the
// other replicas.
func (g *ApprovalGate) publishPending(ctx context.Context, pending *store.Query, req HoldRequest) {
	data := map[string]any{
		"query_uid":         pending.UID.String(),
		"connection_uid":    g.connectionUID.String(),
		"user_uid":          g.userUID.String(),
		"username":          g.username,
		"database_name":     g.databaseName,
		"sql_text":          req.SQL,
		"executed_at":       pending.ExecutedAt,
		"approval_required": true,
		"approval_status":   store.ApprovalPending,
		"approval_pattern":  req.Pattern,
	}

	g.deps.Broker.Publish(events.ConnectionQueriesTopic(g.connectionUID.String()), events.EventApprovalPending, data)
	g.deps.Broker.Publish(events.TopicApprovalsPending, events.EventApprovalPending, data)

	g.notifyReplicas(ctx, store.NotifyChannelApprovals, events.EventApprovalPending, pending.UID)
}

// announceResolved publishes the resolution on both topics, carrying who
// resolved it — every watcher should see who unblocked a query, not merely
// that it unblocked.
func (g *ApprovalGate) announceResolved(ctx context.Context, queryUID uuid.UUID, status string, d approval.Decision) {
	data := map[string]any{
		"query_uid":         queryUID.String(),
		"connection_uid":    g.connectionUID.String(),
		"approval_required": true,
		"approval_status":   status,
		"resolution_reason": d.Reason,
		"resolved_at":       d.At,
	}

	if d.By != nil || d.ByName != "" {
		resolver := map[string]any{"display_name": d.ByName, "username": d.ByName}
		if d.By != nil {
			resolver["uid"] = d.By.String()
		}

		data["resolved_by"] = resolver
	}

	g.deps.Broker.Publish(events.ConnectionQueriesTopic(g.connectionUID.String()), events.EventApprovalResolved, data)
	g.deps.Broker.Publish(events.TopicApprovalsPending, events.EventApprovalResolved, data)

	if g.deps.Escalator != nil {
		g.deps.Escalator.Resolved(ctx, queryUID, status, d.ByName, d.Reason)
	}

	g.notifyReplicas(ctx, store.NotifyChannelApprovals, events.EventApprovalResolved, queryUID)
}

// notifyReplicas is best-effort: a failed NOTIFY degrades the stream on other
// replicas and nothing else. It must never surface to the database session.
func (g *ApprovalGate) notifyReplicas(ctx context.Context, channel, eventType string, queryUID uuid.UUID) {
	if g.deps.Store == nil {
		return
	}

	err := g.deps.Store.NotifyEvent(ctx, channel, store.EventNotification{
		Topic:    events.TopicApprovalsPending,
		Type:     eventType,
		QueryUID: queryUID,
		ConnUID:  g.connectionUID,
	})
	if err != nil {
		g.log(ctx, "failed to fan approval event to other replicas", slog.Any("error", err))
	}
}

func (g *ApprovalGate) log(ctx context.Context, msg string, args ...any) {
	if g.deps.Logger == nil {
		return
	}

	g.deps.Logger.WarnContext(ctx, msg, args...)
}
