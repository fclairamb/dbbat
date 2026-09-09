package main

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/fclairamb/dbbat/internal/config"
	"github.com/fclairamb/dbbat/internal/safe"
	"github.com/fclairamb/dbbat/internal/store"
)

// queryHistorySweepInterval is how often the query-history retention sweep
// runs. Retention is configured in hours or days, so an hourly sweep is ample —
// and it matches the cadence of the dump-file cleanup.
const queryHistorySweepInterval = 1 * time.Hour

// goroutineNameQueryRetentionSweep is what a panic in one sweep is logged under.
const goroutineNameQueryRetentionSweep = "query history retention sweep"

// queryRetentionSweeper deletes history past its retention window: statements
// (and the result rows captured for them) on one window, the closed-session
// ledger they hang off on another.
//
// There is exactly one sweeper per process, started next to the API server: the
// data lives in the shared store, not in any one protocol, so the proxies do
// not own a copy of this the way they each own a dump-file cleanup ticker.
type queryRetentionSweeper struct {
	store    *store.Store
	logger   *slog.Logger
	windows  config.RetentionWindows
	stop     chan struct{}
	stopOnce sync.Once
}

// startQueryRetentionSweep starts the retention sweeper and returns it, or
// returns nil when both windows are disabled — which is the default. Disabled
// means no goroutine and no ticker at all, not a sweep that finds nothing.
//
// An incoherent pair of windows lands here too: config.Config.RetentionWindows
// reports it and zeroes both, so a retention typo keeps everything and warns
// rather than deleting more than the operator asked for or refusing to start.
func startQueryRetentionSweep(
	ctx context.Context,
	cfg *config.Config,
	dataStore *store.Store,
	logger *slog.Logger,
) *queryRetentionSweeper {
	windows := cfg.RetentionWindows()

	if !windows.Enabled() {
		if windows.Misconfiguration != "" {
			logger.WarnContext(ctx, "Invalid history retention, history is kept forever",
				slog.String("problem", windows.Misconfiguration),
				slog.String("query_retention", cfg.QueryStorage.Retention),
				slog.String("connection_retention", cfg.Connection.Retention))
		}

		logger.InfoContext(ctx, "History retention disabled, history is kept forever",
			slog.String("hint", "set DBB_QUERY_STORAGE_RETENTION (e.g. 720h) to enable; "+
				"DBB_CONNECTION_RETENTION (e.g. 8760h) keeps the session ledger longer"))

		return nil
	}

	sweeper := &queryRetentionSweeper{
		store:   dataStore,
		logger:  logger,
		windows: windows,
		stop:    make(chan struct{}),
	}

	go sweeper.run(ctx)

	logger.InfoContext(ctx, "History retention enabled",
		slog.Duration("query_retention", windows.Query),
		slog.Duration("connection_retention", windows.Connection),
		slog.Bool("connections_kept_forever", windows.Connection <= 0),
		slog.Duration("sweep_interval", queryHistorySweepInterval))

	return sweeper
}

// run sweeps once at startup — so newly configured retention applies without
// waiting a full interval — then on every tick until shutdown.
//
// Each sweep runs under safe.RunMaintenance, per turn, exactly as the five
// proxies' dump-retention sweeps do. This is a goroutine of its own, so no
// recover reaches it: a panic in CleanupOldQueryRows would otherwise end the
// process and every live session on it. Per turn rather than around the loop,
// because a retention sweep that silently stopped running looks exactly like
// health from the outside while the history grows without bound.
func (s *queryRetentionSweeper) run(ctx context.Context) {
	s.guardedSweep(ctx)

	ticker := time.NewTicker(queryHistorySweepInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			s.guardedSweep(ctx)
		case <-s.stop:
			return
		}
	}
}

// guardedSweep is one sweep with a panic recover around it.
func (s *queryRetentionSweeper) guardedSweep(ctx context.Context) {
	safe.RunMaintenance(ctx, s.logger, goroutineNameQueryRetentionSweep, func() {
		s.sweep(ctx)
	})
}

func (s *queryRetentionSweeper) sweep(ctx context.Context) {
	result, err := s.store.CleanupOldQueryRows(ctx, s.windows.Query, s.windows.Connection)
	if err != nil {
		s.logger.ErrorContext(ctx, "history retention sweep failed",
			slog.Any("error", err),
			slog.Int64("queries_deleted", result.Queries),
			slog.Int64("connections_deleted", result.Connections))

		return
	}

	if result.Queries > 0 || result.Connections > 0 {
		s.logger.InfoContext(ctx, "deleted history past retention",
			slog.Int64("queries", result.Queries),
			slog.Int64("connections", result.Connections),
			slog.Duration("query_retention", s.windows.Query),
			slog.Duration("connection_retention", s.windows.Connection))
	}
}

// Shutdown stops the sweeper. It never waits for an in-flight sweep: the batched
// delete makes progress row by row, so an interrupted run simply resumes at the
// next start.
func (s *queryRetentionSweeper) Shutdown(context.Context) error {
	s.stopOnce.Do(func() {
		close(s.stop)
	})

	return nil
}
