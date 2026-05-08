// Package notify implements outbound notification channels for grant
// request lifecycle events. Today this is Slack-only and outbound-only —
// no inbound webhooks, no interactive buttons. The admin sees the post in
// Slack, follows the link to the dbbat UI, and decides there.
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

const (
	GrantActionCreated   GrantAction = "created"
	GrantActionApproved  GrantAction = "approved"
	GrantActionDenied    GrantAction = "denied"
	GrantActionCancelled GrantAction = "cancelled" //nolint:misspell // matches DB lifecycle value
)

// GrantRequestEvent carries the data the notifier needs to render a
// message. Fields besides Request are looked up by the API handler before
// firing — denormalizing here keeps the notifier free of store lookups
// (so a slow DB doesn't block Slack and vice-versa).
type GrantRequestEvent struct {
	Action     GrantAction
	Request    *store.GrantRequest
	Definition *store.GrantDefinition
	Database   *store.Database
	Requester  *store.User
	// Decider is set when Action is approved/denied/cancelled.
	Decider *store.User
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
	client    *slack.Client
	channel   string
	publicURL string
	store     SlackPersister
	log       *slog.Logger
}

// NewSlackNotifier returns a configured notifier or nil when the feature is
// disabled. A startup error fires only when notification is enabled but
// dependent fields are missing — silent disable is intentional for
// deployments that just don't want it.
func NewSlackNotifier(cfg config.SlackNotifyConfig, publicURL string, persister SlackPersister, log *slog.Logger) (*SlackNotifier, error) {
	if !cfg.Enabled() {
		log.Info("slack notifications: disabled (DBB_SLACK_NOTIFY_BOT_TOKEN unset)")

		return nil, nil
	}

	if cfg.Channel == "" {
		return nil, errors.New("DBB_SLACK_NOTIFY_BOT_TOKEN set without DBB_SLACK_NOTIFY_CHANNEL")
	}

	if publicURL == "" {
		return nil, errors.New("DBB_SLACK_NOTIFY_BOT_TOKEN set without DBB_PUBLIC_URL")
	}

	return &SlackNotifier{
		client:    slack.New(cfg.BotToken),
		channel:   cfg.Channel,
		publicURL: strings.TrimRight(publicURL, "/"),
		store:     persister,
		log:       log,
	}, nil
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

	switch ev.Action {
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

// buildBlocks renders a Block Kit message for the event. The same shape
// is used for create and update — only the status badge and decider line
// change.
func buildBlocks(ev GrantRequestEvent, publicURL string) []slack.Block {
	header := slack.NewHeaderBlock(slack.NewTextBlockObject(
		"plain_text",
		fmt.Sprintf("%s Grant request — %s", statusEmoji(ev), userLabel(ev.Requester)),
		false,
		false,
	))

	dbName := "—"
	if ev.Database != nil {
		dbName = ev.Database.Name
	}

	defName := "—"

	durationText := ""

	if ev.Definition != nil {
		defName = ev.Definition.Name
		durationText = formatDuration(ev.Definition.DurationSeconds)
	}

	mainText := fmt.Sprintf(
		"*Database*: %s\n*Definition*: %s\n*Duration*: %s\n*Status*: %s",
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

	main := slack.NewSectionBlock(slack.NewTextBlockObject("mrkdwn", mainText, false, false), nil, nil)

	link := fmt.Sprintf("%s/app/grant-requests", publicURL)

	context := slack.NewContextBlock("",
		slack.NewTextBlockObject("mrkdwn", fmt.Sprintf("<%s|Review in dbbat →>", link), false, false),
	)

	return []slack.Block{header, main, context}
}

func userLabel(u *store.User) string {
	if u == nil {
		return "(unknown)"
	}

	return u.Username
}

func statusEmoji(ev GrantRequestEvent) string {
	switch ev.Action {
	case GrantActionCreated:
		return "🔐"
	case GrantActionApproved:
		return "✅"
	case GrantActionDenied:
		return "❌"
	case GrantActionCancelled:
		return "🚫"
	default:
		return "❔"
	}
}

func statusLabel(ev GrantRequestEvent) string {
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
