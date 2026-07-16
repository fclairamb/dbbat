// Package notify implements notification channels for grant request
// lifecycle events. Today this is Slack-only. Notifications are posted
// outbound to a configured channel and, when an inbound transport is
// configured, carry interactive Approve / Deny buttons whose clicks flow
// back into the decision pipeline. Two inbound transports are supported: a
// signing-secret-verified HTTPS endpoint (see internal/api/slack_interactions.go)
// and an outbound Socket Mode connection for deployments that can't accept
// inbound Slack traffic (see internal/api/slack_socketmode.go). With neither
// configured the feature degrades to link-through-UI: messages have no
// buttons and the admin decides in the dbbat UI.
package notify

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"
	"github.com/slack-go/slack"

	"github.com/fclairamb/dbbat/internal/config"
	"github.com/fclairamb/dbbat/internal/store"
)

// GrantAction names the lifecycle event a notification refers to.
type GrantAction string

// Lifecycle actions a notification can describe. The cancel value matches
// the DB CHECK constraint spelling.
const (
	GrantActionCreated   GrantAction = "created"
	GrantActionApproved  GrantAction = "approved"
	GrantActionDenied    GrantAction = "denied"
	GrantActionCancelled GrantAction = "cancelled" //nolint:misspell // matches DB lifecycle value
)

// Slack interaction action IDs carried by the Approve / Deny buttons. The
// inbound interaction handler matches on these; kept here so the button
// rendering and the handler can't drift.
const (
	ActionApprove = "grant_request_approve"
	ActionDeny    = "grant_request_deny"
)

// ErrChannelMissing is returned by NewSlackNotifier when a bot token is
// set but no channel is configured.
var ErrChannelMissing = errors.New("DBB_SLACK_NOTIFY_BOT_TOKEN set without DBB_SLACK_NOTIFY_CHANNEL")

// ErrPublicURLMissing is returned by NewSlackNotifier when a bot token is
// set but no public URL is configured.
var ErrPublicURLMissing = errors.New("DBB_SLACK_NOTIFY_BOT_TOKEN set without DBB_PUBLIC_URL")

// ErrSigningSecretWithoutBotToken is returned by NewSlackNotifier when a
// signing secret is set but no bot token is: interactivity lives on
// notification messages, so it is meaningless without notifications.
var ErrSigningSecretWithoutBotToken = errors.New("DBB_SLACK_SIGNING_SECRET set without DBB_SLACK_NOTIFY_BOT_TOKEN")

// ErrAppTokenWithoutBotToken is returned by NewSlackNotifier when an
// app-level token (Socket Mode) is set but no bot token is: Socket Mode
// receives interactions that ride on notification messages and posts
// decisions with the bot token, so it is meaningless without notifications.
var ErrAppTokenWithoutBotToken = errors.New("DBB_SLACK_NOTIFY_APP_TOKEN set without DBB_SLACK_NOTIFY_BOT_TOKEN")

// GrantRequestEvent carries the data the notifier needs to render a
// message. Fields besides Request are looked up by the API handler before
// firing — denormalizing here keeps the notifier free of store lookups
// (so a slow DB doesn't block Slack and vice-versa).
type GrantRequestEvent struct {
	Action     GrantAction
	Request    *store.GrantRequest
	Definition *store.GrantDefinition
	Server     *store.Server
	Requester  *store.User
	// Decider is set when Action is approved/denied/canceled.
	Decider *store.User

	// RequesterSlackID is the requester's linked Slack user ID, or "" if
	// the requester has no linked Slack identity. Used to @-mention them.
	RequesterSlackID string
	// AdminSlackIDs are the Slack user IDs of admins with a linked Slack
	// identity, used to @-mention them on the pending message.
	AdminSlackIDs []string
	// DeciderSlackID is the decider's linked Slack user ID (set on decision
	// events), used to @-mention them in the thread reply.
	DeciderSlackID string
	// Interactive requests Approve / Deny buttons on the pending message.
	// Only honored for GrantActionCreated; every other render (decision,
	// cancel) rebuilds blocks without buttons so stale buttons disappear.
	Interactive bool
}

// SlackPersister is the slice of the store API the notifier uses to
// remember which Slack message corresponds to a request, so it can
// chat.update on follow-ups.
type SlackPersister interface {
	SetGrantRequestSlackMessage(ctx context.Context, uid uuid.UUID, channel, ts string) error
}

// SlackNotifier posts Block Kit messages to a configured channel for grant
// request lifecycle events. A nil notifier is a graceful no-op (returned
// from NewSlackNotifier when the bot token is unset).
type SlackNotifier struct {
	client        *slack.Client
	channel       string
	publicURL     string
	signingSecret string
	interactive   bool
	store         SlackPersister
	log           *slog.Logger
}

// NewSlackNotifier returns a configured notifier or nil when the feature is
// disabled. A startup error fires only when notification is enabled but
// dependent fields are missing — silent disable is intentional for
// deployments that just don't want it.
//
//nolint:nilnil // nil notifier is the documented "disabled" sentinel
func NewSlackNotifier(cfg config.SlackNotifyConfig, publicURL string, persister SlackPersister, log *slog.Logger) (*SlackNotifier, error) {
	if !cfg.Enabled() {
		// A signing secret or app-level token without a bot token is a
		// misconfiguration: interactivity rides on notification messages, so
		// it can't work without the notifier. Fail fast rather than silently
		// disabling.
		if cfg.SigningSecret != "" {
			return nil, ErrSigningSecretWithoutBotToken
		}

		if cfg.AppToken != "" {
			return nil, ErrAppTokenWithoutBotToken
		}

		log.InfoContext(context.Background(), "slack notifications: disabled (DBB_SLACK_NOTIFY_BOT_TOKEN unset)")

		return nil, nil
	}

	if cfg.Channel == "" {
		return nil, ErrChannelMissing
	}

	if publicURL == "" {
		return nil, ErrPublicURLMissing
	}

	return &SlackNotifier{
		client:        slack.New(cfg.BotToken),
		channel:       cfg.Channel,
		publicURL:     strings.TrimRight(publicURL, "/"),
		signingSecret: cfg.SigningSecret,
		interactive:   cfg.Interactive(),
		store:         persister,
		log:           log,
	}, nil
}

// Interactive reports whether Approve / Deny buttons are rendered and the
// inbound interaction endpoint should be served. A nil notifier is never
// interactive.
func (n *SlackNotifier) Interactive() bool {
	if n == nil {
		return false
	}

	return n.interactive
}

// SigningSecret returns the Slack app signing secret used to verify inbound
// interaction callbacks. Empty on a nil notifier or when interactivity is
// disabled.
func (n *SlackNotifier) SigningSecret() string {
	if n == nil {
		return ""
	}

	return n.signingSecret
}

// NotifyGrantRequest posts a fresh message on creation and chat.update's
// the existing message on subsequent lifecycle events. All errors are
// logged and swallowed — notifications are best-effort and must never
// fail the API request that triggered them.
func (n *SlackNotifier) NotifyGrantRequest(ctx context.Context, ev GrantRequestEvent) {
	if n == nil {
		return
	}

	if ev.Request == nil {
		n.log.WarnContext(ctx, "slack notify called with nil request")

		return
	}

	blocks := buildBlocks(ev, n.publicURL)

	switch ev.Action { //nolint:exhaustive // non-Created actions all share the update branch in the default case
	case GrantActionCreated:
		channel, ts, err := n.client.PostMessageContext(
			ctx,
			n.channel,
			slack.MsgOptionBlocks(blocks...),
			slack.MsgOptionAsUser(true),
		)
		if err != nil {
			n.log.WarnContext(ctx, "slack post failed", slog.Any("error", err))

			return
		}

		if n.store != nil {
			if err := n.store.SetGrantRequestSlackMessage(ctx, ev.Request.UID, channel, ts); err != nil {
				n.log.WarnContext(ctx, "persist slack message failed", slog.Any("error", err))
			}
		}

	default:
		channel := derefString(ev.Request.SlackChannel)
		ts := derefString(ev.Request.SlackMessageTS)

		if channel == "" || ts == "" {
			// No prior post (notifier was disabled when the request was
			// created, or the post failed). Send a fresh message instead
			// of dropping the update silently.
			if _, _, err := n.client.PostMessageContext(ctx, n.channel, slack.MsgOptionBlocks(blocks...)); err != nil {
				n.log.WarnContext(ctx, "slack post (fallback) failed", slog.Any("error", err))
			}

			return
		}

		if _, _, _, err := n.client.UpdateMessageContext(ctx, channel, ts, slack.MsgOptionBlocks(blocks...)); err != nil {
			n.log.WarnContext(ctx, "slack update failed", slog.Any("error", err))
		}
	}
}

// PostThreadReply posts a plain-text reply in the thread of an existing
// message (channel + ts). It notifies watchers of a decision — unlike a
// chat.update, which edits silently. Best-effort: errors are logged and
// swallowed, and a nil notifier or missing coordinates is a no-op.
func (n *SlackNotifier) PostThreadReply(ctx context.Context, channel, ts, text string) {
	if n == nil {
		return
	}

	if channel == "" || ts == "" {
		return
	}

	if _, _, err := n.client.PostMessageContext(
		ctx,
		channel,
		slack.MsgOptionText(text, false),
		slack.MsgOptionTS(ts),
	); err != nil {
		n.log.WarnContext(ctx, "slack thread reply failed", slog.Any("error", err))
	}
}

// buildBlocks renders a Block Kit message for the event. The same shape
// is used for create and update — only the status badge, mention line,
// decider line and (for pending + interactive) the action buttons change.
// The actions block is emitted only for GrantActionCreated + Interactive:
// every other render (decision, cancel) rebuilds without it, which is how
// stale buttons disappear once a request is decided.
func buildBlocks(ev GrantRequestEvent, publicURL string) []slack.Block {
	header := slack.NewHeaderBlock(slack.NewTextBlockObject(
		"plain_text",
		fmt.Sprintf("%s Grant request — %s", statusEmoji(ev), userLabel(ev.Requester)),
		false,
		false,
	))

	blocks := []slack.Block{header}

	if mention := mentionLine(ev); mention != "" {
		blocks = append(blocks, slack.NewSectionBlock(
			slack.NewTextBlockObject("mrkdwn", mention, false, false), nil, nil,
		))
	}

	blocks = append(blocks, slack.NewSectionBlock(
		slack.NewTextBlockObject("mrkdwn", mainSectionText(ev), false, false), nil, nil,
	))

	if ev.Action == GrantActionCreated && ev.Interactive {
		blocks = append(blocks, actionsBlock(ev.Request.UID.String()))
	}

	link := fmt.Sprintf("%s/app/grant-requests", publicURL)
	blocks = append(blocks, slack.NewContextBlock("",
		slack.NewTextBlockObject("mrkdwn", fmt.Sprintf("<%s|Review in dbbat →>", link), false, false),
	))

	return blocks
}

// mainSectionText renders the *Server* / *Definition* / … detail section.
func mainSectionText(ev GrantRequestEvent) string {
	dbName := "—"
	if ev.Server != nil {
		dbName = ev.Server.Name
	}

	defName := "—"

	durationText := ""

	if ev.Definition != nil {
		defName = ev.Definition.Name
		durationText = formatDuration(ev.Definition.DurationSeconds)
	}

	mainText := fmt.Sprintf(
		"*Server*: %s\n*Definition*: %s\n*Duration*: %s\n*Status*: %s",
		dbName, defName, durationText, statusLabel(ev),
	)

	if ev.Action != GrantActionCreated && ev.Decider != nil {
		mainText += fmt.Sprintf("\n*%s by*: %s",
			strings.Title(string(ev.Action)), //nolint:staticcheck // titlecase short verb is fine here
			userLabel(ev.Decider),
		)
	}

	if ev.Request.Justification != "" {
		mainText += "\n*Justification*: " + ev.Request.Justification
	}

	if ev.Request.DecisionReason != nil && *ev.Request.DecisionReason != "" {
		mainText += "\n*Reason*: " + *ev.Request.DecisionReason
	}

	return mainText
}

// mentionLine builds the "@requester requested access … @admin1, @admin2 —
// please approve or deny." sentence for the pending message. Linked users
// render as <@SLACK_ID>; unlinked users fall back to their plain username.
// The second sentence is dropped when no admins have a linked Slack
// identity (nobody to ping). Returns "" for non-created events.
func mentionLine(ev GrantRequestEvent) string {
	if ev.Action != GrantActionCreated {
		return ""
	}

	requester := slackMention(ev.RequesterSlackID, userLabel(ev.Requester))

	dbName := "—"
	if ev.Server != nil {
		dbName = ev.Server.Name
	}

	defName := "access"
	if ev.Definition != nil {
		defName = ev.Definition.Name
	}

	line := fmt.Sprintf("%s requested access on *%s* with *%s*.", requester, dbName, defName)

	if len(ev.AdminSlackIDs) > 0 {
		mentions := make([]string, 0, len(ev.AdminSlackIDs))
		for _, id := range ev.AdminSlackIDs {
			mentions = append(mentions, slackMention(id, ""))
		}

		line += fmt.Sprintf(" %s — please approve or deny.", strings.Join(mentions, ", "))
	}

	return line
}

// actionsBlock renders the Approve / Deny buttons. Both carry the request
// UID as value and a native confirm dialog to guard against fat-finger
// decisions.
func actionsBlock(requestUID string) *slack.ActionBlock {
	approve := slack.NewButtonBlockElement(
		ActionApprove, requestUID,
		slack.NewTextBlockObject("plain_text", "✅ Approve", true, false),
	).WithStyle(slack.StylePrimary).WithConfirm(slack.NewConfirmationBlockObject(
		slack.NewTextBlockObject("plain_text", "Approve grant request?", false, false),
		slack.NewTextBlockObject("plain_text", "This grants the requested access immediately.", false, false),
		slack.NewTextBlockObject("plain_text", "Approve", false, false),
		slack.NewTextBlockObject("plain_text", "Cancel", false, false),
	))

	deny := slack.NewButtonBlockElement(
		ActionDeny, requestUID,
		slack.NewTextBlockObject("plain_text", "❌ Deny", true, false),
	).WithStyle(slack.StyleDanger).WithConfirm(slack.NewConfirmationBlockObject(
		slack.NewTextBlockObject("plain_text", "Deny grant request?", false, false),
		slack.NewTextBlockObject("plain_text", "This rejects the request. It cannot be undone.", false, false),
		slack.NewTextBlockObject("plain_text", "Deny", false, false),
		slack.NewTextBlockObject("plain_text", "Cancel", false, false),
	))

	return slack.NewActionBlock("grant_request_actions", approve, deny)
}

// slackMention renders a Slack @-mention for a linked user (<@ID>) or falls
// back to the plain username for users without a linked Slack identity.
// A blank fallback yields "" so callers can skip empty entries.
func slackMention(slackID, fallback string) string {
	if slackID != "" {
		return "<@" + slackID + ">"
	}

	return fallback
}

func userLabel(u *store.User) string {
	if u == nil {
		return "(unknown)"
	}

	return u.Username
}

// isAutoApproved reports whether an approved event has no human decider —
// i.e. the definition's AutoApprove policy decided it, not an admin.
func isAutoApproved(ev GrantRequestEvent) bool {
	return ev.Action == GrantActionApproved && ev.Decider == nil
}

func statusEmoji(ev GrantRequestEvent) string {
	switch {
	case isAutoApproved(ev):
		return "⚡"
	case ev.Action == GrantActionCreated:
		return "🔐"
	case ev.Action == GrantActionApproved:
		return "✅"
	case ev.Action == GrantActionDenied:
		return "❌"
	case ev.Action == GrantActionCancelled:
		return "🚫"
	default:
		return "❔"
	}
}

func statusLabel(ev GrantRequestEvent) string {
	if isAutoApproved(ev) {
		return "auto-approved"
	}

	switch ev.Action {
	case GrantActionCreated:
		return "pending"
	case GrantActionApproved:
		return "approved"
	case GrantActionDenied:
		return "denied"
	case GrantActionCancelled:
		return "cancelled" //nolint:misspell // matches DB lifecycle
	default:
		return string(ev.Action)
	}
}

func formatDuration(seconds int64) string {
	switch {
	case seconds%86400 == 0:
		return fmt.Sprintf("%dd", seconds/86400)
	case seconds%3600 == 0:
		return fmt.Sprintf("%dh", seconds/3600)
	case seconds%60 == 0:
		return fmt.Sprintf("%dm", seconds/60)
	default:
		return fmt.Sprintf("%ds", seconds)
	}
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}

	return *s
}
