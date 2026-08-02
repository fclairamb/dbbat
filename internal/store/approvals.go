package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/google/uuid"
)

// Approval hold states. These match the queries_approval_status_check CHECK
// constraint exactly. Note the absence of a "timeout" state: by design a hold
// has no clock of its own — it ends on approve, deny, or client disconnect.
const (
	// ApprovalPending marks a query that is parked mid-flight waiting for a
	// human decision. Nothing has been forwarded upstream yet.
	ApprovalPending = "pending"
	// ApprovalApproved marks a hold a second human released; the statement
	// was then forwarded upstream.
	ApprovalApproved = "approved"
	// ApprovalDenied marks a hold a second human rejected; the statement was
	// never forwarded and the client got a protocol-native error.
	ApprovalDenied = "denied"
	// ApprovalAbandoned marks a hold whose client gave up (disconnect, cancel,
	// grant expiry, shutdown) before anyone decided. Nothing was forwarded.
	// Rendered distinctly from denied everywhere — the workflow looks broken
	// otherwise to whoever finally clicks Approve.
	ApprovalAbandoned = "abandoned"
)

// MaxApprovalPatterns and MaxApprovalPatternLength bound what an admin may
// store on a definition. RE2 has no catastrophic backtracking, but an
// unbounded pattern list is still a per-statement cost on the proxy hot path.
const (
	MaxApprovalPatterns      = 32
	MaxApprovalPatternLength = 512
)

// Approval validation errors, surfaced as 400s by the API layer.
var (
	// ErrTooManyApprovalPatterns is returned when a definition carries more
	// than MaxApprovalPatterns patterns.
	ErrTooManyApprovalPatterns = errors.New("too many approval patterns")
	// ErrApprovalPatternTooLong is returned for a pattern over
	// MaxApprovalPatternLength characters.
	ErrApprovalPatternTooLong = errors.New("approval pattern too long")
	// ErrApprovalPatternEmpty is returned for a blank pattern, which would
	// match every statement — almost certainly a mistake, never a policy.
	ErrApprovalPatternEmpty = errors.New("approval pattern must not be empty")
	// ErrQueryNotPending is returned when resolving a query that is not (or
	// is no longer) in the pending state.
	ErrQueryNotPending = errors.New("query is not pending approval")
)

// ValidateApprovalPatterns compiles every pattern so a bad regexp is rejected
// at definition-save time rather than blowing up on the proxy hot path. The
// compiled forms are discarded — the proxy compiles its own copy once per
// session — this is purely a gate.
func ValidateApprovalPatterns(patterns []string) error {
	if len(patterns) > MaxApprovalPatterns {
		return fmt.Errorf("%w: %d > %d", ErrTooManyApprovalPatterns, len(patterns), MaxApprovalPatterns)
	}

	for _, p := range patterns {
		if p == "" {
			return ErrApprovalPatternEmpty
		}

		if len(p) > MaxApprovalPatternLength {
			return fmt.Errorf("%w: %d > %d", ErrApprovalPatternTooLong, len(p), MaxApprovalPatternLength)
		}

		if _, err := regexp.Compile(p); err != nil {
			return fmt.Errorf("invalid approval pattern %q: %w", p, err)
		}
	}

	return nil
}

// RequiresApproval reports whether the grant carries any approval pattern at
// all. Cheap pre-check so the common case (no patterns) never compiles or
// matches anything.
func (g *AccessGrant) RequiresApproval() bool {
	return g != nil && len(g.ApprovalPatterns) > 0
}

// MayApprove reports whether a user in the given groups may resolve holds on
// this grant. Admin-ness is checked by the caller (any admin may approve any
// hold); this covers the definition-scoped approver groups only.
func (g *AccessGrant) MayApprove(userGroupUIDs []uuid.UUID) bool {
	if g == nil {
		return false
	}

	for _, scoped := range g.ApproverGroupUIDs {
		for _, owned := range userGroupUIDs {
			if scoped == owned {
				return true
			}
		}
	}

	return false
}

// CreatePendingQuery inserts a query row already marked pending, so the held
// statement is visible in /queries and addressable by UID *while* it hangs.
// The regular async-persist-on-completion path cannot provide that.
func (s *Store) CreatePendingQuery(ctx context.Context, query *Query, pattern string) (*Query, error) {
	status := ApprovalPending

	result := &Query{
		UID:             newUIDv7(),
		ConnectionID:    query.ConnectionID,
		SQLText:         query.SQLText,
		Parameters:      query.Parameters,
		ExecutedAt:      query.ExecutedAt,
		ApprovalStatus:  &status,
		ApprovalPattern: &pattern,
	}

	if result.ExecutedAt.IsZero() {
		result.ExecutedAt = time.Now()
	}

	if _, err := s.db.NewInsert().Model(result).Exec(ctx); err != nil {
		return nil, fmt.Errorf("failed to create pending query: %w", err)
	}

	return result, nil
}

// ResolveQueryApproval transitions a pending query to a terminal approval
// state. It is a compare-and-set on approval_status = 'pending': the update
// affects zero rows if somebody (or some other replica) already resolved it,
// which is what makes double-approve and approve-after-abandon safe.
func (s *Store) ResolveQueryApproval(
	ctx context.Context,
	uid uuid.UUID,
	status string,
	resolvedBy *uuid.UUID,
	reason string,
) error {
	now := time.Now()

	q := s.db.NewUpdate().
		Model((*Query)(nil)).
		Set("approval_status = ?", status).
		Set("resolved_at = ?", now).
		Where("uid = ?", uid).
		Where("approval_status = ?", ApprovalPending)

	if resolvedBy != nil {
		q = q.Set("resolved_by = ?", *resolvedBy)
	}

	if reason != "" {
		q = q.Set("resolution_reason = ?", reason)
	}

	res, err := q.Exec(ctx)
	if err != nil {
		return fmt.Errorf("failed to resolve query approval: %w", err)
	}

	affected, err := res.RowsAffected()
	if err != nil {
		// Driver doesn't report it — treat as success rather than failing a
		// decision that very likely landed.
		return nil //nolint:nilerr // see comment
	}

	if affected == 0 {
		return ErrQueryNotPending
	}

	return nil
}

// ListPendingApprovalQueries returns every query currently parked awaiting a
// decision, newest first. Backed by the partial index, so this stays cheap
// however large the queries table grows.
func (s *Store) ListPendingApprovalQueries(ctx context.Context) ([]Query, error) {
	var queries []Query

	err := s.db.NewSelect().
		Model(&queries).
		ColumnExpr("q.*, c.user_id, c.database_id").
		Join("JOIN connections c ON q.connection_id = c.uid").
		Where("q.approval_status = ?", ApprovalPending).
		Order("q.uid DESC").
		Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list pending approvals: %w", err)
	}

	if queries == nil {
		queries = []Query{}
	}

	return queries, nil
}

// GetQueryWithOwner loads a query plus the user/database of its connection —
// what the approve/deny path needs to decide who may resolve it and to render
// the resolution event.
func (s *Store) GetQueryWithOwner(ctx context.Context, uid uuid.UUID) (*Query, error) {
	result := new(Query)

	err := s.db.NewSelect().
		Model(result).
		ColumnExpr("q.*, c.user_id, c.database_id").
		Join("JOIN connections c ON q.connection_id = c.uid").
		Where("q.uid = ?", uid).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrQueryNotFound
		}

		return nil, fmt.Errorf("failed to get query: %w", err)
	}

	return result, nil
}
