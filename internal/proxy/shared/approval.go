package shared

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync/atomic"
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
	// the client disconnected, the query was canceled, the grant expired, or
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
	compiled *CompiledApprovalPatterns

	connectionUID uuid.UUID
	userUID       uuid.UUID
	username      string
	databaseName  string

	// resolved remembers the outcome of the last hold this session parked, so
	// the statement's *completion* event can carry it — see ResolutionFor.
	// Written by the session goroutine, read by the goroutine that persists and
	// publishes the executed query, hence the atomic.
	resolved atomic.Pointer[ApprovalResolution]
}

// ApprovalResolution is the terminal outcome of a hold, kept on the gate for
// exactly as long as it takes the released statement to finish and publish.
type ApprovalResolution struct {
	QueryUID uuid.UUID
	Status   string
	ByUID    *uuid.UUID
	ByName   string
	Reason   string
	At       time.Time
}

// ResolutionFor reports the outcome of a hold this session parked, but only
// for the statement it last resolved.
//
// A session parks at most one statement at a time — the session goroutine is
// blocked for the whole hold — so remembering just the most recent resolution
// covers the completion event that immediately follows it, without the gate
// accumulating per-query state for the lifetime of a long-lived session. A
// miss simply means the completion event carries no approval fields, which is
// what it did before.
func (g *ApprovalGate) ResolutionFor(queryUID uuid.UUID) *ApprovalResolution {
	if g == nil || queryUID == uuid.Nil {
		return nil
	}

	r := g.resolved.Load()
	if r == nil || r.QueryUID != queryUID {
		return nil
	}

	return r
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

	g.compiled = CompileApprovalPatterns(grant.ApprovalPatterns())

	for _, ce := range g.compiled.CompileErrors() {
		if deps.Logger != nil {
			deps.Logger.WarnContext(context.Background(), "skipping invalid approval pattern",
				slog.String("pattern", ce.Pattern), slog.Any("error", ce.Err))
		}
	}

	return g
}

// Active reports whether this gate can hold anything at all.
func (g *ApprovalGate) Active() bool {
	return g != nil && g.deps.Enabled && g.compiled.Len() > 0
}

// Match reports the first pattern the statement matches. Matching runs on the
// same normalized SQL text the static validators use, so an operator writing a
// pattern does not have to reason about a second normalization.
//
// Delegates to CompiledApprovalPatterns.Match, the same code the
// validate-patterns API endpoint calls — so a "test patterns" panel built on
// that endpoint reports exactly what the live gate will do, not a
// reimplementation that can drift from it.
func (g *ApprovalGate) Match(sql string) (string, bool) {
	if !g.Active() {
		return "", false
	}

	return g.compiled.Match(sql)
}

// NormalizeSQL is the canonical normalization applied before pattern matching:
// a trim, and nothing else.
//
// Note the deliberate divergence from the static validators (IsWriteQuery,
// IsDDLQuery, IsPasswordChangeQuery), which upper-case before their
// keyword-prefix checks and are therefore case-insensitive for free. An
// approval pattern is a full regexp an operator writes and then reads back
// against the SQL shown in /queries, so rewriting the statement first would
// make patterns behave differently from what the UI displays.
//
// The cost is that `^DELETE` does not match `delete from …`. Patterns should
// carry `(?i)` — which the definition form's placeholder and docs/approvals.md
// both teach — because a pattern that misses is a hold that never happens.
func NormalizeSQL(sql string) string {
	return strings.TrimSpace(sql)
}

// PatternCompileEntry is the compile outcome for one approval pattern
// source, in the order the source was given to CompileApprovalPatterns.
// Exactly one of Regexp/Err is set: Err nil means the pattern compiled.
type PatternCompileEntry struct {
	Pattern string
	Regexp  *regexp.Regexp
	Err     error
}

// PatternCompileError names one approval pattern source that failed to
// compile, and why. A narrower view of PatternCompileEntry for callers that
// only care about the failures.
type PatternCompileError struct {
	Pattern string
	Err     error
}

// CompiledApprovalPatterns is a set of RE2 approval-pattern sources compiled
// once, ready to match SQL with exactly ApprovalGate.Match's semantics:
// normalize with NormalizeSQL, then report the first pattern (in the order
// given) that matches — patterns that failed to compile never match anything.
//
// This type is the single place that logic lives. NewApprovalGate uses it to
// build the live per-session gate, and the grant-definition
// validate-patterns API endpoint uses it to preview what that gate will do —
// so the "test patterns" panel in the UI can never drift from proxy
// behavior by reimplementing the match loop separately.
type CompiledApprovalPatterns struct {
	// entries holds one item per source, in the exact order given to
	// CompileApprovalPatterns — index-aligned with the caller's input, which
	// is what lets the validate-patterns API report per-pattern results
	// without having to re-associate them by source text (sources can
	// repeat).
	entries []PatternCompileEntry
}

// CompileApprovalPatterns compiles every pattern source. A pattern that fails
// to compile is recorded on its PatternCompileEntry rather than aborting the
// batch, so one typo does not hide the compile errors — or the matches — of
// every other pattern.
func CompileApprovalPatterns(sources []string) *CompiledApprovalPatterns {
	c := &CompiledApprovalPatterns{entries: make([]PatternCompileEntry, len(sources))}

	for i, src := range sources {
		re, err := regexp.Compile(src)
		c.entries[i] = PatternCompileEntry{Pattern: src, Regexp: re, Err: err}
	}

	return c
}

// Len reports how many patterns compiled successfully.
func (c *CompiledApprovalPatterns) Len() int {
	if c == nil {
		return 0
	}

	n := 0

	for _, e := range c.entries {
		if e.Err == nil {
			n++
		}
	}

	return n
}

// Entries reports the compile outcome of every source, in the order given to
// CompileApprovalPatterns — one entry per source, success or failure.
func (c *CompiledApprovalPatterns) Entries() []PatternCompileEntry {
	if c == nil {
		return nil
	}

	return c.entries
}

// CompileErrors reports every pattern that failed to compile, in source
// order. Empty when every pattern compiled.
func (c *CompiledApprovalPatterns) CompileErrors() []PatternCompileError {
	if c == nil {
		return nil
	}

	var errs []PatternCompileError

	for _, e := range c.entries {
		if e.Err != nil {
			errs = append(errs, PatternCompileError{Pattern: e.Pattern, Err: e.Err})
		}
	}

	return errs
}

// Match reports the first successfully-compiled pattern that matches sql
// after NormalizeSQL, or false if none does.
func (c *CompiledApprovalPatterns) Match(sql string) (string, bool) {
	if c == nil {
		return "", false
	}

	normalized := NormalizeSQL(sql)

	for _, e := range c.entries {
		if e.Regexp != nil && e.Regexp.MatchString(normalized) {
			return e.Pattern, true
		}
	}

	return "", false
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
	// OnPending, when set, is called with the pending row's uid the instant
	// it exists — before the session blocks. Protocols use it to publish the
	// uid to their out-of-band cancellation path (PostgreSQL CancelRequest,
	// MySQL KILL QUERY, Mongo killOperations), which must be able to end a
	// hold that has not returned yet.
	OnPending func(queryUID uuid.UUID)
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

	// Announce the uid to the protocol's cancellation path before blocking.
	if req.OnPending != nil {
		req.OnPending(pending.UID)
		defer req.OnPending(uuid.Nil)
	}

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
				// Abandoned, or anything unexpected: fail closed.
				//
				// Persist here, do not merely announce. Approve/deny arrive
				// from the API, which has already written the row; an
				// *abandon* delivered through the registry has no such
				// writer — that is how an out-of-band cancel (PostgreSQL
				// CancelRequest, MySQL KILL QUERY, Mongo killOperations) and
				// the shutdown drain reach a parked session. Skipping the
				// write would unblock the session correctly while leaving
				// approval_status = 'pending' forever: a phantom hold that
				// haunts /queries/pending, the watch panel and the global
				// badge, and that a later POST /approve would happily
				// "release" although nothing is waiting on it.
				g.persistResolution(ctx, pending.UID, store.ApprovalAbandoned, d.By, d.Reason)
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
	g.persistResolution(ctx, queryUID, store.ApprovalAbandoned, nil, reason)

	g.announceResolved(ctx, queryUID, store.ApprovalAbandoned, approval.Decision{
		QueryUID: queryUID,
		Status:   store.ApprovalAbandoned,
		Reason:   reason,
		At:       time.Now(),
	})
}

// persistResolution writes the terminal approval state. The store's
// compare-and-set on approval_status = 'pending' makes this safe to call even
// when the API already wrote the same decision (approve/deny): the second
// write simply affects no rows.
//
// The session's own context may already be canceled — a client disconnect and
// a shutdown drain both arrive that way — so the write runs on a detached
// context. Otherwise the row would stay 'pending' forever in exactly the cases
// where nothing else will ever fix it.
func (g *ApprovalGate) persistResolution(
	ctx context.Context, queryUID uuid.UUID, status string, by *uuid.UUID, reason string,
) {
	if g.deps.Store == nil {
		return
	}

	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), resolutionWriteTimeout)
	defer cancel()

	if err := g.deps.Store.ResolveQueryApproval(writeCtx, queryUID, status, by, reason); err != nil &&
		!errors.Is(err, store.ErrQueryNotPending) {
		g.log(ctx, "failed to persist approval resolution",
			slog.String("status", status), slog.Any("error", err))
	}
}

// resolutionWriteTimeout bounds the detached terminal-state write.
const resolutionWriteTimeout = 5 * time.Second

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
	// Remember the outcome before announcing it: an approved statement is
	// forwarded the moment Hold returns, and its completion event has to be
	// able to say it was approved and by whom.
	g.resolved.Store(&ApprovalResolution{
		QueryUID: queryUID,
		Status:   status,
		ByUID:    d.By,
		ByName:   d.ByName,
		Reason:   d.Reason,
		At:       d.At,
	})

	data := map[string]any{
		"query_uid":         queryUID.String(),
		"connection_uid":    g.connectionUID.String(),
		"approval_required": true,
		"approval_status":   status,
		"resolution_reason": d.Reason,
		"resolved_at":       d.At,
	}

	if resolver := resolverData(d.By, d.ByName); resolver != nil {
		data["resolved_by"] = resolver
	}

	g.deps.Broker.Publish(events.ConnectionQueriesTopic(g.connectionUID.String()), events.EventApprovalResolved, data)
	g.deps.Broker.Publish(events.TopicApprovalsPending, events.EventApprovalResolved, data)

	if g.deps.Escalator != nil {
		g.deps.Escalator.Resolved(ctx, queryUID, status, d.ByName, d.Reason)
	}

	g.notifyReplicas(ctx, store.NotifyChannelApprovals, events.EventApprovalResolved, queryUID)
}

// resolverData renders the human who resolved a hold for the stream payload,
// or nil when nobody did (an abandoned hold has no approver). Shared by the
// resolution event and the released statement's completion event so the two
// cannot describe the same person differently.
func resolverData(byUID *uuid.UUID, byName string) map[string]any {
	if byUID == nil && byName == "" {
		return nil
	}

	resolver := map[string]any{"display_name": byName, "username": byName}
	if byUID != nil {
		resolver["uid"] = byUID.String()
	}

	return resolver
}

// notifyReplicas is best-effort: a failed NOTIFY degrades the stream on other
// replicas and nothing else. It must never surface to the database session.
func (g *ApprovalGate) notifyReplicas(ctx context.Context, channel, eventType string, queryUID uuid.UUID) {
	if g.deps.Store == nil {
		return
	}

	// Detached: a resolution is fanned out precisely when the session is
	// ending (disconnect, shutdown), so a canceled session context must not
	// swallow the notification other replicas are waiting for.
	notifyCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), resolutionWriteTimeout)
	defer cancel()

	err := g.deps.Store.NotifyEvent(notifyCtx, channel, store.EventNotification{
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
