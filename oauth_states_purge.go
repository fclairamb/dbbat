package main

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/fclairamb/dbbat/internal/safe"
)

// oauthStatePurgeInterval is how often expired oauth_states rows are deleted.
// The rows live minutes (OAuth authorize state, device-authorization requests,
// login-exchange codes), so ten minutes keeps the table at roughly what is
// still live without the purge being anything but a cheap indexed DELETE.
const oauthStatePurgeInterval = 10 * time.Minute

// goroutineNameOAuthStatePurge is what a panic in one purge is logged under.
const goroutineNameOAuthStatePurge = "expired oauth state purge"

// oauthStateCleaner is the one store call the purge needs. *store.Store
// satisfies it; tests substitute a recorder.
type oauthStateCleaner interface {
	CleanupExpiredOAuthStates(ctx context.Context) (int64, error)
}

// oauthStatePurger deletes expired oauth_states rows.
//
// It exists because unauthenticated endpoints create those rows —
// GET /api/v1/auth/<provider> and POST /api/v1/auth/device — and nothing used
// to delete them: every expired row stayed forever, growing the table and
// slowing the device flow's user-code lookup. It runs whatever the retention
// settings: these rows are not history anyone asked to keep, they are dead
// credentials.
//
// Its own goroutine rather than a ride on the instance heartbeat, because the
// heartbeat does not run when there is no instance id, and this must.
type oauthStatePurger struct {
	cleaner  oauthStateCleaner
	logger   *slog.Logger
	interval time.Duration
	stop     chan struct{}
	stopOnce sync.Once
}

// startOAuthStatePurge starts the purge and returns it for the shutdown
// sequence. It is never disabled.
func startOAuthStatePurge(ctx context.Context, cleaner oauthStateCleaner, logger *slog.Logger) *oauthStatePurger {
	purger := newOAuthStatePurger(cleaner, logger, oauthStatePurgeInterval)

	go purger.run(ctx)

	return purger
}

func newOAuthStatePurger(cleaner oauthStateCleaner, logger *slog.Logger, interval time.Duration) *oauthStatePurger {
	return &oauthStatePurger{
		cleaner:  cleaner,
		logger:   logger,
		interval: interval,
		stop:     make(chan struct{}),
	}
}

// run purges once at startup — whatever accumulated while no process was
// running goes straight away — then on every tick until shutdown. Each turn is
// guarded on its own, so one panic does not stop the purge for good.
func (p *oauthStatePurger) run(ctx context.Context) {
	p.guardedPurge(ctx)

	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			p.guardedPurge(ctx)
		case <-p.stop:
			return
		}
	}
}

func (p *oauthStatePurger) guardedPurge(ctx context.Context) {
	safe.RunMaintenance(ctx, p.logger, goroutineNameOAuthStatePurge, func() {
		p.purge(ctx)
	})
}

// purge deletes the expired rows. A failure is logged at warn: the next tick
// tries again, and nothing about serving traffic depends on it.
func (p *oauthStatePurger) purge(ctx context.Context) {
	deleted, err := p.cleaner.CleanupExpiredOAuthStates(ctx)
	if err != nil {
		p.logger.WarnContext(ctx, "failed to purge expired oauth states", slog.Any("error", err))

		return
	}

	if deleted > 0 {
		p.logger.DebugContext(ctx, "Purged expired oauth states", slog.Int64("rows", deleted))
	}
}

// Shutdown stops the purge. An in-flight DELETE is not waited for: it is one
// statement, and whatever it misses the next start deletes.
func (p *oauthStatePurger) Shutdown(context.Context) error {
	p.stopOnce.Do(func() {
		close(p.stop)
	})

	return nil
}
