package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// ErrUserGroupNotFound is returned when a group lookup misses.
var ErrUserGroupNotFound = errors.New("user group not found")

// ErrUserGroupDuplicate is returned when a group name collides (case
// insensitively) with an existing group.
var ErrUserGroupDuplicate = errors.New("user group with this name already exists")

// CreateUserGroup inserts a new group. Names are unique case-insensitively.
func (s *Store) CreateUserGroup(ctx context.Context, group *UserGroup) (*UserGroup, error) {
	result := &UserGroup{
		Name:        group.Name,
		Description: group.Description,
		CreatedBy:   group.CreatedBy,
		CreatedAt:   time.Now(),
	}

	if _, err := s.db.NewInsert().Model(result).Returning("*").Exec(ctx); err != nil {
		if isUniqueViolation(err, "user_groups_name_uniq") {
			return nil, ErrUserGroupDuplicate
		}

		return nil, fmt.Errorf("create user group: %w", err)
	}

	return result, nil
}

// GetUserGroup fetches a group by UID.
func (s *Store) GetUserGroup(ctx context.Context, uid uuid.UUID) (*UserGroup, error) {
	group := new(UserGroup)

	if err := s.db.NewSelect().Model(group).Where("uid = ?", uid).Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrUserGroupNotFound
		}

		return nil, fmt.Errorf("get user group: %w", err)
	}

	return group, nil
}

// ListUserGroups returns every group, name-ordered.
func (s *Store) ListUserGroups(ctx context.Context) ([]UserGroup, error) {
	var groups []UserGroup

	if err := s.db.NewSelect().Model(&groups).Order("name ASC").Scan(ctx); err != nil {
		return nil, fmt.Errorf("list user groups: %w", err)
	}

	return groups, nil
}

// UpdateUserGroup mutates the editable fields of a group.
func (s *Store) UpdateUserGroup(ctx context.Context, group *UserGroup) error {
	res, err := s.db.NewUpdate().
		Model(group).
		Column("name", "description").
		Where("uid = ?", group.UID).
		Exec(ctx)
	if err != nil {
		if isUniqueViolation(err, "user_groups_name_uniq") {
			return ErrUserGroupDuplicate
		}

		return fmt.Errorf("update user group: %w", err)
	}

	if rows, _ := res.RowsAffected(); rows == 0 {
		return ErrUserGroupNotFound
	}

	return nil
}

// DeleteUserGroup hard-deletes a group. Memberships cascade away; grant
// definition scopes do NOT — a definition scoped to the deleted group keeps
// the dangling uid and therefore matches nobody (fail closed) until an admin
// edits it.
func (s *Store) DeleteUserGroup(ctx context.Context, uid uuid.UUID) error {
	res, err := s.db.NewDelete().
		Model((*UserGroup)(nil)).
		Where("uid = ?", uid).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("delete user group: %w", err)
	}

	if rows, _ := res.RowsAffected(); rows == 0 {
		return ErrUserGroupNotFound
	}

	return nil
}

// AddUserToGroup adds a membership. Idempotent: re-adding is a no-op.
func (s *Store) AddUserToGroup(ctx context.Context, groupUID, userUID uuid.UUID) error {
	_, err := s.db.NewInsert().
		Model(&UserGroupMember{GroupUID: groupUID, UserUID: userUID}).
		On("CONFLICT (group_uid, user_uid) DO NOTHING").
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("add user to group: %w", err)
	}

	return nil
}

// RemoveUserFromGroup drops a membership. Removing a non-membership is a
// no-op rather than an error.
func (s *Store) RemoveUserFromGroup(ctx context.Context, groupUID, userUID uuid.UUID) error {
	_, err := s.db.NewDelete().
		Model((*UserGroupMember)(nil)).
		Where("group_uid = ?", groupUID).
		Where("user_uid = ?", userUID).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("remove user from group: %w", err)
	}

	return nil
}

// ListGroupMemberUIDs returns the user UIDs belonging to a group.
func (s *Store) ListGroupMemberUIDs(ctx context.Context, groupUID uuid.UUID) ([]uuid.UUID, error) {
	var uids []uuid.UUID

	err := s.db.NewSelect().
		Model((*UserGroupMember)(nil)).
		Column("user_uid").
		Where("group_uid = ?", groupUID).
		Scan(ctx, &uids)
	if err != nil {
		return nil, fmt.Errorf("list group members: %w", err)
	}

	if uids == nil {
		uids = []uuid.UUID{}
	}

	return uids, nil
}

// ListGroupMembers returns the (non-deleted) users belonging to a group.
func (s *Store) ListGroupMembers(ctx context.Context, groupUID uuid.UUID) ([]User, error) {
	var users []User

	err := s.db.NewSelect().
		Model(&users).
		Join("JOIN user_group_members AS m ON m.user_uid = u.uid").
		Where("m.group_uid = ?", groupUID).
		Order("u.username ASC").
		Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("list group member users: %w", err)
	}

	return users, nil
}

// ListUserGroupUIDs returns the UIDs of the groups a user belongs to. This is
// the eligibility input for GrantDefinition.AppliesTo.
func (s *Store) ListUserGroupUIDs(ctx context.Context, userUID uuid.UUID) ([]uuid.UUID, error) {
	var uids []uuid.UUID

	err := s.db.NewSelect().
		Model((*UserGroupMember)(nil)).
		Column("group_uid").
		Where("user_uid = ?", userUID).
		Scan(ctx, &uids)
	if err != nil {
		return nil, fmt.Errorf("list user group uids: %w", err)
	}

	if uids == nil {
		uids = []uuid.UUID{}
	}

	return uids, nil
}

// ListGroupsForUser returns the full group rows a user belongs to, for the
// user detail response and the admin UI.
func (s *Store) ListGroupsForUser(ctx context.Context, userUID uuid.UUID) ([]UserGroup, error) {
	var groups []UserGroup

	err := s.db.NewSelect().
		Model(&groups).
		Join("JOIN user_group_members AS m ON m.group_uid = ug.uid").
		Where("m.user_uid = ?", userUID).
		Order("ug.name ASC").
		Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("list groups for user: %w", err)
	}

	if groups == nil {
		groups = []UserGroup{}
	}

	return groups, nil
}

// SetGroupMembers replaces a group's membership with exactly the given set of
// users, in one transaction so the group is never transiently empty (an empty
// group is a real access-control state, not a transient one).
func (s *Store) SetGroupMembers(ctx context.Context, groupUID uuid.UUID, userUIDs []uuid.UUID) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("set group members: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.NewDelete().
		Model((*UserGroupMember)(nil)).
		Where("group_uid = ?", groupUID).
		Exec(ctx); err != nil {
		return fmt.Errorf("set group members (clear): %w", err)
	}

	for _, userUID := range userUIDs {
		if _, err := tx.NewInsert().
			Model(&UserGroupMember{GroupUID: groupUID, UserUID: userUID}).
			On("CONFLICT (group_uid, user_uid) DO NOTHING").
			Exec(ctx); err != nil {
			return fmt.Errorf("set group members (insert): %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("set group members (commit): %w", err)
	}

	return nil
}

// SetUserGroups replaces a user's group memberships with exactly the given
// set, in one transaction so the user is never transiently ungrouped.
func (s *Store) SetUserGroups(ctx context.Context, userUID uuid.UUID, groupUIDs []uuid.UUID) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("set user groups: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.NewDelete().
		Model((*UserGroupMember)(nil)).
		Where("user_uid = ?", userUID).
		Exec(ctx); err != nil {
		return fmt.Errorf("set user groups (clear): %w", err)
	}

	for _, groupUID := range groupUIDs {
		if _, err := tx.NewInsert().
			Model(&UserGroupMember{GroupUID: groupUID, UserUID: userUID}).
			On("CONFLICT (group_uid, user_uid) DO NOTHING").
			Exec(ctx); err != nil {
			return fmt.Errorf("set user groups (insert): %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("set user groups (commit): %w", err)
	}

	return nil
}
