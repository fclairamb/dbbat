package main

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/fclairamb/dbbat/internal/api"
	"github.com/fclairamb/dbbat/internal/archive"
	"github.com/fclairamb/dbbat/internal/config"
	"github.com/fclairamb/dbbat/internal/dump"
	"github.com/fclairamb/dbbat/internal/safe"
	"github.com/fclairamb/dbbat/internal/store"
)

const (
	// retentionSweepInterval is the backstop pass: it covers what events do not
	// (a replica that died before its timer fired, statements of sessions still
	// open, a window set through another replica).
	retentionSweepInterval = 1 * time.Hour

	// retentionMinPassGap is the resolution of the windows: passes are at least
	// this far apart, so a storm of closes costs one pass a second.
	retentionMinPassGap = 1 * time.Second

	// retentionTimerHorizon: a close arms a timer only when the nearest window of
	// its server is under this; longer windows are served by the hourly pass.
	retentionTimerHorizon = 1 * time.Hour
)

// goroutineNameRetentionSweep is what a panic in one pass is logged under.
const goroutineNameRetentionSweep = "retention sweep"

// retentionSweeper deletes (and archives) history past its retention windows.
//
// One per process. It runs a pass when it is woken: at startup, after a
// connection close, after a policy edit, and once an hour. A wake is a
// non-blocking send on a channel of size 1, so any number of closes during a
// pass collapse into one more pass and two passes never run at once.
//
// With no window set at any level it stays idle: no close wakes it and no pass
// reads a session. It still has to exist, because a window can be set from the
// UI while the process runs.
type retentionSweeper struct {
	store    *store.Store
	cfg      *config.Config
	archiver *archive.Archiver
	logger   *slog.Logger

	wakeCh   chan struct{}
	stop     chan struct{}
	stopOnce sync.Once

	// active is whether any window is set anywhere (env, parameter, group,
	// server). Refreshed at startup, after a policy edit and hourly.
	activeMu sync.RWMutex
	active   bool
}

// startRetention opens the archive bucket (nil when DBB_ARCHIVE_URL is unset,
// the default: archival does not exist) and starts the one retention sweeper of
// the process. The sweeper always exists: a window can be set from the UI while
// the process runs.
func startRetention(
	ctx context.Context,
	cfg *config.Config,
	dataStore *store.Store,
	apiServer *api.Server,
	logger *slog.Logger,
) (*retentionSweeper, error) {
	archiver, err := archive.Open(ctx, cfg.ArchiveURL, dataStore, logger)
	if err != nil {
		return nil, fmt.Errorf("failed to open the archive: %w", err)
	}

	apiServer.SetArchive(archiver)

	// After an hour of failing passes the archiver calls this once per outage.
	if archiver != nil {
		if notifier := apiServer.Notifier(); notifier != nil {
			archiver.OnPersistentFailure = func(cause error) {
				notifier.NotifyArchiveFailure(context.WithoutCancel(ctx), cause)
			}
		}
	}

	sweeper := startRetentionSweeper(ctx, cfg, dataStore, archiver, logger)
	apiServer.SetRetentionWaker(sweeper.Wake)

	return sweeper, nil
}

// startRetentionSweeper starts the sweeper. It always returns a sweeper.
func startRetentionSweeper(
	ctx context.Context,
	cfg *config.Config,
	dataStore *store.Store,
	archiver *archive.Archiver,
	logger *slog.Logger,
) *retentionSweeper {
	windows := cfg.RetentionWindows()

	if windows.Misconfiguration != "" {
		logger.WarnContext(ctx, "Invalid retention settings, nothing is deleted",
			slog.String("problem", windows.Misconfiguration))
	}

	if cfg.Retention.Queries.ArchiveAfter != "" && cfg.ArchiveURL == "" {
		logger.WarnContext(ctx, "DBB_RETENTION_QUERIES_ARCHIVE_AFTER is ignored: DBB_ARCHIVE_URL is not set")
	}

	s := &retentionSweeper{
		store:    dataStore,
		cfg:      cfg,
		archiver: archiver,
		logger:   logger,
		wakeCh:   make(chan struct{}, 1),
		stop:     make(chan struct{}),
	}

	dataStore.SetConnectionClosedHook(s.onConnectionClosed)

	go s.run(ctx)

	logger.InfoContext(ctx, "Retention sweeper started",
		slog.Duration("queries_delete_after", windows.Queries),
		slog.Duration("queries_archive_after", windows.QueriesArchive),
		slog.Duration("connections_delete_after", windows.Connections),
		slog.Duration("dumps_delete_after", windows.Dumps),
		slog.Bool("archive_enabled", archiver != nil))

	return s
}

// Wake asks for a pass. Never blocks.
func (s *retentionSweeper) Wake() {
	select {
	case s.wakeCh <- struct{}{}:
	default:
	}
}

// onConnectionClosed wakes the sweeper after a close, and arms a timer at the
// nearest window of that session's server when it is under an hour: the session
// that just closed is due in archive_after, not now, and nothing else may close
// in between.
func (s *retentionSweeper) onConnectionClosed(serverUID uuid.UUID, _ time.Time) {
	if !s.isActive() {
		return
	}

	s.Wake()

	go safe.RunMaintenance(context.Background(), s.logger, goroutineNameRetentionSweep, func() {
		s.armTimer(context.Background(), serverUID)
	})
}

func (s *retentionSweeper) armTimer(ctx context.Context, serverUID uuid.UUID) {
	instance, err := s.store.InstanceRetention(ctx)
	if err != nil {
		return
	}

	resolved, err := s.store.ResolveRetentionByServers(ctx, []uuid.UUID{serverUID}, instance)
	if err != nil {
		return
	}

	r := resolved[serverUID]

	nearest := time.Duration(0)

	for _, d := range []time.Duration{r.Archive.Duration, r.Delete.Duration} {
		if d > 0 && (nearest == 0 || d < nearest) {
			nearest = d
		}
	}

	if nearest > 0 && nearest < retentionTimerHorizon {
		time.AfterFunc(nearest+retentionMinPassGap, s.Wake)
	}
}

func (s *retentionSweeper) isActive() bool {
	s.activeMu.RLock()
	defer s.activeMu.RUnlock()

	return s.active
}

// refreshActive re-reads whether any window is set anywhere.
func (s *retentionSweeper) refreshActive(ctx context.Context) bool {
	w := s.cfg.RetentionWindows()
	active := w.Queries > 0 || w.Connections > 0 || w.QueriesArchive > 0

	if !active {
		set, err := s.store.AnyRetentionPolicySet(ctx)
		if err != nil {
			s.logger.WarnContext(ctx, "failed to read the retention policy", slog.Any("error", err))

			return s.isActive()
		}

		active = set
	}

	s.activeMu.Lock()
	s.active = active
	s.activeMu.Unlock()

	return active
}

func (s *retentionSweeper) run(ctx context.Context) {
	ticker := time.NewTicker(retentionSweepInterval)
	defer ticker.Stop()

	s.pass(ctx)

	for {
		select {
		case <-s.wakeCh:
			// Passes are at least a second apart.
			select {
			case <-time.After(retentionMinPassGap):
			case <-s.stop:
				return
			}

			s.pass(ctx)
		case <-ticker.C:
			s.pass(ctx)
		case <-s.stop:
			return
		case <-ctx.Done():
			return
		}
	}
}

// pass is one sweep, under safe.RunMaintenance: a panic must not take the
// process down and must not retire retention for its lifetime either.
func (s *retentionSweeper) pass(ctx context.Context) {
	safe.RunMaintenance(ctx, s.logger, goroutineNameRetentionSweep, func() {
		s.sweep(ctx)
	})
}

func (s *retentionSweeper) sweep(ctx context.Context) {
	windows := s.cfg.RetentionWindows()

	s.sweepDumps(ctx, windows)

	if !s.refreshActive(ctx) {
		return
	}

	// 1. Archive due connections. 2. Delete due archives.
	more := false

	if s.archiver != nil {
		more = s.sweepArchive(ctx)
	}

	// 3, 4. Delete due statements and ledger rows, per resolved window.
	instance, err := s.store.InstanceRetention(ctx)
	if err != nil {
		s.logger.ErrorContext(ctx, "retention sweep failed", slog.Any("error", err))

		return
	}

	if s.archiver == nil {
		instance.Archive = store.RetentionWindow{}
	}

	result, err := s.store.CleanupRetention(ctx, store.SweepPolicy{
		Instance:       instance,
		Connections:    windows.Connections,
		ArchiveEnabled: s.archiver != nil,
	})
	if err != nil {
		s.logger.ErrorContext(ctx, "retention sweep failed",
			slog.Any("error", err),
			slog.Int64("queries_deleted", result.Queries),
			slog.Int64("connections_deleted", result.Connections))

		return
	}

	if result.Queries > 0 || result.Connections > 0 {
		s.logger.InfoContext(ctx, "deleted history past retention",
			slog.Int64("queries", result.Queries),
			slog.Int64("connections", result.Connections))
	}

	if more {
		s.Wake()
	}
}

// sweepArchive runs the archive phases and reports whether a bound stopped them.
func (s *retentionSweeper) sweepArchive(ctx context.Context) bool {
	res, err := s.archiver.Pass(ctx)
	if err != nil {
		s.logger.ErrorContext(ctx, "archive pass failed", slog.Any("error", err))
	}

	if res.Archived > 0 || res.Deleted > 0 {
		s.logger.InfoContext(ctx, "archive pass",
			slog.Int("archived", res.Archived),
			slog.Int("archives_deleted", res.Deleted),
			slog.Bool("bounded", res.Bounded))
	}

	return res.Bounded
}

// sweepDumps deletes local capture files past their window.
func (s *retentionSweeper) sweepDumps(ctx context.Context, w config.RetentionWindows) {
	if s.cfg.Dump.Dir == "" || w.Dumps <= 0 {
		return
	}

	deleted, err := dump.CleanupOldFiles(s.cfg.Dump.Dir, w.Dumps)
	if err != nil {
		s.logger.ErrorContext(ctx, "dump cleanup failed", slog.Any("error", err))
	} else if deleted > 0 {
		s.logger.InfoContext(ctx, "dump cleanup", slog.Int("deleted", deleted))
	}
}

// Shutdown stops the sweeper. It never waits for an in-flight pass: the batched
// deletes make progress row by row, so an interrupted run resumes at the next
// start.
func (s *retentionSweeper) Shutdown(context.Context) error {
	s.stopOnce.Do(func() {
		close(s.stop)
	})

	return nil
}
