package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/fclairamb/dbbat/internal/safe"
)

// GrantBytesSweepInterval is how often every process checks whether a grant's
// cumulative bytes crossed another soft-limit step. A constant, not a setting:
// the signal is "this grant is moving a lot of data", and a minute of latency
// on it changes nothing.
const GrantBytesSweepInterval = time.Minute

// AuditEventGrantBytesThreshold is written once per announced soft-limit
// crossing, so the signal survives a deployment without Slack or a failed post.
const AuditEventGrantBytesThreshold = "grant.bytes_threshold"

// grantBytesNotifyTimeout bounds one outbound notice. Its own budget, detached
// from the sweep: a wedged Slack API leaks this goroutine, never the heartbeat
// loop the sweep runs on.
const grantBytesNotifyTimeout = 15 * time.Second

const goroutineNameGrantBytesNotify = "grant-bytes-notify"

// GrantBytesEvent is one soft-limit crossing, denormalized for the notifier
// the same way TerminationEvent is: every lookup is done here, so the notifier
// never touches the store.
type GrantBytesEvent struct {
	Grant *AccessGrant
	// User is the grant's holder. Nil on a failed lookup: the notice still
	// goes out with a degraded label.
	User *User
	// UserSlackID is the holder's linked Slack id, "" if unlinked.
	UserSlackID string
	// Database is the grant's anchor server. Nil on a failed lookup.
	Database *Server
	// RequestSlackChannel / RequestSlackTS are the coordinates of the grant
	// request's Slack message, empty for a grant assigned directly (no
	// request) or a request that was never posted. The notifier replies in
	// that thread when they are set and posts to its channel otherwise.
	RequestSlackChannel string
	RequestSlackTS      string

	// Step is the effective step in bytes; Steps how many of them the grant
	// has now crossed (and the value claimed). Total is the grant's
	// cumulative bytes.
	Step  int64
	Steps int64
	Total int64
	// Delta is the bytes moved since the previous announced step, and Since
	// when the previous notice went out (the grant's start for the first).
	Delta int64
	Since time.Time
}

// GrantBytesNotifier is the optional collaborator a soft-limit crossing is
// handed to. Nil is a silent no-op, like TerminationNotifier. The store calls
// it from a detached goroutine of its own.
type GrantBytesNotifier interface {
	NotifyGrantBytes(ctx context.Context, ev GrantBytesEvent)
}

// grantBytesSettings is what SetGrantBytesNotify installs. Held behind an
// atomic pointer because the heartbeat goroutine that runs the sweep starts
// before the API server, which owns the Slack client, exists.
type grantBytesSettings struct {
	defaultStep int64
	notifier    GrantBytesNotifier
}

// SetGrantBytesNotify installs the instance-wide step (DBB_SLACK_NOTIFY_BYTES_STEP,
// 0 = off) and the notifier crossings are handed to. A nil notifier still
// leaves the sweep claiming steps and writing audit entries for definitions
// that set notify_bytes_step.
func (s *Store) SetGrantBytesNotify(defaultStep int64, notifier GrantBytesNotifier) {
	s.grantBytes.Store(&grantBytesSettings{defaultStep: defaultStep, notifier: notifier})
}

// EffectiveNotifyBytesStep resolves a grant's soft-limit step: the
// definition's notify_bytes_step when set (0 included, which turns it off),
// otherwise the instance default.
func (g *AccessGrant) EffectiveNotifyBytesStep(defaultStep int64) int64 {
	if g == nil || g.Definition == nil || g.Definition.NotifyBytesStep == nil {
		return defaultStep
	}

	return *g.Definition.NotifyBytesStep
}

// SweepGrantBytes announces every active grant whose cumulative bytes crossed
// another multiple of its step since the last announcement.
//
// Only grants with connection activity at or after since are considered, so an
// idle fleet costs one query. For each, the bytes are the same aggregate the
// quota reads (grantBytesTransferred), steps = bytes / step, and the crossing
// is claimed with ClaimGrantBytesSteps. Only the claim's winner writes the
// audit entry and hands the event to the notifier, which is the whole
// cross-replica dedupe. Several steps crossed between two sweeps make one
// event naming the current total.
//
// Returns the events this call claimed. A failure on one grant is logged and
// skipped: the next sweep tries again.
func (s *Store) SweepGrantBytes(ctx context.Context, since time.Time) ([]GrantBytesEvent, error) {
	settings := s.grantBytes.Load()
	if settings == nil {
		settings = &grantBytesSettings{}
	}

	var grants []*AccessGrant

	err := s.db.NewSelect().
		Model(&grants).
		Where("revoked_at IS NULL").
		Where("starts_at <= NOW()").
		Where("expires_at > NOW()").
		Where("uid IN (SELECT grant_uid FROM connections WHERE grant_uid IS NOT NULL AND last_activity_at >= ?)", since).
		Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list active grants for the byte soft limit: %w", err)
	}

	if err := s.attachDefinitions(ctx, grants); err != nil {
		return nil, err
	}

	var events []GrantBytesEvent

	for _, g := range grants {
		ev, claimed, err := s.sweepGrantBytesOne(ctx, g, settings.defaultStep)
		if err != nil {
			slog.WarnContext(ctx, "byte soft limit: failed to check a grant",
				slog.String("grant", g.UID.String()), slog.Any("error", err))

			continue
		}

		if !claimed {
			continue
		}

		events = append(events, ev)
		s.recordGrantBytesThreshold(ctx, ev)
		s.notifyGrantBytes(ctx, settings.notifier, ev)
	}

	return events, nil
}

func (s *Store) sweepGrantBytesOne(ctx context.Context, g *AccessGrant, defaultStep int64) (GrantBytesEvent, bool, error) {
	step := g.EffectiveNotifyBytesStep(defaultStep)
	if step <= 0 {
		return GrantBytesEvent{}, false, nil
	}

	total, err := s.grantBytesTransferred(ctx, g)
	if err != nil {
		return GrantBytesEvent{}, false, err
	}

	steps := total / step
	if steps <= g.BytesNotifiedSteps {
		return GrantBytesEvent{}, false, nil
	}

	claimed, err := s.ClaimGrantBytesSteps(ctx, g.UID, steps)
	if err != nil || !claimed {
		return GrantBytesEvent{}, false, err
	}

	since := g.StartsAt
	if g.BytesNotifiedAt != nil {
		since = *g.BytesNotifiedAt
	}

	g.BytesTransferred = total

	return s.buildGrantBytesEvent(ctx, GrantBytesEvent{
		Grant: g,
		Step:  step,
		Steps: steps,
		Total: total,
		Delta: total - g.BytesNotifiedSteps*step,
		Since: since,
	}), true, nil
}

// ClaimGrantBytesSteps records that steps soft-limit steps have been announced
// for a grant, if no one recorded as many already. Reports whether this call
// moved the counter: only the caller that did may announce. Concurrent claims
// on the same step therefore have exactly one winner, across replicas.
func (s *Store) ClaimGrantBytesSteps(ctx context.Context, grantUID uuid.UUID, steps int64) (bool, error) {
	res, err := s.db.NewUpdate().
		Model((*AccessGrant)(nil)).
		Set("bytes_notified_steps = ?", steps).
		Set("bytes_notified_at = NOW()").
		Where("uid = ?", grantUID).
		Where("bytes_notified_steps < ?", steps).
		Exec(ctx)
	if err != nil {
		return false, fmt.Errorf("failed to claim a byte soft-limit step: %w", err)
	}

	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("failed to claim a byte soft-limit step: %w", err)
	}

	return affected > 0, nil
}

// buildGrantBytesEvent resolves who and where. Every lookup is best-effort: a
// failed one leaves its field empty rather than dropping the notice.
func (s *Store) buildGrantBytesEvent(ctx context.Context, ev GrantBytesEvent) GrantBytesEvent {
	g := ev.Grant

	if user, err := s.GetUserByUID(ctx, g.UserID); err != nil {
		slog.WarnContext(ctx, "byte soft limit: failed to load the user",
			slog.String("grant", g.UID.String()), slog.Any("error", err))
	} else {
		ev.User = user
		ev.UserSlackID = s.slackIDForUser(ctx, user.UID)
	}

	if db, err := s.GetServerByUID(ctx, g.DatabaseID); err != nil {
		slog.WarnContext(ctx, "byte soft limit: failed to load the database",
			slog.String("grant", g.UID.String()), slog.Any("error", err))
	} else {
		ev.Database = db
	}

	req, err := s.GetGrantRequestByResultingGrant(ctx, g.UID)

	switch {
	case err == nil:
		ev.RequestSlackChannel = derefStr(req.SlackChannel)
		ev.RequestSlackTS = derefStr(req.SlackMessageTS)
	case !errors.Is(err, ErrGrantRequestNotFound):
		slog.WarnContext(ctx, "byte soft limit: failed to load the grant request",
			slog.String("grant", g.UID.String()), slog.Any("error", err))
	}

	return ev
}

func derefStr(s *string) string {
	if s == nil {
		return ""
	}

	return *s
}

// grantBytesAuditDetails is the payload of a grant.bytes_threshold entry.
type grantBytesAuditDetails struct {
	GrantUID    uuid.UUID `json:"grant_uid"`
	DatabaseUID uuid.UUID `json:"database_uid"`
	TotalBytes  int64     `json:"total_bytes"`
	StepBytes   int64     `json:"step_bytes"`
	Steps       int64     `json:"steps"`
	DeltaBytes  int64     `json:"delta_bytes"`
	Since       time.Time `json:"since"`
}

// recordGrantBytesThreshold writes the audit entry for a claimed crossing.
// Best effort: the claim already committed, and a lost entry must not stop the
// notice.
func (s *Store) recordGrantBytesThreshold(ctx context.Context, ev GrantBytesEvent) {
	details, err := json.Marshal(grantBytesAuditDetails{
		GrantUID:    ev.Grant.UID,
		DatabaseUID: ev.Grant.DatabaseID,
		TotalBytes:  ev.Total,
		StepBytes:   ev.Step,
		Steps:       ev.Steps,
		DeltaBytes:  ev.Delta,
		Since:       ev.Since,
	})
	if err != nil {
		slog.ErrorContext(ctx, "byte soft limit: failed to encode the audit entry", slog.Any("error", err))

		return
	}

	userID := ev.Grant.UserID

	if err := s.LogAuditEvent(ctx, &AuditEvent{
		EventType: AuditEventGrantBytesThreshold,
		UserID:    &userID,
		Details:   details,
	}); err != nil {
		slog.WarnContext(ctx, "byte soft limit: failed to write the audit entry",
			slog.String("grant", ev.Grant.UID.String()), slog.Any("error", err))
	}
}

// notifyGrantBytes hands the event to the notifier on a detached, bounded
// goroutine, so a slow Slack API never holds up the heartbeat loop.
func (s *Store) notifyGrantBytes(ctx context.Context, notifier GrantBytesNotifier, ev GrantBytesEvent) {
	if notifier == nil {
		return
	}

	notifyCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), grantBytesNotifyTimeout)

	go safe.RunGuarded(notifyCtx, nil, goroutineNameGrantBytesNotify, func() {
		defer cancel()

		notifier.NotifyGrantBytes(notifyCtx, ev)
	})
}
