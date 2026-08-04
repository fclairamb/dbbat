package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// InstanceHeartbeatInterval is how often a running process refreshes its
// instances row. Short — the whole point of the registry is to notice that a
// process is gone reasonably soon after it dies, and the write is a single
// primary-key upsert on a table with one row per replica.
const InstanceHeartbeatInterval = 30 * time.Second

// InstanceStaleAfter is how long an instances row may go un-refreshed before
// its owner is treated as dead and its still-open connections are reclaimed by
// whoever starts next.
//
// Deliberately 30 missed heartbeats, not two or three. This grace period is the
// only thing standing between the reclaim and the failure mode the instance
// scoping exists to prevent: closing a *live* replica's connections, which then
// satisfy the retention sweep's cutoff predicate and can be deleted while a
// session is still writing queries against them. The cost of being wrong in
// that direction is data loss and a foreign-key error in a live session; the
// cost of being wrong in the other direction is that a dead pod's rows linger
// until some instance restarts more than 15 minutes later. That asymmetry is
// why the multiplier is generous.
//
// 15 minutes also comfortably covers a rolling upgrade window: replicas still
// running a build that predates the instances table never heartbeat, and their
// seeded rows (see the 20260803030000_instances migration) must not go stale
// before the rollout has replaced them.
const InstanceStaleAfter = 30 * InstanceHeartbeatInterval

// InstanceReclaimInterval is how often a running process re-runs
// Store.ReclaimDeadInstanceConnections and PruneStaleInstances.
//
// Half the grace period, so a dead instance's rows are picked up within roughly
// one grace period of going stale rather than waiting for some unrelated
// process to start. That matters most in the crash case: a SIGKILLed pod leaves
// a registry row seconds old, so its immediate replacement reclaims nothing at
// startup, and 15 minutes later — when the row finally goes stale — nothing is
// starting any more. On a stable deployment the rows would then sit open until
// the next restart, which may be days away.
//
// It is not shorter because there is nothing to gain: the rows only become
// reclaimable after InstanceStaleAfter anyway, so polling faster would only add
// UPDATEs that match nothing. See the jitter in the heartbeat loop for why
// several replicas do not run it in lockstep.
const InstanceReclaimInterval = InstanceStaleAfter / 2

// instanceNow is the SQL expression every timestamp in the instance registry is
// written and compared against.
//
// One clock, the database's — never the process's. The heartbeat is written by
// one replica and judged by another, so mixing time bases would subtract the
// clock skew between them straight out of the grace period: a replica whose
// clock runs 5 minutes behind the reconciling one would be declared dead after
// 10 minutes of heartbeating, not 15. Both replicas already share the store, so
// its clock is the one thing they cannot disagree about. The migration that
// seeds the registry uses now() for the same reason.
const instanceNow = "now()"

// instanceStaleCutoff is the SQL expression for "last seen before the grace
// period", evaluated on the database clock to match instanceNow.
func instanceStaleCutoff() string {
	return fmt.Sprintf("%s - make_interval(secs => %f)", instanceNow, InstanceStaleAfter.Seconds())
}

// RegisterInstance records this run in the instance registry, resetting
// started_at: the row means "this process, this run".
//
// Call it at startup, before the reconcile and before any proxy accepts. Until
// the row exists this process looks dead to every other replica, so the window
// between the first connection it opens and its registration must be zero.
//
// The row is keyed by the pair, so registering never disturbs another live run
// that happens to carry the same instance id — before run ids that upsert
// overwrote the peer's heartbeat, which made a process that was serving traffic
// look like it had stopped reporting. In exchange, a restart leaves the
// previous run's row behind until it goes stale and is pruned; that is what
// makes the reconcile wait out the grace period rather than trusting an id.
//
// An empty instance id is refused: it is not an identity, and registering it
// would make the legacy no-owner connection rows look alive forever.
func (s *Store) RegisterInstance(ctx context.Context) error {
	if s.instanceID == "" {
		return nil
	}

	_, err := s.db.NewInsert().
		Model(&Instance{InstanceID: s.instanceID, RunID: s.runID}).
		// The database clock, never the process clock: see instanceNow.
		Value("started_at", instanceNow).
		Value("last_seen_at", instanceNow).
		On("CONFLICT (instance_id, run_id) DO UPDATE").
		Set("started_at = EXCLUDED.started_at").
		Set("last_seen_at = EXCLUDED.last_seen_at").
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("failed to register instance: %w", err)
	}

	return nil
}

// LiveRunsSharingInstanceID returns the registry rows of *other* runs that
// carry this process's instance id and are still heartbeating.
//
// Non-empty means several live processes share one DBB_INSTANCE_ID. The
// reconcile is safe when that happens — it keys on the run id — but it is still
// worth saying out loud at startup: the operator almost certainly meant the ids
// to be unique, the "own previous run" count stops meaning what its name says,
// and every session in the UI attributes to the same instance. Call it after
// RegisterInstance, which excludes our own freshly written row.
func (s *Store) LiveRunsSharingInstanceID(ctx context.Context) ([]Instance, error) {
	if s.instanceID == "" {
		return nil, nil
	}

	var peers []Instance

	err := s.db.NewSelect().
		Model(&peers).
		Where("instance_id = ?", s.instanceID).
		Where("run_id <> ?", s.runID).
		Where("last_seen_at >= " + instanceStaleCutoff()).
		Order("last_seen_at DESC").
		Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list live runs sharing this instance id: %w", err)
	}

	return peers, nil
}

// HeartbeatInstance refreshes this process's last_seen_at.
//
// It is an upsert rather than an UPDATE on purpose: a missing row is what marks
// an instance as dead, so if ours ever disappears — pruned by another instance
// after a long enough series of failed heartbeats, or wiped by an operator — it
// must come back on the next tick instead of leaving our live connections
// reclaimable. started_at is preserved, so the row keeps describing this run.
func (s *Store) HeartbeatInstance(ctx context.Context) error {
	if s.instanceID == "" {
		return nil
	}

	_, err := s.db.NewInsert().
		Model(&Instance{InstanceID: s.instanceID, RunID: s.runID}).
		Value("started_at", instanceNow).
		Value("last_seen_at", instanceNow).
		On("CONFLICT (instance_id, run_id) DO UPDATE").
		Set("last_seen_at = EXCLUDED.last_seen_at").
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("failed to heartbeat instance: %w", err)
	}

	return nil
}

// DeregisterInstance removes this run from the registry on a clean shutdown.
// That is what makes the common case immediate: the next process to start sees
// no row for us and reclaims anything we left open straight away, instead of
// waiting out InstanceStaleAfter.
//
// Scoped to our own run, not to our instance id: deleting every row carrying
// the id would deregister a live replica that shares it, and a replica with no
// registry row has all of its open sessions reclaimed by the next reconcile.
func (s *Store) DeregisterInstance(ctx context.Context) error {
	if s.instanceID == "" {
		return nil
	}

	_, err := s.db.NewDelete().
		Model((*Instance)(nil)).
		Where("instance_id = ?", s.instanceID).
		Where("run_id = ?", s.runID).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("failed to deregister instance: %w", err)
	}

	return nil
}

// GetInstance returns the most recently seen run registered under an instance
// id, or nil when the instance is not registered at all. Mostly useful to tests
// and to operators eyeballing the registry.
//
// One row per id was the schema until run ids arrived; now an id can carry
// several runs — a restart whose predecessor has not gone stale yet, or
// replicas sharing a pinned DBB_INSTANCE_ID — so this answers "is anything
// running under this id, and how fresh is it?". Nothing in the reconcile uses
// it: liveness is decided in SQL, on the pair. See LiveRunsSharingInstanceID
// for the full picture of one id.
func (s *Store) GetInstance(ctx context.Context, instanceID string) (*Instance, error) {
	inst := &Instance{}

	err := s.db.NewSelect().
		Model(inst).
		Where("instance_id = ?", instanceID).
		Order("last_seen_at DESC").
		Limit(1).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil //nolint:nilnil // "not registered" is a valid, non-error answer
		}

		return nil, fmt.Errorf("failed to get instance: %w", err)
	}

	return inst, nil
}

// PruneStaleInstances deletes registry rows whose owner is past the grace
// period. Purely housekeeping: a stale row and a missing row mean the same
// thing to the reconcile, so dropping it changes no decision — it just stops
// the table growing one row per pod name for the lifetime of the deployment,
// and now one row per crashed run of a stable instance id as well.
//
// Only call it after the reclaim has run, so the reclaim still sees the rows it
// is judging.
func (s *Store) PruneStaleInstances(ctx context.Context) (int64, error) {
	result, err := s.db.NewDelete().
		Model((*Instance)(nil)).
		Where("last_seen_at < " + instanceStaleCutoff()).
		Exec(ctx)
	if err != nil {
		return 0, fmt.Errorf("failed to prune stale instances: %w", err)
	}

	pruned, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("failed to get rows affected: %w", err)
	}

	return pruned, nil
}
