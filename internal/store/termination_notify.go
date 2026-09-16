package store

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/fclairamb/dbbat/internal/safe"
)

// terminationNotifyTimeout bounds the detached lookups plus the outbound
// notification. Its own budget, not the caller's: the session teardown this
// fires from must never wait on it (see notifyTermination), so a wedged Slack
// API or a slow lookup can only ever leak this one goroutine, never the
// caller.
const terminationNotifyTimeout = 15 * time.Second

const goroutineNameTerminationNotify = "termination-notify"

// terminationNotifyReasons is the closed set of Termination.Reason values
// worth a human's attention on Slack. The rest already went through a human
// (grant_revoked), are routine (grant_expired) or belong to infrastructure
// alerting elsewhere (instance_lost) — see docs/approvals.md's sibling doc on
// terminations for the reasoning.
var terminationNotifyReasons = map[string]bool{
	TerminationStatementTimeout: true,
	TerminationAdminTerminated:  true,
	TerminationQuotaExceeded:    true,
}

// notifiableTermination reports whether a termination reason is worth posting
// to Slack.
func notifiableTermination(reason string) bool {
	return terminationNotifyReasons[reason]
}

// TerminationEvent carries what a notifier needs to render a message about a
// session dbbat ended on its own. Denormalized — Connection/User/Database/
// Grant are resolved once here rather than handed to the notifier as bare
// uids — so the notifier stays free of store lookups, matching
// notify.GrantRequestEvent's contract.
type TerminationEvent struct {
	// Connection is the row as it stood right after the close committed.
	Connection *Connection
	// User is the session's owner. Nil only if the lookup failed — the event
	// still fires, since "dbbat ended a session" is worth saying even with a
	// degraded message.
	User *User
	// UserSlackID is the user's linked Slack id, "" if unlinked or unknown.
	UserSlackID string
	// Database is the target server. Nil on a failed lookup, same rule as
	// User.
	Database *Server
	// Grant is the grant the session authenticated under, nil for an unbound
	// session or a failed lookup.
	Grant *AccessGrant

	// Reason is one of the Termination* vocabulary constants.
	Reason string
	// TerminatedBy is the human who asked for this, empty for a
	// watchdog-driven reason. Mirrors Termination.By.
	TerminatedBy string
	// Detail is the terminating admin's free text, empty when they gave none
	// or the reason isn't admin_terminated. Mirrors Termination.Detail.
	Detail string

	// QueryHead is the SQL text of the statement in flight when dbbat acted,
	// empty when none was captured. The notifier decides whether to render it
	// and how much of it — see notify.SlackNotifier.NotifyTermination.
	QueryHead string

	// Limit and Ran are the statement-timeout duration pair: the grant's
	// per-statement limit and how long the statement had actually run. Both
	// zero outside a statement_timeout termination.
	Limit time.Duration
	Ran   time.Duration
}

// TerminationNotifier is the optional collaborator Store.closeConnection
// fires after recording a termination, mirroring
// shared.ApprovalEscalator's nil-is-off contract: a nil notifier (the zero
// value of the field, never explicitly wired) is never called.
//
// Implementations must not block the caller — Store fires this from its own
// detached goroutine already (see notifyTermination), so a slow or blocking
// NotifyTermination only ever delays itself, never a live session's teardown.
type TerminationNotifier interface {
	NotifyTermination(ctx context.Context, ev TerminationEvent)
}

// notifyTermination posts a best-effort, fire-and-forget notification for a
// termination worth a human's attention. It is called from closeConnection
// after the row is committed and the audit trail is written, and must never
// delay that return: the DB lookups it does to enrich the event, and the
// notifier call itself, run on a goroutine of their own, detached from the
// caller's (possibly already-canceled) context and bounded by their own
// timeout.
func (s *Store) notifyTermination(ctx context.Context, conn *Connection, t Termination) {
	if s.terminationNotifier == nil || !t.Set() || !notifiableTermination(t.Reason) {
		return
	}

	notifyCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), terminationNotifyTimeout)

	go safe.RunGuarded(notifyCtx, nil, goroutineNameTerminationNotify, func() {
		defer cancel()

		s.terminationNotifier.NotifyTermination(notifyCtx, s.buildTerminationEvent(notifyCtx, conn, t))
	})
}

// buildTerminationEvent resolves the identity the notifier needs to render a
// message. Every lookup is best-effort: a failed one is logged and leaves its
// field nil/empty rather than dropping the whole notification — dbbat ending
// a session is worth saying even with a degraded message.
func (s *Store) buildTerminationEvent(ctx context.Context, conn *Connection, t Termination) TerminationEvent {
	ev := TerminationEvent{
		Connection:   conn,
		Reason:       t.Reason,
		TerminatedBy: t.By,
		Detail:       t.Detail,
		Limit:        t.Limit,
		Ran:          t.Observed,
	}

	if user, err := s.GetUserByUID(ctx, conn.UserID); err != nil {
		slog.WarnContext(ctx, "termination notify: failed to load the user",
			slog.String("connection", conn.UID.String()), slog.Any("error", err))
	} else {
		ev.User = user
		ev.UserSlackID = s.slackIDForUser(ctx, user.UID)
	}

	if db, err := s.GetServerByUID(ctx, conn.DatabaseID); err != nil {
		slog.WarnContext(ctx, "termination notify: failed to load the database",
			slog.String("connection", conn.UID.String()), slog.Any("error", err))
	} else {
		ev.Database = db
	}

	if conn.GrantUID != nil {
		if grant, err := s.GetGrantByUID(ctx, *conn.GrantUID); err != nil {
			slog.WarnContext(ctx, "termination notify: failed to load the grant",
				slog.String("connection", conn.UID.String()), slog.Any("error", err))
		} else {
			ev.Grant = grant
		}
	}

	if t.QueryUID != uuid.Nil {
		if q, err := s.GetQuery(ctx, t.QueryUID); err != nil {
			slog.WarnContext(ctx, "termination notify: failed to load the statement",
				slog.String("connection", conn.UID.String()), slog.Any("error", err))
		} else {
			ev.QueryHead = q.SQLText
		}
	}

	return ev
}

// slackIDForUser resolves a user's linked Slack id, "" if unlinked or on a
// lookup failure. Mirrors api.Server.slackIDForUser (which lives too far
// downstream of store to share code with — it drives the grant-request
// notifier, this the termination one).
func (s *Store) slackIDForUser(ctx context.Context, userID uuid.UUID) string {
	identities, err := s.GetUserIdentities(ctx, userID)
	if err != nil {
		return ""
	}

	for i := range identities {
		if identities[i].Provider == IdentityTypeSlack {
			return identities[i].ProviderID
		}
	}

	return ""
}
