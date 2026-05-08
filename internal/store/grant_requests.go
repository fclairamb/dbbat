package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"
)

// ErrGrantRequestNotFound is returned when a request UID misses.
var ErrGrantRequestNotFound = errors.New("grant request not found")

// ErrInvalidTransition is returned when a state transition is rejected
// because the request is not in `pending`.
var ErrInvalidTransition = errors.New("grant request not pending")

// ErrDefinitionInactive is returned by ApproveGrantRequest if the
// referenced definition has been deactivated between request and approval.
var ErrDefinitionInactive = errors.New("grant definition is no longer active")

// CreateGrantRequest inserts a new pending request.
func (s *Store) CreateGrantRequest(ctx context.Context, req *GrantRequest) (*GrantRequest, error) {
	result := &GrantRequest{
		UserID:            req.UserID,
		GrantDefinitionID: req.GrantDefinitionID,
		DatabaseID:        req.DatabaseID,
		Justification:     req.Justification,
		Status:            GrantRequestPending,
		RequestedAt:       time.Now(),
	}

	_, err := s.db.NewInsert().
		Model(result).
		Returning("*").
		Exec(ctx)
	if err != nil {
		return nil, fmt.Errorf("create grant request: %w", err)
	}

	return result, nil
}

// GetGrantRequest fetches a request by UID.
func (s *Store) GetGrantRequest(ctx context.Context, uid uuid.UUID) (*GrantRequest, error) {
	req := new(GrantRequest)

	err := s.db.NewSelect().
		Model(req).
		Where("uid = ?", uid).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrGrantRequestNotFound
		}

		return nil, fmt.Errorf("get grant request: %w", err)
	}

	return req, nil
}

// ListGrantRequests returns requests matching the filter, newest first.
func (s *Store) ListGrantRequests(ctx context.Context, filter GrantRequestFilter) ([]GrantRequest, error) {
	var requests []GrantRequest

	q := s.db.NewSelect().Model(&requests)

	if filter.UserID != nil {
		q = q.Where("user_id = ?", *filter.UserID)
	}

	if filter.DatabaseID != nil {
		q = q.Where("database_id = ?", *filter.DatabaseID)
	}

	if filter.Status != nil {
		q = q.Where("status = ?", *filter.Status)
	}

	q = q.Order("requested_at DESC")

	if filter.Limit > 0 {
		q = q.Limit(filter.Limit)
	}

	if filter.Offset > 0 {
		q = q.Offset(filter.Offset)
	}

	if err := q.Scan(ctx); err != nil {
		return nil, fmt.Errorf("list grant requests: %w", err)
	}

	return requests, nil
}

// HasPendingRequest checks whether a user already has an open request for
// the same database+definition. Used by the API to short-circuit duplicates
// before they get persisted.
func (s *Store) HasPendingRequest(ctx context.Context, userID, definitionID, databaseID uuid.UUID) (bool, error) {
	count, err := s.db.NewSelect().
		Model((*GrantRequest)(nil)).
		Where("user_id = ?", userID).
		Where("grant_definition_id = ?", definitionID).
		Where("database_id = ?", databaseID).
		Where("status = ?", GrantRequestPending).
		Count(ctx)
	if err != nil {
		return false, fmt.Errorf("check pending request: %w", err)
	}

	return count > 0, nil
}

// ApproveGrantRequest atomically transitions a pending request to approved
// and creates the resulting AccessGrant from the linked definition. The
// caller (admin) is captured in decided_by + grant.granted_by.
//
// Returns:
//   - the resulting grant, the updated request, nil on success
//   - ErrGrantRequestNotFound if the request doesn't exist
//   - ErrInvalidTransition if the request isn't pending
//   - ErrDefinitionInactive if the linked definition was deactivated
//
// Wrapped in a transaction so a partial failure (request flipped, grant
// not created) can't leak.
func (s *Store) ApproveGrantRequest(ctx context.Context, uid, decidedBy uuid.UUID) (*Grant, *GrantRequest, error) {
	var (
		grant   *Grant
		request *GrantRequest
	)

	err := s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		req := new(GrantRequest)
		if err := tx.NewSelect().Model(req).Where("uid = ?", uid).For("UPDATE").Scan(ctx); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrGrantRequestNotFound
			}

			return fmt.Errorf("select request: %w", err)
		}

		if req.Status != GrantRequestPending {
			return ErrInvalidTransition
		}

		def := new(GrantDefinition)
		if err := tx.NewSelect().Model(def).Where("uid = ?", req.GrantDefinitionID).For("UPDATE").Scan(ctx); err != nil {
			return fmt.Errorf("select definition: %w", err)
		}

		if !def.IsActive {
			return ErrDefinitionInactive
		}

		newGrant := BuildGrantFromDefinition(def, req.UserID, req.DatabaseID, decidedBy, time.Now())

		if _, err := tx.NewInsert().Model(newGrant).Returning("*").Exec(ctx); err != nil {
			return fmt.Errorf("create grant: %w", err)
		}

		now := time.Now()
		req.Status = GrantRequestApproved
		req.DecidedAt = &now
		req.DecidedBy = &decidedBy
		req.ResultingGrantID = &newGrant.UID

		if _, err := tx.NewUpdate().Model(req).
			Column("status", "decided_at", "decided_by", "resulting_grant_id").
			Where("uid = ?", uid).
			Exec(ctx); err != nil {
			return fmt.Errorf("update request: %w", err)
		}

		grant = newGrant
		request = req

		return nil
	})

	if err != nil {
		return nil, nil, err
	}

	return grant, request, nil
}

// DenyGrantRequest atomically transitions pending → denied with an
// optional reason.
func (s *Store) DenyGrantRequest(ctx context.Context, uid, decidedBy uuid.UUID, reason string) (*GrantRequest, error) {
	return s.transition(ctx, uid, GrantRequestDenied, &decidedBy, reason)
}

// CancelGrantRequest atomically transitions pending → cancelled. Used by
// the requester themselves; the caller layer enforces who can cancel
// whose request.
func (s *Store) CancelGrantRequest(ctx context.Context, uid, byUser uuid.UUID) (*GrantRequest, error) {
	return s.transition(ctx, uid, GrantRequestCancelled, &byUser, "")
}

// SetGrantRequestSlackMessage records the channel/ts of the Slack post
// that announced this request, so the notifier can chat.update on
// status changes (Spec 04). NULL on either column means "no Slack post
// for this request" (notifier disabled, or first post failed).
func (s *Store) SetGrantRequestSlackMessage(ctx context.Context, uid uuid.UUID, channel, ts string) error {
	_, err := s.db.NewUpdate().
		Model((*GrantRequest)(nil)).
		Set("slack_channel = ?", channel).
		Set("slack_message_ts = ?", ts).
		Where("uid = ?", uid).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("set slack message: %w", err)
	}

	return nil
}

// transition is the shared impl for deny/cancel — both flip pending →
// terminal-state and capture a decider + reason.
func (s *Store) transition(
	ctx context.Context,
	uid uuid.UUID,
	target GrantRequestStatus,
	decidedBy *uuid.UUID,
	reason string,
) (*GrantRequest, error) {
	var updated *GrantRequest

	err := s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		req := new(GrantRequest)
		if err := tx.NewSelect().Model(req).Where("uid = ?", uid).For("UPDATE").Scan(ctx); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrGrantRequestNotFound
			}

			return fmt.Errorf("select request: %w", err)
		}

		if req.Status != GrantRequestPending {
			return ErrInvalidTransition
		}

		now := time.Now()
		req.Status = target
		req.DecidedAt = &now
		req.DecidedBy = decidedBy

		if reason != "" {
			req.DecisionReason = &reason
		}

		if _, err := tx.NewUpdate().Model(req).
			Column("status", "decided_at", "decided_by", "decision_reason").
			Where("uid = ?", uid).
			Exec(ctx); err != nil {
			return fmt.Errorf("update request: %w", err)
		}

		updated = req

		return nil
	})

	if err != nil {
		return nil, err
	}

	return updated, nil
}
