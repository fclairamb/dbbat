package notify

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/slack-go/slack"

	"github.com/fclairamb/dbbat/internal/store"
)

// NotifyGrantBytes announces a grant's byte soft-limit crossing. It replies in
// the grant request's Slack thread when the request was posted, so whoever
// approved or watched it is notified, and posts to the configured channel
// otherwise (a grant assigned directly has no request and no thread).
//
// Best-effort like every other notice: errors are logged and swallowed. The
// store already calls this from a detached, bounded goroutine.
func (n *SlackNotifier) NotifyGrantBytes(ctx context.Context, ev store.GrantBytesEvent) {
	if n == nil || ev.Grant == nil {
		return
	}

	text := grantBytesText(ev, n.publicURL, time.Now())

	if ev.RequestSlackChannel != "" && ev.RequestSlackTS != "" {
		n.PostThreadReply(ctx, ev.RequestSlackChannel, ev.RequestSlackTS, text)

		return
	}

	if _, _, err := n.client.PostMessageContext(ctx, n.channel, slack.MsgOptionText(text, false)); err != nil {
		n.log.WarnContext(ctx, "slack grant bytes post failed", slog.Any("error", err))
	}
}

// grantBytesText renders the one-message notice, e.g. "Grant `…` for @user on
// `db` has transferred 3.0 GiB so far (last 1.0 GiB in 12 min). The traffic is
// not blocked."
func grantBytesText(ev store.GrantBytesEvent, publicURL string, now time.Time) string {
	user := "(unknown)"
	if ev.User != nil {
		user = slackMention(ev.UserSlackID, ev.User.Username)
	}

	database := "(unknown)"
	if ev.Database != nil {
		database = ev.Database.Name
	}

	text := fmt.Sprintf(
		":package: Grant `%s` for %s on `%s` has transferred %s so far (last %s in %s). The traffic is not blocked.",
		ev.Grant.UID, user, database,
		formatBytesHuman(ev.Total), formatBytesHuman(ev.Delta), formatSince(ev.Since, now),
	)

	if publicURL != "" {
		text += fmt.Sprintf(" <%s/app/grants?uid=%s|Open grant →>", publicURL, ev.Grant.UID)
	}

	return text
}

// formatBytesHuman renders a byte count in binary units with one decimal:
// 1.0 GiB, 512.0 MiB, 900 B.
func formatBytesHuman(n int64) string {
	const unit = 1024

	if n < unit {
		return fmt.Sprintf("%d B", n)
	}

	units := []string{"KiB", "MiB", "GiB", "TiB", "PiB", "EiB"}
	value := float64(n) / unit
	i := 0

	for value >= unit && i < len(units)-1 {
		value /= unit
		i++
	}

	return fmt.Sprintf("%.1f %s", value, units[i])
}

// formatSince renders how long ago since was, in minutes up to two hours and
// in hours past that.
func formatSince(since, now time.Time) string {
	elapsed := now.Sub(since)

	switch {
	case elapsed < time.Minute:
		return "under a minute"
	case elapsed < 2*time.Hour:
		return fmt.Sprintf("%d min", int(elapsed/time.Minute))
	default:
		return fmt.Sprintf("%.1f h", elapsed.Hours())
	}
}
