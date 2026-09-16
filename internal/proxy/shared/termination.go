package shared

import (
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/fclairamb/dbbat/internal/store"
)

// TerminationReasonFor maps a limit-guard violation to the vocabulary written
// on the connection row and into the audit entry.
//
// An unrecognized error returns "", which makes the resulting record a no-op
// rather than inventing a reason: the session still tears down, it is just not
// claimed to be a dbbat-initiated termination of a kind we can name.
func TerminationReasonFor(err error) string {
	switch {
	case errors.Is(err, ErrStatementTimeout):
		return store.TerminationStatementTimeout
	case errors.Is(err, ErrGrantRevoked):
		return store.TerminationGrantRevoked
	case errors.Is(err, ErrGrantExpired):
		return store.TerminationGrantExpired
	case errors.Is(err, ErrByteQuotaExceeded):
		return store.TerminationQuotaExceeded
	default:
		return ""
	}
}

// TerminationFor builds the record a session hands to the store when the
// watchdog tore it down: the reason, the statement that was in flight, and —
// for a statement timeout — the limit and how far past it the statement got.
//
// guard may be nil; the duration fields are then simply absent, which
// store.Termination.Message already handles.
func TerminationFor(err error, guard *LimitGuard, queryUID uuid.UUID) store.Termination {
	t := store.Termination{
		Reason:   TerminationReasonFor(err),
		QueryUID: queryUID,
	}

	if t.Reason != store.TerminationStatementTimeout {
		return t
	}

	t.Limit = guard.StatementLimit()
	t.Observed = guard.StatementOverrun()

	return t
}

// StatementTimeoutMessage is the one-liner a client is told when its statement
// was canceled for running too long, on the protocols where dbbat can still
// get a message out. It names the limit because "canceled" on its own tells
// the author nothing about how to fix the query.
func StatementTimeoutMessage(limit time.Duration) string {
	if limit <= 0 {
		return "statement canceled by dbbat: per-statement time limit exceeded"
	}

	return "statement canceled by dbbat: exceeded the " + limit.String() +
		" per-statement limit of your grant"
}
