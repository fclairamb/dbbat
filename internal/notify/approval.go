package notify

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/slack-go/slack"
)

// Slack interaction action IDs carried by the approval Approve / Deny buttons.
// Distinct from the grant-request ones so the inbound handler can tell a
// query hold from a grant request by action id alone.
const (
	// ActionApproveQuery releases a held statement.
	ActionApproveQuery = "query_approval_approve"
	// ActionDenyQuery rejects a held statement.
	ActionDenyQuery = "query_approval_deny"
)

// ApprovalHold is what the escalator needs to render a message.
type ApprovalHold struct {
	QueryUID      uuid.UUID
	ConnectionUID uuid.UUID
	Username      string
	DatabaseName  string
	SQL           string
	Pattern       string
	StartedAt     time.Time
}

// ApprovalEscalator posts a Slack message for a hold that is still pending
// after a fixed delay, and updates that message in place the moment the hold
// resolves — by any route.
//
// The trigger is purely time-based. There is no live-subscriber detection and
// no cluster-wide presence bookkeeping: if an admin was watching, they had the
// delay to act, and resolving the hold cancels the pending notification. That
// behaves identically in the case that actually matters (nobody watching) and
// is dramatically simpler.
//
// Exactly one instance posts per hold because the timer is owned by the
// replica running the parked session — the session is pinned to one pod
// anyway, so no election is needed.
type ApprovalEscalator struct {
	notifier *SlackNotifier
	delay    time.Duration
	// includeSQL controls whether query text is copied into Slack. Slack is a
	// lower trust boundary than the dbbat UI and this feature pipes production
	// SQL into it, so it is switchable off.
	includeSQL bool
	log        *slog.Logger

	mu      sync.Mutex
	pending map[uuid.UUID]*escalation
}

// escalation is the per-hold timer plus, once fired, the posted message's
// coordinates so it can be edited in place.
type escalation struct {
	timer   *time.Timer
	hold    ApprovalHold
	channel string
	ts      string
	posted  bool
}

// NewApprovalEscalator returns an escalator, or nil when escalation is
// disabled (no notifier, or a non-positive delay). A nil escalator is a
// no-op on every method.
func NewApprovalEscalator(notifier *SlackNotifier, delay time.Duration, includeSQL bool, log *slog.Logger) *ApprovalEscalator {
	if notifier == nil || delay <= 0 {
		return nil
	}

	return &ApprovalEscalator{
		notifier:   notifier,
		delay:      delay,
		includeSQL: includeSQL,
		log:        log,
		pending:    make(map[uuid.UUID]*escalation),
	}
}

// Schedule arms the escalation timer for a hold.
func (e *ApprovalEscalator) Schedule(ctx context.Context, hold ApprovalHold) {
	if e == nil {
		return
	}

	esc := &escalation{hold: hold}

	e.mu.Lock()
	if _, exists := e.pending[hold.QueryUID]; exists {
		e.mu.Unlock()

		return
	}

	e.pending[hold.QueryUID] = esc
	e.mu.Unlock()

	// Detach from the request context: the notification is about a hold that
	// outlives whatever HTTP/proxy context created it.
	postCtx := context.WithoutCancel(ctx)

	esc.timer = time.AfterFunc(e.delay, func() { e.fire(postCtx, hold.QueryUID) })
}

// fire posts the escalation message once the delay elapses without a decision.
func (e *ApprovalEscalator) fire(ctx context.Context, queryUID uuid.UUID) {
	e.mu.Lock()
	esc, ok := e.pending[queryUID]
	e.mu.Unlock()

	if !ok {
		return
	}

	blocks := e.buildHoldBlocks(esc.hold, "", "", "")

	channel, ts, err := e.notifier.client.PostMessageContext(
		ctx, e.notifier.channel, slack.MsgOptionBlocks(blocks...),
	)
	if err != nil {
		if e.log != nil {
			e.log.WarnContext(ctx, "approval escalation post failed", slog.Any("error", err))
		}

		return
	}

	e.mu.Lock()

	// Resolved while the post was in flight — edit it straight away rather
	// than leaving a live Approve button on a dead hold.
	current, still := e.pending[queryUID]
	if still {
		current.channel = channel
		current.ts = ts
		current.posted = true
	}
	e.mu.Unlock()

	if !still {
		e.updateMessage(ctx, channel, ts, esc.hold, "abandoned", "", "resolved before the notification landed")
	}
}

// Resolved cancels a not-yet-fired escalation, or rewrites the posted message
// in place if it already fired.
//
// Because there is no approval timeout, a posted message would otherwise stay
// actionable indefinitely — and a stale Approve button that silently no-ops is
// worse than no button at all, especially when it can linger for hours.
func (e *ApprovalEscalator) Resolved(ctx context.Context, queryUID uuid.UUID, status, byName, reason string) {
	if e == nil {
		return
	}

	e.mu.Lock()
	esc, ok := e.pending[queryUID]

	if ok {
		delete(e.pending, queryUID)
	}
	e.mu.Unlock()

	if !ok {
		return
	}

	if esc.timer != nil {
		esc.timer.Stop()
	}

	if !esc.posted {
		return
	}

	e.updateMessage(context.WithoutCancel(ctx), esc.channel, esc.ts, esc.hold, status, byName, reason)
}

func (e *ApprovalEscalator) updateMessage(ctx context.Context, channel, ts string, hold ApprovalHold, status, byName, reason string) {
	if channel == "" || ts == "" {
		return
	}

	blocks := e.buildHoldBlocks(hold, status, byName, reason)

	if _, _, _, err := e.notifier.client.UpdateMessageContext(ctx, channel, ts, slack.MsgOptionBlocks(blocks...)); err != nil {
		if e.log != nil {
			e.log.WarnContext(ctx, "approval escalation update failed", slog.Any("error", err))
		}
	}
}

// buildHoldBlocks renders the message. Approve/Deny buttons appear only while
// the hold is still pending *and* an inbound interaction transport is
// configured; every resolved render drops them, which is how stale buttons
// disappear.
func (e *ApprovalEscalator) buildHoldBlocks(hold ApprovalHold, status, byName, reason string) []slack.Block {
	header := slack.NewHeaderBlock(slack.NewTextBlockObject(
		"plain_text",
		fmt.Sprintf("%s Query awaiting approval — %s", approvalEmoji(status), hold.Username),
		false, false,
	))

	body := fmt.Sprintf("*User*: %s\n*Database*: %s\n*Matched pattern*: `%s`\n*Held for*: %s\n*Status*: %s",
		hold.Username,
		hold.DatabaseName,
		hold.Pattern,
		formatHeldFor(hold.StartedAt),
		approvalStatusLabel(status),
	)

	if byName != "" {
		body += "\n*Resolved by*: " + byName
	}

	if reason != "" {
		body += "\n*Reason*: " + reason
	}

	blocks := []slack.Block{
		header,
		slack.NewSectionBlock(slack.NewTextBlockObject("mrkdwn", body, false, false), nil, nil),
	}

	if e.includeSQL && hold.SQL != "" {
		blocks = append(blocks, slack.NewSectionBlock(
			slack.NewTextBlockObject("mrkdwn", "```"+truncateSQL(hold.SQL, MaxSlackSQLLength)+"```", false, false), nil, nil,
		))
	}

	if status == "" && e.notifier.Interactive() {
		blocks = append(blocks, approvalActionsBlock(hold.QueryUID.String()))
	}

	link := fmt.Sprintf("%s/app/connections/%s?watch=1", e.notifier.publicURL, hold.ConnectionUID)
	blocks = append(blocks, slack.NewContextBlock("",
		slack.NewTextBlockObject("mrkdwn", fmt.Sprintf("<%s|Watch this connection in dbbat →>", link), false, false),
	))

	return blocks
}

// MaxSlackSQLLength bounds the query text copied into a Slack message.
const MaxSlackSQLLength = 500

// approvalActionsBlock renders the Approve / Deny buttons, carrying the *query*
// uid — approval is by query uid, and the waiting session re-checks that it is
// its own before acting on the decision.
func approvalActionsBlock(queryUID string) *slack.ActionBlock {
	approve := slack.NewButtonBlockElement(
		ActionApproveQuery, queryUID,
		slack.NewTextBlockObject("plain_text", "✅ Approve", true, false),
	).WithStyle(slack.StylePrimary).WithConfirm(slack.NewConfirmationBlockObject(
		slack.NewTextBlockObject("plain_text", "Approve this query?", false, false),
		slack.NewTextBlockObject("plain_text", "The statement runs immediately against the target database.", false, false),
		slack.NewTextBlockObject("plain_text", "Approve", false, false),
		slack.NewTextBlockObject("plain_text", "Cancel", false, false),
	))

	deny := slack.NewButtonBlockElement(
		ActionDenyQuery, queryUID,
		slack.NewTextBlockObject("plain_text", "❌ Deny", true, false),
	).WithStyle(slack.StyleDanger).WithConfirm(slack.NewConfirmationBlockObject(
		slack.NewTextBlockObject("plain_text", "Deny this query?", false, false),
		slack.NewTextBlockObject("plain_text", "The client gets an error. It cannot be undone.", false, false),
		slack.NewTextBlockObject("plain_text", "Deny", false, false),
		slack.NewTextBlockObject("plain_text", "Cancel", false, false),
	))

	return slack.NewActionBlock("query_approval_actions", approve, deny)
}

func approvalEmoji(status string) string {
	switch status {
	case "":
		return "⏸️"
	case "approved":
		return "✅"
	case "denied":
		return "❌"
	case "abandoned":
		return "🚪"
	default:
		return "❔"
	}
}

// approvalStatusLabel spells out the outcome. "abandoned" is deliberately
// worded so nobody reads it as a rejection: the client gave up, nothing ran,
// and there is nothing left to approve.
func approvalStatusLabel(status string) string {
	switch status {
	case "":
		return "pending — waiting for an approver"
	case "approved":
		return "approved — the statement ran"
	case "denied":
		return "denied — the client got an error"
	case "abandoned":
		return "abandoned — the client gave up before anyone decided (nothing ran)"
	default:
		return status
	}
}

func formatHeldFor(since time.Time) string {
	if since.IsZero() {
		return "—"
	}

	d := time.Since(since).Round(time.Second)

	return d.String()
}

func truncateSQL(sql string, limit int) string {
	if len(sql) <= limit {
		return sql
	}

	return sql[:limit] + " …(truncated)"
}
