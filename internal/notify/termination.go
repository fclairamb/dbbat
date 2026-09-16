package notify

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/slack-go/slack"

	"github.com/fclairamb/dbbat/internal/store"
)

// terminationCoalesceWindow is how long repeated terminations for the same
// (user, database, reason) are folded into one follow-up message instead of
// flooding the channel — see NotifyTermination. A runaway client reconnecting
// in a loop and timing out every time is the case this exists for.
const terminationCoalesceWindow = 10 * time.Minute

// TerminationSQLHeadLength bounds the statement text copied into a
// termination Slack message — shorter than approval's MaxSlackSQLLength
// because this is a one-line "what was running" pointer, not the full
// statement an approver needs to decide on.
const TerminationSQLHeadLength = 200

// terminationCoalesceKey groups repeated terminations that would otherwise
// each post their own message: the same user hitting the same limit on the
// same database, over and over.
type terminationCoalesceKey struct {
	userUID     uuid.UUID
	databaseUID uuid.UUID
	reason      string
}

// terminationCoalesce is the in-memory state for one open coalescing window:
// how many terminations landed after the first (which already posted), and
// the timer that closes the window and posts the follow-up. Per-replica —
// like ApprovalEscalator, that is an accepted trade for a signal this cheap.
type terminationCoalesce struct {
	timer *time.Timer
	extra int
}

// NotifyTermination posts a Slack message for a dbbat-initiated termination.
// The first termination for a given (user, database, reason) posts
// immediately; every further one within the coalescing window is counted and
// folded into a single follow-up ("+N more in the last 10 min") posted when
// the window closes, rather than flooding the channel.
//
// Best-effort like NotifyGrantRequest: errors are logged and swallowed. The
// caller (Store.notifyTermination) already runs this from its own detached,
// bounded goroutine, so blocking here only ever delays this notification,
// never the session teardown that triggered it.
func (n *SlackNotifier) NotifyTermination(ctx context.Context, ev store.TerminationEvent) {
	if n == nil {
		return
	}

	key := terminationCoalesceKey{reason: ev.Reason}
	if ev.User != nil {
		key.userUID = ev.User.UID
	}

	if ev.Database != nil {
		key.databaseUID = ev.Database.UID
	}

	n.terminationMu.Lock()

	if pending, exists := n.terminationPending[key]; exists {
		pending.extra++
		n.terminationMu.Unlock()

		return
	}

	// Claimed before the post goes out (and before we hold the lock again):
	// a termination arriving while chat.postMessage is on the wire must land
	// as a coalesced "+1", not a second top-level post.
	n.terminationPending[key] = &terminationCoalesce{}
	n.terminationMu.Unlock()

	blocks := buildTerminationBlocks(ev, n.publicURL, n.terminationSQL)

	if _, _, err := n.client.PostMessageContext(ctx, n.channel, slack.MsgOptionBlocks(blocks...)); err != nil {
		n.log.WarnContext(ctx, "slack termination post failed", slog.Any("error", err))
	}

	window := n.terminationWindow
	if window <= 0 {
		window = terminationCoalesceWindow
	}

	timer := time.AfterFunc(window, func() {
		n.flushTerminationCoalesce(context.WithoutCancel(ctx), key)
	})

	n.terminationMu.Lock()
	if pending, exists := n.terminationPending[key]; exists {
		pending.timer = timer
	} else {
		// Flushed already somehow (should not happen: only the timer above
		// deletes the entry) — don't leak the timer.
		timer.Stop()
	}
	n.terminationMu.Unlock()
}

// flushTerminationCoalesce closes one coalescing window: if anything landed
// after the first post, it posts the "+N more" follow-up.
func (n *SlackNotifier) flushTerminationCoalesce(ctx context.Context, key terminationCoalesceKey) {
	n.terminationMu.Lock()
	pending, exists := n.terminationPending[key]
	delete(n.terminationPending, key)
	n.terminationMu.Unlock()

	if !exists || pending.extra == 0 {
		return
	}

	text := fmt.Sprintf("+%d more in the last %s", pending.extra, terminationCoalesceWindow)

	if _, _, err := n.client.PostMessageContext(ctx, n.channel, slack.MsgOptionText(text, false)); err != nil {
		n.log.WarnContext(ctx, "slack termination coalesce post failed", slog.Any("error", err))
	}
}

// buildTerminationBlocks renders the Block Kit message for one termination.
// No buttons — there is no decision left to make, only something to know
// happened.
func buildTerminationBlocks(ev store.TerminationEvent, publicURL string, includeSQL bool) []slack.Block {
	header := slack.NewHeaderBlock(slack.NewTextBlockObject(
		"plain_text",
		fmt.Sprintf("🛑 dbbat ended a session on %s", terminationDatabaseLabel(ev)),
		false, false,
	))

	blocks := []slack.Block{
		header,
		slack.NewSectionBlock(slack.NewTextBlockObject("mrkdwn", terminationMainText(ev), false, false), nil, nil),
	}

	if includeSQL && ev.QueryHead != "" {
		blocks = append(blocks, slack.NewSectionBlock(
			slack.NewTextBlockObject(
				"mrkdwn", "```"+truncateSQL(ev.QueryHead, TerminationSQLHeadLength)+"```", false, false,
			), nil, nil,
		))
	}

	if publicURL != "" && ev.Connection != nil {
		link := fmt.Sprintf("%s/app/connections/%s", publicURL, ev.Connection.UID)
		blocks = append(blocks, slack.NewContextBlock("",
			slack.NewTextBlockObject("mrkdwn", fmt.Sprintf("<%s|Open connection →>", link), false, false),
		))
	}

	return blocks
}

// terminationMainText renders the "*User*: … · *Grant*: …\n*Reason*: …" body.
func terminationMainText(ev store.TerminationEvent) string {
	text := "*User*: " + terminationUserLabel(ev)

	if ev.Grant != nil && ev.Grant.Definition != nil {
		text += fmt.Sprintf(" · *Grant*: %s", ev.Grant.Definition.Name)
	}

	text += "\n*Reason*: " + terminationReasonText(ev)

	return text
}

func terminationUserLabel(ev store.TerminationEvent) string {
	if ev.User == nil {
		return "(unknown)"
	}

	return slackMention(ev.UserSlackID, ev.User.Username)
}

func terminationDatabaseLabel(ev store.TerminationEvent) string {
	if ev.Database == nil {
		return "(unknown)"
	}

	return ev.Database.Name
}

// terminationReasonText is the one line that tells a reader why dbbat acted.
// The admin case reads as a human decision ("Terminated by … : …"); the
// watchdog cases read as a limit crossed.
func terminationReasonText(ev store.TerminationEvent) string {
	switch ev.Reason {
	case store.TerminationAdminTerminated:
		by := ev.TerminatedBy
		if by == "" {
			by = "an admin"
		}

		if ev.Detail != "" {
			return fmt.Sprintf("Terminated by %s: %s", by, ev.Detail)
		}

		return "Terminated by " + by

	case store.TerminationStatementTimeout:
		switch {
		case ev.Limit > 0 && ev.Ran > 0:
			return fmt.Sprintf("statement exceeded the *%s* limit (ran %s)", ev.Limit, ev.Ran.Round(100*time.Millisecond))
		case ev.Limit > 0:
			return fmt.Sprintf("statement exceeded the *%s* limit", ev.Limit)
		default:
			return "statement exceeded the per-statement time limit"
		}

	case store.TerminationQuotaExceeded:
		return "the grant's transfer quota was exceeded"

	default:
		return ev.Reason
	}
}
