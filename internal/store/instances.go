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

// RegisterInstance records this process in the instance registry, resetting
// started_at: the row means "this process, this run".
//
// Call it at startup, before the reconcile and before any proxy accepts. Until
// the row exists this process looks dead to every other replica, so the window
// between the first connection it opens and its registration must be zero.
//
// An empty instance id is refused: it is not an identity, and registering it
// would make the legacy no-owner connection rows look alive forever.
func (s *Store) RegisterInstance(ctx context.Context) error {
	if s.instanceID == "" {
		return nil
	}

	_, err := s.db.NewInsert().
		Model(&Instance{InstanceID: s.instanceID}).
		// The database clock, never the process clock: see instanceNow.
		Value("started_at", instanceNow).
		Value("last_seen_at", instanceNow).
		On("CONFLICT (instance_id) DO UPDATE").
		Set("started_at = EXCLUDED.started_at").
		Set("last_seen_at = EXCLUDED.last_seen_at").
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("failed to register instance: %w", err)
	}

	return nil
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
		Model(&Instance{InstanceID: s.instanceID}).
		Value("started_at", instanceNow).
		Value("last_seen_at", instanceNow).
		On("CONFLICT (instance_id) DO UPDATE").
		Set("last_seen_at = EXCLUDED.last_seen_at").
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("failed to heartbeat instance: %w", err)
	}

	return nil
}

// DeregisterInstance removes this process from the registry on a clean
// shutdown. That is what makes the common case immediate: the next process to
// start sees no row for us and reclaims anything we left open straight away,
// instead of waiting out InstanceStaleAfter.
func (s *Store) DeregisterInstance(ctx context.Context) error {
	if s.instanceID == "" {
		return nil
	}

	_, err := s.db.NewDelete().
		Model((*Instance)(nil)).
		Where("instance_id = ?", s.instanceID).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("failed to deregister instance: %w", err)
	}

	return nil
}

// GetInstance returns one registry row, or nil when the instance is not
// registered. Mostly useful to tests and to operators eyeballing the registry.
func (s *Store) GetInstance(ctx context.Context, instanceID string) (*Instance, error) {
	inst := &Instance{}

	err := s.db.NewSelect().
		Model(inst).
		Where("instance_id = ?", instanceID).
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
// the table growing one row per pod name for the lifetime of the deployment.
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
