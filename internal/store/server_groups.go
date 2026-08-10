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

// ErrServerGroupNotFound is returned when a server group lookup misses.
var ErrServerGroupNotFound = errors.New("server group not found")

// ErrServerGroupDuplicate is returned when a server group name collides (case
// insensitively) with an existing group.
var ErrServerGroupDuplicate = errors.New("server group with this name already exists")

// CreateServerGroup inserts a new server group. Names are unique
// case-insensitively.
func (s *Store) CreateServerGroup(ctx context.Context, group *ServerGroup) (*ServerGroup, error) {
	result := &ServerGroup{
		Name:        group.Name,
		Description: group.Description,
		CreatedBy:   group.CreatedBy,
		CreatedAt:   time.Now(),
	}

	if _, err := s.db.NewInsert().Model(result).Returning("*").Exec(ctx); err != nil {
		if isUniqueViolation(err, "server_groups_name_uniq") {
			return nil, ErrServerGroupDuplicate
		}

		return nil, fmt.Errorf("create server group: %w", err)
	}

	return result, nil
}

// GetServerGroup fetches a server group by UID.
func (s *Store) GetServerGroup(ctx context.Context, uid uuid.UUID) (*ServerGroup, error) {
	group := new(ServerGroup)

	if err := s.db.NewSelect().Model(group).Where("uid = ?", uid).Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrServerGroupNotFound
		}

		return nil, fmt.Errorf("get server group: %w", err)
	}

	return group, nil
}

// ListServerGroups returns every server group, name-ordered.
func (s *Store) ListServerGroups(ctx context.Context) ([]ServerGroup, error) {
	var groups []ServerGroup

	if err := s.db.NewSelect().Model(&groups).Order("name ASC").Scan(ctx); err != nil {
		return nil, fmt.Errorf("list server groups: %w", err)
	}

	if groups == nil {
		groups = []ServerGroup{}
	}

	return groups, nil
}

// UpdateServerGroup mutates the editable fields of a server group.
func (s *Store) UpdateServerGroup(ctx context.Context, group *ServerGroup) error {
	res, err := s.db.NewUpdate().
		Model(group).
		Column("name", "description").
		Where("uid = ?", group.UID).
		Exec(ctx)
	if err != nil {
		if isUniqueViolation(err, "server_groups_name_uniq") {
			return ErrServerGroupDuplicate
		}

		return fmt.Errorf("update server group: %w", err)
	}

	if rows, _ := res.RowsAffected(); rows == 0 {
		return ErrServerGroupNotFound
	}

	return nil
}

// DeleteServerGroup hard-deletes a server group. Memberships cascade away, and
// so does the `server_group_uid` of any grant bound to it (ON DELETE SET NULL),
// which narrows those grants back to their anchor database — never widens them.
// Grant definition scopes do NOT cascade: a definition scoped to the deleted
// group keeps the dangling uid and therefore matches no database (fail closed)
// until an admin edits it.
func (s *Store) DeleteServerGroup(ctx context.Context, uid uuid.UUID) error {
	res, err := s.db.NewDelete().
		Model((*ServerGroup)(nil)).
		Where("uid = ?", uid).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("delete server group: %w", err)
	}

	if rows, _ := res.RowsAffected(); rows == 0 {
		return ErrServerGroupNotFound
	}

	return nil
}

// AddServerToGroup adds a membership. Idempotent: re-adding is a no-op.
func (s *Store) AddServerToGroup(ctx context.Context, groupUID, serverUID uuid.UUID) error {
	_, err := s.db.NewInsert().
		Model(&ServerGroupMember{GroupUID: groupUID, ServerUID: serverUID}).
		On("CONFLICT (group_uid, server_uid) DO NOTHING").
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("add server to group: %w", err)
	}

	return nil
}

// RemoveServerFromGroup drops a membership. Removing a non-membership is a
// no-op rather than an error.
func (s *Store) RemoveServerFromGroup(ctx context.Context, groupUID, serverUID uuid.UUID) error {
	_, err := s.db.NewDelete().
		Model((*ServerGroupMember)(nil)).
		Where("group_uid = ?", groupUID).
		Where("server_uid = ?", serverUID).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("remove server from group: %w", err)
	}

	return nil
}

// ListServerGroupMemberUIDs returns the server UIDs belonging to a group.
func (s *Store) ListServerGroupMemberUIDs(ctx context.Context, groupUID uuid.UUID) ([]uuid.UUID, error) {
	var uids []uuid.UUID

	err := s.db.NewSelect().
		Model((*ServerGroupMember)(nil)).
		Column("server_uid").
		Where("group_uid = ?", groupUID).
		Scan(ctx, &uids)
	if err != nil {
		return nil, fmt.Errorf("list server group members: %w", err)
	}

	if uids == nil {
		uids = []uuid.UUID{}
	}

	return uids, nil
}

// ListServerGroupMemberUIDsByGroups is the batched form of
// ListServerGroupMemberUIDs: it resolves the current membership of every
// group in groupUIDs with exactly one query, however many groups are asked
// about. This is what lets a grant-definition listing resolve every
// definition's scoped_database_uids without firing one membership query per
// definition (see GrantDefinition.ScopedDatabaseUIDs).
//
// A group with no members (or one not present in groupUIDs at all) is simply
// absent from the result map; callers read it with a plain map index, which
// yields a nil slice either way.
func (s *Store) ListServerGroupMemberUIDsByGroups(ctx context.Context, groupUIDs []uuid.UUID) (map[uuid.UUID][]uuid.UUID, error) {
	result := make(map[uuid.UUID][]uuid.UUID, len(groupUIDs))

	if len(groupUIDs) == 0 {
		return result, nil
	}

	var members []ServerGroupMember

	err := s.db.NewSelect().
		Model(&members).
		Where("group_uid IN (?)", bun.List(groupUIDs)).
		Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("list server group members by groups: %w", err)
	}

	for _, m := range members {
		result[m.GroupUID] = append(result[m.GroupUID], m.ServerUID)
	}

	return result, nil
}

// ListServerGroupMembers returns the server rows belonging to a group.
func (s *Store) ListServerGroupMembers(ctx context.Context, groupUID uuid.UUID) ([]Server, error) {
	var servers []Server

	err := s.db.NewSelect().
		Model(&servers).
		Join("JOIN server_group_members AS m ON m.server_uid = d.uid").
		Where("m.group_uid = ?", groupUID).
		Order("d.name ASC").
		Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("list server group member servers: %w", err)
	}

	if servers == nil {
		servers = []Server{}
	}

	return servers, nil
}

// ListServerGroupUIDsForServer returns the UIDs of the server groups a given
// server currently belongs to.
//
// This is the auth path's membership lookup: every protocol resolves its grant
// through GetActiveGrant, and every scoping decision about a definition asks
// this question about the target database. Membership is read live and never
// snapshotted — see ServerGroup's doc comment for the consequence.
func (s *Store) ListServerGroupUIDsForServer(ctx context.Context, serverUID uuid.UUID) ([]uuid.UUID, error) {
	return serverGroupUIDsForServer(ctx, s.db, serverUID)
}

// serverGroupUIDsForServer is ListServerGroupUIDsForServer against an
// arbitrary bun handle, so the grant-materialization path can ask the same
// question inside the transaction that is issuing the grant.
func serverGroupUIDsForServer(ctx context.Context, db bun.IDB, serverUID uuid.UUID) ([]uuid.UUID, error) {
	var uids []uuid.UUID

	err := db.NewSelect().
		Model((*ServerGroupMember)(nil)).
		Column("group_uid").
		Where("server_uid = ?", serverUID).
		Scan(ctx, &uids)
	if err != nil {
		return nil, fmt.Errorf("list server group uids for server: %w", err)
	}

	if uids == nil {
		uids = []uuid.UUID{}
	}

	return uids, nil
}

// resolveServerGroupBinding decides which server group a grant materialized
// from def against databaseID binds to — the single answer every grant
// creation path uses, so no path can forget to bind one.
//
// nil means "anchor database only": either the definition is unscoped (it
// applies to every database, which is not the same as covering a named set) or
// the database is not currently in any of its groups. The latter should not
// happen — scope is enforced before issuance — but returning nil is the
// narrow, fail-closed answer either way.
//
// absent one: the grant column is nullable and NULL is its normal state.
//
//nolint:nilnil // nil here is a meaningful value ("no group binds"), not an
func resolveServerGroupBinding(
	ctx context.Context, db bun.IDB, def *GrantDefinition, databaseID uuid.UUID,
) (*uuid.UUID, error) {
	if def == nil || len(def.ServerGroupUIDs) == 0 {
		return nil, nil
	}

	groups, err := serverGroupUIDsForServer(ctx, db, databaseID)
	if err != nil {
		return nil, err
	}

	return def.MatchingServerGroup(groups), nil
}

// ListServerGroupsForServer returns the full group rows a server belongs to,
// for the server detail response and the admin UI.
func (s *Store) ListServerGroupsForServer(ctx context.Context, serverUID uuid.UUID) ([]ServerGroup, error) {
	var groups []ServerGroup

	err := s.db.NewSelect().
		Model(&groups).
		Join("JOIN server_group_members AS m ON m.group_uid = sg.uid").
		Where("m.server_uid = ?", serverUID).
		Order("sg.name ASC").
		Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("list server groups for server: %w", err)
	}

	if groups == nil {
		groups = []ServerGroup{}
	}

	return groups, nil
}

// SetServerGroupMembers replaces a group's membership with exactly the given
// set of servers, in one transaction so the group is never transiently empty
// (an empty group is a real access-control state, not a transient one).
func (s *Store) SetServerGroupMembers(ctx context.Context, groupUID uuid.UUID, serverUIDs []uuid.UUID) error {
	return s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.NewDelete().
			Model((*ServerGroupMember)(nil)).
			Where("group_uid = ?", groupUID).
			Exec(ctx); err != nil {
			return fmt.Errorf("set server group members (clear): %w", err)
		}

		for _, serverUID := range serverUIDs {
			if _, err := tx.NewInsert().
				Model(&ServerGroupMember{GroupUID: groupUID, ServerUID: serverUID}).
				On("CONFLICT (group_uid, server_uid) DO NOTHING").
				Exec(ctx); err != nil {
				return fmt.Errorf("set server group members (insert): %w", err)
			}
		}

		return nil
	})
}

// CountActiveGrantsForServerGroup reports how many currently-authorizing
// grants are bound to this server group. It is what tells an operator, before
// they add or remove a server, how much live access the edit moves — the
// blast radius of live membership, surfaced rather than implied.
func (s *Store) CountActiveGrantsForServerGroup(ctx context.Context, groupUID uuid.UUID) (int64, error) {
	count, err := s.db.NewSelect().
		Model((*AccessGrant)(nil)).
		Where("server_group_uid = ?", groupUID).
		Where("revoked_at IS NULL").
		Where("expires_at > NOW()").
		Where("grant_definition_id IN (SELECT uid FROM grant_definitions WHERE is_active)").
		Count(ctx)
	if err != nil {
		return 0, fmt.Errorf("count active grants for server group: %w", err)
	}

	return int64(count), nil
}
