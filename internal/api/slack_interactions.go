package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/slack-go/slack"

	"github.com/fclairamb/dbbat/internal/notify"
	"github.com/fclairamb/dbbat/internal/store"
)

// slackInteractionMaxBody caps the inbound interaction body. Slack payloads
// are small (a few KB); anything larger is rejected before any parsing to
// avoid unbounded reads from a forged request.
const slackInteractionMaxBody = 1 << 20 // 1 MB

// slackInteractionTimeout bounds the background decision processing. Slack
// requires the HTTP ack within 3s, so the work happens in a goroutine with
// its own context (mirrors notifyAsync).
const slackInteractionTimeout = 10 * time.Second

// Ephemeral message copy shown only to the clicking user.
const (
	msgNotLinked       = "Your Slack account isn't linked to a dbbat user. Sign in to dbbat with Slack first: %s"
	msgNotAdmin        = "Only dbbat admins can decide grant requests."
	msgNoLongerPending = "This request is no longer pending."
	msgRequestNotFound = "That grant request no longer exists."
	msgDefinitionGone  = "The grant definition is no longer active, so this request can't be approved."
	msgDecideFailed    = "Something went wrong deciding this request. Try again from the dbbat UI."
)

// slackDecider is the slice of decision behavior the interaction handler
// needs. It's an interface so the HTTP handler can be unit-tested with a
// stub — *Server is the production implementation.
type slackDecider interface {
	// userBySlackID maps a Slack user ID to a dbbat user, or returns
	// store.ErrUserNotFound / store.ErrIdentityNotFound if unlinked.
	userBySlackID(ctx context.Context, slackID string) (*store.User, error)
	// approve / deny run the shared decide flow (store + audit + notify)
	// with decisionSourceSlack, returning the raw store error unmapped.
	approve(ctx context.Context, uid uuid.UUID, decider *store.User) (*decideOutcome, error)
	deny(ctx context.Context, uid uuid.UUID, decider *store.User) (*decideOutcome, error)
	// rerenderRequest re-posts the current message state for a request so
	// stale buttons vanish after a losing/late click. Best-effort.
	rerenderRequest(ctx context.Context, uid uuid.UUID)
	// postThreadReply posts the decision notification in the message thread.
	postThreadReply(ctx context.Context, ev notify.GrantRequestEvent, action notify.GrantAction)
}

// logSlackInteractivityTransport notes at startup which inbound transport
// carries Slack Approve/Deny clicks. When interactivity is enabled through
// the signing secret alone (no Socket Mode app token), the buttons are
// advertised on notification messages but clicks only work if Slack's
// servers — not just users' browsers — can reach the inbound endpoint. On
// gated/intranet deployments that failure mode is silent: Slack shows
// "Operation timed out" after 3s and dbbat never sees the request. This
// line makes the reachability requirement, and the Socket Mode
// alternative, visible in the startup logs. Socket Mode logs its own
// "slack socket mode enabled" line in startSocketMode.
func (s *Server) logSlackInteractivityTransport() {
	if !s.slackHTTPOnlyInteractivity() {
		return
	}

	s.logger.InfoContext(context.Background(),
		"slack interactivity: inbound HTTP transport only — POST /api/v1/slack/interactions must be reachable"+
			" from Slack's servers (not just users' browsers), or Approve/Deny clicks will time out after 3s;"+
			" on gated/intranet deployments set DBB_SLACK_NOTIFY_APP_TOKEN to receive clicks over Socket Mode instead",
		slog.String("endpoint", s.publicURLForMessage()+"/api/v1/slack/interactions"),
	)
}

// slackHTTPOnlyInteractivity reports whether Approve/Deny interactivity is
// enabled with the signing-secret HTTP endpoint as its only inbound
// transport (no Socket Mode).
func (s *Server) slackHTTPOnlyInteractivity() bool {
	if s.config == nil {
		return false
	}

	cfg := s.config.SlackNotify

	return cfg.Interactive() && !cfg.SocketMode()
}

// handleSlackInteraction handles POST /api/v1/slack/interactions — inbound
// Approve/Deny button clicks from Slack. Registered OUTSIDE the
// authenticated group: the Slack request signature is the authentication.
//
// The handler verifies the signature, parses the callback, acks 200
// immediately (Slack's 3s deadline), and processes the decision in a
// goroutine. All user feedback flows through chat.update, the thread reply,
// or an ephemeral response_url post — never this HTTP response body.
func (s *Server) handleSlackInteraction(c *gin.Context) {
	s.serveSlackInteraction(c, s.notifier.SigningSecret(), s)
}

// serveSlackInteraction is the testable core, parameterized on the signing
// secret and the decider so it can run without a live store or Slack.
func (s *Server) serveSlackInteraction(c *gin.Context, signingSecret string, decider slackDecider) {
	body, ok := s.readVerifiedSlackBody(c, signingSecret)
	if !ok {
		return
	}

	callback, err := parseSlackInteraction(body)
	if err != nil {
		// Signature was valid but the payload is unparseable. 200 so Slack
		// doesn't surface an error to the user; nothing to process.
		s.logger.WarnContext(c.Request.Context(), "slack interaction: unparseable payload", slog.Any("error", err))
		c.Status(http.StatusOK)

		return
	}

	action, requestUID, ok := slackDecisionFromCallback(callback)
	if !ok {
		// Not one of our block_actions (or a non-UUID value) — ignore
		// quietly with a 200.
		c.Status(http.StatusOK)

		return
	}

	// Ack immediately, then process out of band. Slack's 3s deadline is
	// unforgiving and store + Slack round-trips can exceed it.
	c.Status(http.StatusOK)

	responseURL := callback.ResponseURL
	slackUserID := callback.User.ID

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), slackInteractionTimeout)
		defer cancel()

		s.processSlackDecision(ctx, decider, slackUserID, responseURL, action, requestUID)
	}()
}

// readVerifiedSlackBody reads the request body (capped) and verifies the
// Slack signature. On any failure it writes a 401 with no detail and
// returns ok=false. The signing secret being empty is treated as a
// verification failure (defense in depth — the route shouldn't be
// registered at all in that case).
func (s *Server) readVerifiedSlackBody(c *gin.Context, signingSecret string) ([]byte, bool) {
	if signingSecret == "" {
		c.Status(http.StatusUnauthorized)

		return nil, false
	}

	verifier, err := slack.NewSecretsVerifier(c.Request.Header, signingSecret)
	if err != nil {
		// Missing headers or a stale timestamp (>5 min) — reject.
		c.Status(http.StatusUnauthorized)

		return nil, false
	}

	limited := io.LimitReader(c.Request.Body, slackInteractionMaxBody+1)

	body, err := io.ReadAll(limited)
	if err != nil || int64(len(body)) > slackInteractionMaxBody {
		c.Status(http.StatusUnauthorized)

		return nil, false
	}

	if _, err := verifier.Write(body); err != nil {
		c.Status(http.StatusUnauthorized)

		return nil, false
	}

	if err := verifier.Ensure(); err != nil {
		c.Status(http.StatusUnauthorized)

		return nil, false
	}

	return body, true
}

// parseSlackInteraction decodes the form-encoded `payload` field into a
// slack.InteractionCallback.
func parseSlackInteraction(body []byte) (slack.InteractionCallback, error) {
	req, err := http.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	if err != nil {
		return slack.InteractionCallback{}, fmt.Errorf("build parse request: %w", err)
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	return slack.InteractionCallbackParse(req)
}

// slackDecisionFromCallback extracts the (action, request UID) from a
// block_actions callback carrying one of our two action IDs. It returns
// ok=false for any other interaction type, action ID, or a non-UUID value —
// the caller then no-ops with a 200.
func slackDecisionFromCallback(cb slack.InteractionCallback) (notify.GrantAction, uuid.UUID, bool) {
	if cb.Type != slack.InteractionTypeBlockActions {
		return "", uuid.Nil, false
	}

	for _, ba := range cb.ActionCallback.BlockActions {
		if ba == nil {
			continue
		}

		var action notify.GrantAction

		switch ba.ActionID {
		case notify.ActionApprove:
			action = notify.GrantActionApproved
		case notify.ActionDeny:
			action = notify.GrantActionDenied
		default:
			continue
		}

		uid, err := uuid.Parse(ba.Value)
		if err != nil {
			// Trust only IDs from the payload, and only well-formed ones.
			return "", uuid.Nil, false
		}

		return action, uid, true
	}

	return "", uuid.Nil, false
}

// processSlackDecision runs the authorization + decision for a verified
// button click. All outcomes surface to the user via ephemeral messages,
// chat.update, or a thread reply.
func (s *Server) processSlackDecision(
	ctx context.Context,
	decider slackDecider,
	slackUserID, responseURL string,
	action notify.GrantAction,
	requestUID uuid.UUID,
) {
	user, err := decider.userBySlackID(ctx, slackUserID)
	if err != nil {
		if errors.Is(err, store.ErrUserNotFound) || errors.Is(err, store.ErrIdentityNotFound) {
			s.postEphemeral(ctx, responseURL, fmt.Sprintf(msgNotLinked, s.publicURLForMessage()))

			return
		}

		s.logger.WarnContext(ctx, "slack interaction: user lookup failed", slog.Any("error", err))
		s.postEphemeral(ctx, responseURL, msgDecideFailed)

		return
	}

	if !user.IsAdmin() {
		s.postEphemeral(ctx, responseURL, msgNotAdmin)

		return
	}

	var outcome *decideOutcome

	switch action { //nolint:exhaustive // only approve/deny reach here (filtered in slackDecisionFromCallback)
	case notify.GrantActionApproved:
		outcome, err = decider.approve(ctx, requestUID, user)
	case notify.GrantActionDenied:
		outcome, err = decider.deny(ctx, requestUID, user)
	default:
		return
	}

	if err != nil {
		s.handleSlackDecisionError(ctx, decider, responseURL, requestUID, err)

		return
	}

	// Success: chat.update already fired via the shared helper's notify;
	// add the thread reply so watchers get a real notification.
	decider.postThreadReply(ctx, outcome.Event, outcome.Action)
}

// handleSlackDecisionError maps a decision store error to the appropriate
// ephemeral message, re-rendering the message for the already-decided case
// so stale buttons disappear.
func (s *Server) handleSlackDecisionError(
	ctx context.Context,
	decider slackDecider,
	responseURL string,
	requestUID uuid.UUID,
	err error,
) {
	switch {
	case errors.Is(err, store.ErrInvalidTransition):
		// Already decided/canceled/expired (includes the losing side of a
		// two-admin race). Re-render to current state and tell the clicker.
		decider.rerenderRequest(ctx, requestUID)
		s.postEphemeral(ctx, responseURL, msgNoLongerPending)
	case errors.Is(err, store.ErrGrantRequestNotFound):
		s.postEphemeral(ctx, responseURL, msgRequestNotFound)
	case errors.Is(err, store.ErrDefinitionInactive):
		s.postEphemeral(ctx, responseURL, msgDefinitionGone)
	default:
		s.logger.WarnContext(ctx, "slack interaction: decide failed", slog.Any("error", err))
		s.postEphemeral(ctx, responseURL, msgDecideFailed)
	}
}

// postEphemeral posts an ephemeral (only-visible-to-the-clicker) message via
// the interaction's response_url. Best-effort: errors are logged, never
// bubbled. replace_original stays false so the channel message is untouched.
func (s *Server) postEphemeral(ctx context.Context, responseURL, text string) {
	if responseURL == "" {
		return
	}

	if err := slack.PostWebhookContext(ctx, responseURL, &slack.WebhookMessage{
		Text:            text,
		ResponseType:    "ephemeral",
		ReplaceOriginal: false,
	}); err != nil {
		s.logger.WarnContext(ctx, "slack ephemeral post failed", slog.Any("error", err))
	}
}

// publicURLForMessage returns the configured public URL for user-facing
// copy, falling back to a generic hint if unset.
func (s *Server) publicURLForMessage() string {
	if s.config != nil && s.config.PublicURL != "" {
		return s.config.PublicURL
	}

	return "your dbbat instance"
}

// --- slackDecider implementation on *Server ---

func (s *Server) userBySlackID(ctx context.Context, slackID string) (*store.User, error) {
	return s.store.GetUserByIdentity(ctx, store.IdentityTypeSlack, slackID)
}

func (s *Server) approve(ctx context.Context, uid uuid.UUID, decider *store.User) (*decideOutcome, error) {
	return s.approveGrantRequest(ctx, uid, decider, decisionSourceSlack)
}

func (s *Server) deny(ctx context.Context, uid uuid.UUID, decider *store.User) (*decideOutcome, error) {
	// v1 denies with an empty reason; the UI remains the place for nuance.
	return s.denyGrantRequest(ctx, uid, decider, "", decisionSourceSlack)
}

// rerenderRequest fetches the request's current state and re-fires the
// notifier so the message rebuilds without buttons. Used when a click
// arrives for an already-decided request.
func (s *Server) rerenderRequest(ctx context.Context, uid uuid.UUID) {
	req, err := s.store.GetGrantRequest(ctx, uid)
	if err != nil {
		return
	}

	ev := s.loadEventContext(ctx, req, s.deciderUserForRequest(ctx, req))
	ev.Action = actionForStatus(req.Status)
	// notifyAsync intentionally detaches from ctx (own background timeout),
	// so the request context is deliberately not threaded through.
	s.notifyAsync(ev) //nolint:contextcheck // notifyAsync detaches by design
}

// deciderUserForRequest loads the user who decided a request (if any), so a
// re-render can still show the "Approved/Denied by" line.
func (s *Server) deciderUserForRequest(ctx context.Context, req *store.GrantRequest) *store.User {
	if req.DecidedBy == nil {
		return nil
	}

	u, err := s.store.GetUserByUID(ctx, *req.DecidedBy)
	if err != nil {
		return nil
	}

	return u
}

// postThreadReply builds and posts the decision thread reply for a
// successful decision, mentioning the decider when their Slack ID is known.
func (s *Server) postThreadReply(ctx context.Context, ev notify.GrantRequestEvent, action notify.GrantAction) {
	if ev.Request == nil {
		return
	}

	channel := derefStr(ev.Request.SlackChannel)
	ts := derefStr(ev.Request.SlackMessageTS)

	s.notifier.PostThreadReply(ctx, channel, ts, threadReplyText(action, ev.DeciderSlackID, ev.Decider))
}

// threadReplyText renders the thread-reply sentence for a decision.
func threadReplyText(action notify.GrantAction, deciderSlackID string, decider *store.User) string {
	who := "an admin"

	switch {
	case deciderSlackID != "":
		who = "<@" + deciderSlackID + ">"
	case decider != nil:
		who = decider.Username
	}

	switch action { //nolint:exhaustive // GrantActionCreated has no reply text (handled by default)
	case notify.GrantActionApproved:
		return fmt.Sprintf("✅ This request has been *approved* by %s.", who)
	case notify.GrantActionDenied:
		return fmt.Sprintf("❌ This request has been *denied* by %s.", who)
	case notify.GrantActionCancelled:
		return "🚫 This request has been *cancelled* by the requester." //nolint:misspell // matches DB lifecycle
	default:
		return ""
	}
}

// actionForStatus maps a request's terminal status to the notify action
// used to re-render its message.
func actionForStatus(status store.GrantRequestStatus) notify.GrantAction {
	switch status { //nolint:exhaustive // pending/expired fall through to the no-buttons default
	case store.GrantRequestApproved:
		return notify.GrantActionApproved
	case store.GrantRequestDenied:
		return notify.GrantActionDenied
	case store.GrantRequestCancelled:
		return notify.GrantActionCancelled
	default:
		// pending/expired — render without buttons (Interactive only adds
		// buttons on the created action, and this isn't it).
		return notify.GrantActionCreated
	}
}

func derefStr(s *string) string {
	if s == nil {
		return ""
	}

	return *s
}
