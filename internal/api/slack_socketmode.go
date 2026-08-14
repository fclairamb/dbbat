package api

import (
	"context"
	"errors"
	"log/slog"

	"github.com/fclairamb/dbbat/internal/safe"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"
)

// What a panic on the Socket Mode transport is logged under.
const (
	goroutineNameSlackSocketRun   = "slack socket mode transport"
	goroutineNameSlackSocketEvent = "slack socket mode event"
)

// startSocketMode opens an outbound Slack Socket Mode connection when an
// app-level token is configured, receiving Approve/Deny interactions over the
// socket instead of the inbound HTTP endpoint. This is the transport for
// deployments that can't accept inbound Slack traffic (intranet /
// IP-allowlisted ingress): dbbat dials out, so no signing secret or public
// reachability is needed — the connection is authenticated by the app-level
// token. No-op unless both an app-level token and a bot token are configured.
func (s *Server) startSocketMode() {
	if s.config == nil || !s.config.SlackNotify.SocketMode() {
		return
	}

	api := slack.New(
		s.config.SlackNotify.BotToken,
		slack.OptionAppLevelToken(s.config.SlackNotify.AppToken),
	)
	client := socketmode.New(api)

	ctx, cancel := context.WithCancel(context.Background())
	s.socketCancel = cancel

	go s.runSocketMode(ctx, client)

	// Whole-loop guard: RunContext is third-party and already allowed to stop on
	// its own error (the warn below), so a recovered panic lands on a state the
	// transport is designed for — and one that is not the process dying with
	// every live proxy session on it.
	go safe.RunGuarded(ctx, s.logger, goroutineNameSlackSocketRun, func() {
		// RunContext blocks and reconnects internally until ctx is canceled.
		if err := client.RunContext(ctx); err != nil && !errors.Is(err, context.Canceled) {
			s.logger.WarnContext(ctx, "slack socket mode stopped", slog.Any("error", err))
		}
	})

	s.logger.InfoContext(context.Background(), "slack socket mode enabled")
}

// runSocketMode consumes Socket Mode events until the context is canceled or
// the event channel closes.
func (s *Server) runSocketMode(ctx context.Context, client *socketmode.Client) {
	for {
		select {
		case <-ctx.Done():
			return
		case evt, ok := <-client.Events:
			if !ok {
				return
			}

			// Per event rather than around the loop: the payload is
			// Slack-supplied and decoded by a third-party library, so this is
			// the one turn worth losing. Retiring the whole receiver would
			// leave the socket connected and every later Approve/Deny click
			// silently ignored.
			safe.RunMaintenance(ctx, s.logger, goroutineNameSlackSocketEvent, func() {
				s.handleSocketEvent(ctx, client, evt)
			})
		}
	}
}

// handleSocketEvent acks an interaction envelope immediately (Slack's 3s
// deadline) and dispatches Approve/Deny clicks into the same decision pipeline
// as the inbound HTTP endpoint. Non-interaction events are ignored.
func (s *Server) handleSocketEvent(ctx context.Context, client *socketmode.Client, evt socketmode.Event) {
	callback, ok := socketInteractionCallback(evt)
	if !ok {
		return
	}

	// Ack the envelope right away so Slack doesn't retry; all user feedback
	// flows through chat.update, the thread reply, or an ephemeral post.
	if evt.Request != nil {
		if err := client.Ack(*evt.Request); err != nil {
			s.logger.WarnContext(ctx, "slack socket ack failed", slog.Any("error", err))
		}
	}

	// Decision processing detaches from the socket lifecycle (own timeout) so
	// an in-flight decision completes even during shutdown, mirroring the HTTP
	// path.
	s.dispatchSlackCallback(s, callback) //nolint:contextcheck // detaches by design
}

// socketInteractionCallback returns the interaction callback carried by a
// Socket Mode event, or ok=false for any non-interaction event or unexpected
// payload.
func socketInteractionCallback(evt socketmode.Event) (slack.InteractionCallback, bool) {
	if evt.Type != socketmode.EventTypeInteractive {
		return slack.InteractionCallback{}, false
	}

	callback, ok := evt.Data.(slack.InteractionCallback)

	return callback, ok
}

// dispatchSlackCallback runs the decision pipeline for an already-parsed
// interaction callback (Socket Mode delivers callbacks directly, without the
// HTTP form/signature step). It returns false when the callback isn't one of
// our Approve/Deny block actions. Processing happens in a goroutine with its
// own timeout, mirroring the HTTP path.
func (s *Server) dispatchSlackCallback(decider slackDecider, callback slack.InteractionCallback) bool {
	if s.dispatchSlackQueryDecision(callback) {
		return true
	}

	action, requestUID, ok := slackDecisionFromCallback(callback)
	if !ok {
		return false
	}

	slackUserID := callback.User.ID
	responseURL := callback.ResponseURL

	go safe.RunGuarded(context.Background(), s.logger, goroutineNameSlackGrantDecision, func() {
		ctx, cancel := context.WithTimeout(context.Background(), slackInteractionTimeout)
		defer cancel()

		s.processSlackDecision(ctx, decider, slackUserID, responseURL, action, requestUID)
	})

	return true
}
