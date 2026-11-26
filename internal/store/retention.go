package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"

	"github.com/fclairamb/dbbat/internal/config"
)

// Retention parameters live in the global_parameters group "retention", keyed by
// the dotted setting name, and win over the environment like
// limits.statement_timeout does.
const (
	GroupRetention = "retention"

	KeyRetentionQueriesArchiveAfter = "queries.archive_after"
	KeyRetentionQueriesDeleteAfter  = "queries.delete_after"
)

// Retention levels, in the order they are consulted.
const (
	RetentionLevelServer      = "server"
	RetentionLevelServerGroup = "server_group"
	RetentionLevelInstance    = "instance"
	RetentionLevelNone        = ""
)

// Retention sources of the instance level.
const (
	RetentionSourceParameter = "parameter"
	RetentionSourceEnv       = "env"
)

// OptionalSeconds is a tri-state JSON integer for a retention window: absent
// leaves the stored value alone (Set false), null inherits (Set, nil Value), a
// number sets it (0 meaning never).
type OptionalSeconds struct {
	Set   bool
	Value *int64
}

// UnmarshalJSON implements json.Unmarshaler. It is only called when the key is
// present, which is what makes "absent" distinguishable from "null".
func (o *OptionalSeconds) UnmarshalJSON(b []byte) error {
	o.Set = true

	if string(b) == "null" {
		o.Value = nil

		return nil
	}

	var v int64
	if err := json.Unmarshal(b, &v); err != nil {
		return fmt.Errorf("%w: a number of seconds or null", ErrRetentionInvalid)
	}

	o.Value = &v

	return nil
}

// MarshalJSON implements json.Marshaler.
func (o OptionalSeconds) MarshalJSON() ([]byte, error) {
	if o.Value == nil {
		return []byte("null"), nil
	}

	return json.Marshal(*o.Value)
}

// ErrRetentionInvalid is a retention value the API refuses.
var ErrRetentionInvalid = errors.New("invalid retention window")

// RetentionParams are the raw instance-level parameters (Go duration strings,
// empty when unset so the environment applies).
type RetentionParams struct {
	ArchiveAfter string
	DeleteAfter  string
}

// GetRetentionParams reads the retention.queries.* parameters.
func (s *Store) GetRetentionParams(ctx context.Context) (RetentionParams, error) {
	params, err := s.GetParameters(ctx, GroupRetention)
	if err != nil {
		return RetentionParams{}, err
	}

	var p RetentionParams

	for _, kv := range params {
		switch kv.Key {
		case KeyRetentionQueriesArchiveAfter:
			p.ArchiveAfter = kv.Value
		case KeyRetentionQueriesDeleteAfter:
			p.DeleteAfter = kv.Value
		}
	}

	return p, nil
}

// SetRetentionParams writes the retention.queries.* parameters. An empty value
// deletes the parameter, which falls back to the environment.
func (s *Store) SetRetentionParams(ctx context.Context, p RetentionParams) error {
	for key, value := range map[string]string{
		KeyRetentionQueriesArchiveAfter: p.ArchiveAfter,
		KeyRetentionQueriesDeleteAfter:  p.DeleteAfter,
	} {
		if value == "" {
			if err := s.DeleteParameter(ctx, GroupRetention, key); err != nil &&
				!errors.Is(err, ErrParameterNotFound) {
				return err
			}

			continue
		}

		if err := s.SetParameter(ctx, GroupRetention, key, value); err != nil {
			return err
		}
	}

	return nil
}

// ParseRetentionParam parses a retention parameter: a Go duration, "0" for
// never, never negative. Sub-second precision is refused: the resolution of a
// window is one second.
func ParseRetentionParam(raw string) (time.Duration, error) {
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%w: %q is not a Go duration such as 90s or 720h", ErrRetentionInvalid, raw)
	}

	if d < 0 {
		return 0, fmt.Errorf("%w: %q is negative", ErrRetentionInvalid, raw)
	}

	if d%time.Second != 0 {
		return 0, fmt.Errorf("%w: %q has a sub-second part, the resolution is one second", ErrRetentionInvalid, raw)
	}

	return d, nil
}

// RetentionWindow is one resolved window and where it came from.
type RetentionWindow struct {
	// Duration is the resolved window, zero meaning never.
	Duration time.Duration `json:"-"`
	// Seconds is Duration in seconds, for the API.
	Seconds int64 `json:"seconds"`
	// Source is "server", "server_group" or "instance"; empty when nothing sets
	// a window at any level.
	Source string `json:"source"`
	// GroupUIDs are the groups the value came from when Source is server_group.
	GroupUIDs []uuid.UUID `json:"server_group_uids,omitempty"`
	// InstanceSource says where an instance-level value comes from:
	// "parameter" or "env".
	InstanceSource string `json:"instance_source,omitempty"`
}

// ResolvedRetention is both windows of one server, resolved.
type ResolvedRetention struct {
	Archive RetentionWindow `json:"archive"`
	Delete  RetentionWindow `json:"delete"`
}

// InstanceRetention is the instance level: the parameters over the environment.
type InstanceRetention struct {
	Archive RetentionWindow
	Delete  RetentionWindow
}

// ResolveInstanceRetention applies parameter-over-environment. A malformed
// parameter (written behind the API's back) folds to the environment value.
func ResolveInstanceRetention(p RetentionParams, env config.RetentionWindows) InstanceRetention {
	resolve := func(raw string, envValue time.Duration) RetentionWindow {
		if raw != "" {
			if d, err := ParseRetentionParam(raw); err == nil {
				return RetentionWindow{
					Duration: d, Seconds: int64(d / time.Second),
					Source: RetentionLevelInstance, InstanceSource: RetentionSourceParameter,
				}
			}
		}

		w := RetentionWindow{Duration: envValue, Seconds: int64(envValue / time.Second)}
		if envValue > 0 {
			w.Source = RetentionLevelInstance
			w.InstanceSource = RetentionSourceEnv
		}

		return w
	}

	return InstanceRetention{
		Archive: resolve(p.ArchiveAfter, env.QueriesArchive),
		Delete:  resolve(p.DeleteAfter, env.Queries),
	}
}

// InstanceRetention reads the parameters and resolves the instance level
// against the environment windows the store was started with.
func (s *Store) InstanceRetention(ctx context.Context) (InstanceRetention, error) {
	p, err := s.GetRetentionParams(ctx)
	if err != nil {
		return InstanceRetention{}, err
	}

	return ResolveInstanceRetention(p, config.RetentionWindows{
		Queries:        s.queryRetention,
		QueriesArchive: s.archiveRetention,
	}), nil
}

// retentionGroupRow is one (server, group) pair with the group's windows.
type retentionGroupRow struct {
	ServerUID           uuid.UUID `bun:"server_uid"`
	GroupUID            uuid.UUID `bun:"group_uid"`
	ArchiveAfterSeconds *int64    `bun:"archive_after_seconds"`
	DeleteAfterSeconds  *int64    `bun:"delete_after_seconds"`
}

// ResolveRetentionByServers resolves both windows of every server in two
// batched queries (the servers, then their groups), the same shape as
// ResolveServerApproverGroupsByServers.
//
// Each parameter resolves on its own: the server's value when not NULL;
// otherwise the groups that set one (several: the longest delete window and the
// shortest archive window, so conflicting policies resolve to the least
// destructive outcome); otherwise the instance. Soft-deleted servers are read
// too, so their sessions keep the window the server had.
//
// After resolution archival applies only when 0 < archive < delete or delete is
// never; otherwise the archive window is zero and the data is simply deleted at
// its delete window.
func (s *Store) ResolveRetentionByServers(
	ctx context.Context, serverUIDs []uuid.UUID, instance InstanceRetention,
) (map[uuid.UUID]ResolvedRetention, error) {
	resolved := make(map[uuid.UUID]ResolvedRetention, len(serverUIDs))

	if len(serverUIDs) == 0 {
		return resolved, nil
	}

	var servers []Server

	err := s.db.NewSelect().
		Model(&servers).
		WhereAllWithDeleted().
		Column("uid", "archive_after_seconds", "delete_after_seconds").
		Where("uid IN (?)", bun.List(serverUIDs)).
		Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve retention (servers): %w", err)
	}

	var groups []retentionGroupRow

	err = s.db.NewSelect().
		TableExpr("server_group_members AS sgm").
		ColumnExpr("sgm.server_uid, sgm.group_uid").
		ColumnExpr("sg.archive_after_seconds, sg.delete_after_seconds").
		Join("JOIN server_groups AS sg ON sg.uid = sgm.group_uid").
		Where("sgm.server_uid IN (?)", bun.List(serverUIDs)).
		Scan(ctx, &groups)
	if err != nil {
		return nil, fmt.Errorf("resolve retention (groups): %w", err)
	}

	byServer := make(map[uuid.UUID][]retentionGroupRow, len(servers))
	for _, g := range groups {
		byServer[g.ServerUID] = append(byServer[g.ServerUID], g)
	}

	known := make(map[uuid.UUID]*Server, len(servers))
	for i := range servers {
		known[servers[i].UID] = &servers[i]
	}

	for _, uid := range serverUIDs {
		var own archiveDelete

		if srv, ok := known[uid]; ok {
			own = archiveDelete{srv.ArchiveAfterSeconds, srv.DeleteAfterSeconds}
		}

		r := ResolvedRetention{
			Archive: resolveWindow(own.archive, groupValues(byServer[uid], true), instance.Archive, false),
			Delete:  resolveWindow(own.delete, groupValues(byServer[uid], false), instance.Delete, true),
		}

		if r.Archive.Duration > 0 && r.Delete.Duration > 0 && r.Archive.Duration >= r.Delete.Duration {
			r.Archive = RetentionWindow{}
		}

		resolved[uid] = r
	}

	return resolved, nil
}

type archiveDelete struct{ archive, delete *int64 }

type groupValue struct {
	uid     uuid.UUID
	seconds int64
}

func groupValues(rows []retentionGroupRow, archive bool) []groupValue {
	var out []groupValue

	for _, r := range rows {
		v := r.DeleteAfterSeconds
		if archive {
			v = r.ArchiveAfterSeconds
		}

		if v != nil {
			out = append(out, groupValue{r.GroupUID, *v})
		}
	}

	return out
}

// resolveWindow applies the three levels. longest picks the largest value among
// groups (delete); otherwise the smallest (archive). Zero means never, which is
// the largest value of all.
func resolveWindow(
	server *int64, groups []groupValue, instance RetentionWindow, longest bool,
) RetentionWindow {
	if server != nil {
		d := time.Duration(*server) * time.Second

		return RetentionWindow{Duration: d, Seconds: *server, Source: RetentionLevelServer}
	}

	if len(groups) > 0 {
		pick := groups[0].seconds

		for _, g := range groups[1:] {
			pick = pickWindow(pick, g.seconds, longest)
		}

		var uids []uuid.UUID

		for _, g := range groups {
			if g.seconds == pick {
				uids = append(uids, g.uid)
			}
		}

		return RetentionWindow{
			Duration: time.Duration(pick) * time.Second, Seconds: pick,
			Source: RetentionLevelServerGroup, GroupUIDs: uids,
		}
	}

	return instance
}

// pickWindow compares two windows where 0 means never (infinite).
func pickWindow(a, b int64, longest bool) int64 {
	aInf, bInf := a == 0, b == 0

	switch {
	case longest && (aInf || bInf):
		return 0
	case longest:
		return max(a, b)
	case aInf:
		return b
	case bInf:
		return a
	default:
		return min(a, b)
	}
}

// AnyRetentionPolicySet reports whether any window is set at any level of the
// store (a server, a server group or an instance parameter). The environment is
// the caller's to add. One small query: it is what keeps the sweeper off on an
// instance with no retention at all.
func (s *Store) AnyRetentionPolicySet(ctx context.Context) (bool, error) {
	var set bool

	err := s.db.NewRaw(`SELECT
		EXISTS (SELECT 1 FROM servers WHERE archive_after_seconds > 0 OR delete_after_seconds > 0)
		OR EXISTS (SELECT 1 FROM server_groups WHERE archive_after_seconds > 0 OR delete_after_seconds > 0)
		OR EXISTS (SELECT 1 FROM global_parameters WHERE group_key = ? AND deleted_at IS NULL AND value NOT IN ('', '0'))`,
		GroupRetention).Scan(ctx, &set)
	if err != nil {
		return false, fmt.Errorf("read retention policy: %w", err)
	}

	return set, nil
}

// RetentionServerUIDs lists every server uid, soft-deleted ones included.
func (s *Store) RetentionServerUIDs(ctx context.Context) ([]uuid.UUID, error) {
	var uids []uuid.UUID

	err := s.db.NewSelect().
		Model((*Server)(nil)).
		WhereAllWithDeleted().
		Column("uid").
		Scan(ctx, &uids)
	if err != nil {
		return nil, fmt.Errorf("list servers for retention: %w", err)
	}

	return uids, nil
}

// DeleteWindowForConnection resolves the delete window of a connection's
// server, falling back to the instance statement window when resolution fails.
func (s *Store) DeleteWindowForConnection(ctx context.Context, serverUID uuid.UUID) time.Duration {
	instance, err := s.InstanceRetention(ctx)
	if err != nil {
		return s.queryRetention
	}

	resolved, err := s.ResolveRetentionByServers(ctx, []uuid.UUID{serverUID}, instance)
	if err != nil {
		return instance.Delete.Duration
	}

	return resolved[serverUID].Delete.Duration
}

// SweepPolicy is what one retention pass applies.
type SweepPolicy struct {
	// Instance is the resolved instance level.
	Instance InstanceRetention
	// Connections is the ledger window (instance level, from the environment).
	Connections time.Duration
	// ArchiveEnabled is true when a bucket is configured: statements of
	// connections due for archival but not archived yet are then never deleted.
	ArchiveEnabled bool
}

// SweepResult counts what a pass deleted.
type SweepResult struct {
	Queries     int64
	Connections int64
}

// windowGroup collects the servers sharing one pair of windows.
type windowGroup struct {
	servers []uuid.UUID
	delete  time.Duration
	archive time.Duration
}

// CleanupRetention deletes statements (and their captured rows) and ledger rows
// past their windows, resolved per server.
//
//   - Statements: executed_at + resolved delete window, one batched delete per
//     distinct window. While archival is enabled, statements of a closed
//     connection due for archival but not archived are skipped, even past their
//     window: a failing bucket never turns into deletion.
//   - Ledger: a connection row is never deleted before its statements, so the
//     window is max(connections window, resolved delete window), never when
//     either is never, and rows with a live archive are skipped.
func (s *Store) CleanupRetention(ctx context.Context, policy SweepPolicy) (SweepResult, error) {
	var result SweepResult

	uids, err := s.RetentionServerUIDs(ctx)
	if err != nil {
		return result, err
	}

	resolved, err := s.ResolveRetentionByServers(ctx, uids, policy.Instance)
	if err != nil {
		return result, err
	}

	groups := make(map[[2]time.Duration]*windowGroup)

	for _, uid := range uids {
		r := resolved[uid]
		key := [2]time.Duration{r.Delete.Duration, r.Archive.Duration}

		g, ok := groups[key]
		if !ok {
			g = &windowGroup{delete: r.Delete.Duration, archive: r.Archive.Duration}
			groups[key] = g
		}

		g.servers = append(g.servers, uid)
	}

	now := time.Now()

	for _, g := range groups {
		if err := ctx.Err(); err != nil {
			return result, err
		}

		if g.delete > 0 {
			n, err := s.deleteStatementsPastWindow(ctx, g, now, policy.ArchiveEnabled)
			result.Queries += n

			if err != nil {
				return result, fmt.Errorf("failed to delete old queries: %w", err)
			}
		}

		if policy.Connections > 0 && g.delete > 0 {
			n, err := s.deleteConnectionsPastWindow(ctx, g, max(policy.Connections, g.delete), now, policy.ArchiveEnabled)
			result.Connections += n

			if err != nil {
				return result, fmt.Errorf("failed to delete old connections: %w", err)
			}
		}
	}

	// Servers with no statement window at all still honor a ledger window,
	// except that the ledger never goes before the statements.
	return result, nil
}

func (s *Store) deleteStatementsPastWindow(
	ctx context.Context, g *windowGroup, now time.Time, archiveEnabled bool,
) (int64, error) {
	var (
		sb   strings.Builder
		args []any
	)

	sb.WriteString(`DELETE FROM queries WHERE uid IN (
		SELECT q.uid FROM queries q
		JOIN connections c ON c.uid = q.connection_id
		WHERE c.database_id IN (?) AND q.executed_at < ?`)

	args = append(args, bun.List(g.servers), now.Add(-g.delete))

	if archiveEnabled && g.archive > 0 {
		// Due for archival but not archived: keep the rows the archive has not
		// taken yet, whatever the delete window says.
		sb.WriteString(`
		AND NOT (` + archivePending("c") + `)`)

		args = append(args, now.Add(-g.archive))
	}

	sb.WriteString(`
		LIMIT ?
	)`)

	return s.deleteBatches(ctx, sb.String(), args)
}

// archivePending is the SQL condition (one `?`: the archive cutoff) for "closed
// before the cutoff, rows still in PostgreSQL, so due for archival and not
// archived". Deleting such a connection, or its statements, would destroy rows
// a failing bucket has not taken yet.
func archivePending(alias string) string {
	return alias + `.disconnected_at IS NOT NULL AND ` + alias + `.archived_at IS NULL AND ` +
		alias + `.disconnected_at < ? AND ` + hasCapturedRows(alias)
}

// hasCapturedRows is the SQL condition "this connection captured at least one
// result row". A LIMIT 1 scalar subquery, not EXISTS: PostgreSQL turns an
// EXISTS into a semi-join and, with a large query_rows, hashes all of it (48s
// over 86M rows on a production store, past the driver's read timeout) where
// probing the indexes once per connection takes milliseconds. COALESCE keeps it
// false rather than NULL under NOT.
func hasCapturedRows(alias string) string {
	return `COALESCE((SELECT true FROM queries q2 JOIN query_rows qr ON qr.query_id = q2.uid
			WHERE q2.connection_id = ` + alias + `.uid LIMIT 1), false)`
}

func (s *Store) deleteConnectionsPastWindow(
	ctx context.Context, g *windowGroup, window time.Duration, now time.Time, archiveEnabled bool,
) (int64, error) {
	query := `DELETE FROM connections WHERE uid IN (
		SELECT c.uid FROM connections c
		WHERE c.database_id IN (?) AND c.disconnected_at IS NOT NULL AND c.disconnected_at < ?
		AND (c.archived_at IS NULL OR c.archive_deleted_at IS NOT NULL)`

	args := []any{bun.List(g.servers), now.Add(-window)}

	if archiveEnabled && g.archive > 0 {
		query += `
		AND NOT (` + archivePending("c") + `)`

		args = append(args, now.Add(-g.archive))
	}

	query += `
		LIMIT ?
	)`

	return s.deleteBatches(ctx, query, args)
}

// deleteBatches runs a LIMIT-ed delete (the batch size is the last argument)
// until it stops finding rows.
func (s *Store) deleteBatches(ctx context.Context, query string, args []any) (int64, error) {
	var total int64

	for {
		res, err := s.db.ExecContext(ctx, query, append(append([]any{}, args...), RetentionBatchSize)...)
		if err != nil {
			return total, err
		}

		affected, err := res.RowsAffected()
		if err != nil {
			return total, err
		}

		total += affected

		if affected < RetentionBatchSize {
			return total, nil
		}

		if err := ctx.Err(); err != nil {
			return total, err
		}
	}
}

// SetServerGroupRetention writes the retention windows of a server group. Only
// the fields that are Set are touched.
func (s *Store) SetServerGroupRetention(ctx context.Context, uid uuid.UUID, archive, del OptionalSeconds) error {
	if !archive.Set && !del.Set {
		return nil
	}

	q := s.db.NewUpdate().Model((*ServerGroup)(nil)).Where("uid = ?", uid)

	if archive.Set {
		q = q.Set("archive_after_seconds = ?", archive.Value)
	}

	if del.Set {
		q = q.Set("delete_after_seconds = ?", del.Value)
	}

	if _, err := q.Exec(ctx); err != nil {
		return fmt.Errorf("set server group retention: %w", err)
	}

	return nil
}
