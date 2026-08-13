package shared

import (
	"context"
	"log/slog"

	"github.com/google/uuid"
)

// GoroutineNameAPIKeyUsage is what a panic in the usage bump is logged under.
// It is one name for all five protocols and the REST API on purpose: the write
// is literally the same statement everywhere, so a second name would only make
// the log harder to grep.
const GoroutineNameAPIKeyUsage = "api key usage bump"

// APIKeyUsageStore is the slice of the store the usage bump needs.
type APIKeyUsageStore interface {
	IncrementAPIKeyUsage(ctx context.Context, id uuid.UUID) error
}

// BumpAPIKeyUsage records that an API key was just used, off the caller's
// latency path.
//
// Every proxy and the REST API's bearer middleware had its own copy of this
// one-liner, each an unguarded `go func()`: a panic in any of them ended the
// *process*, taking every live session of every user on every database with it.
// One helper is one recover.
//
// The write deliberately runs under context.WithoutCancel: it must outlive the
// login (or the HTTP request) that triggered it, which is canceled the moment
// that returns, while still carrying whatever tracing values the caller's
// context holds. Four of the five proxies used context.Background() before,
// which merely dropped those values; the REST middleware passed the *request*
// context, so its bump raced request completion and lost more often than not.
//
// The store error stays swallowed — nothing has ever branched on it, and a
// failed usage bump is not worth a line at anything above debug.
func BumpAPIKeyUsage(ctx context.Context, logger *slog.Logger, st APIKeyUsageStore, keyID uuid.UUID) {
	if st == nil {
		return
	}

	if logger == nil {
		logger = slog.Default()
	}

	writeCtx := context.WithoutCancel(ctx)

	go RunGuarded(writeCtx, logger, GoroutineNameAPIKeyUsage, func() {
		if err := st.IncrementAPIKeyUsage(writeCtx, keyID); err != nil {
			logger.DebugContext(writeCtx, "failed to record API key usage", slog.Any("error", err))
		}
	})
}
