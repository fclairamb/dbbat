package store

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/uptrace/bun/dialect/pgdialect"
)

// ApproverKind names one of the two, deliberately non-hierarchical approver
// roles a server (or a server group) can carry.
//
// They do not imply one another: a query approver gains no say over grant
// requests, and an access approver gains no say over a held statement. An
// organization wanting overlap lists the same user group in both.
type ApproverKind string

const (
	// ApproverKindAccess names the group allowed to approve or deny grant
	// *requests* targeting a server. A grant request is an asynchronous policy
	// decision; its audience is ops.
	ApproverKindAccess ApproverKind = "access"
	// ApproverKindQuery names the group allowed to release approval *holds* on
	// statements running against a server. A hold blocks a live wire-protocol
	// connection, so its audience is whoever can answer fastest — typically
	// lead ops.
	ApproverKindQuery ApproverKind = "query"
)

// ApproverUserGroupUIDs returns the server's own list for one approver kind.
// Empty means "this server names nobody" — the caller falls back to the
// server's groups, and then to admins.
func (db *Server) ApproverUserGroupUIDs(kind ApproverKind) []uuid.UUID {
	if db == nil {
		return nil
	}

	if kind == ApproverKindAccess {
		return db.AccessApproverUserGroupUIDs
	}

	return db.QueryApproverUserGroupUIDs
}

// ApproverUserGroupUIDs returns the group's list for one approver kind — the
// fallback for every member server that names none of its own.
func (g *ServerGroup) ApproverUserGroupUIDs(kind ApproverKind) []uuid.UUID {
	if g == nil {
		return nil
	}

	if kind == ApproverKindAccess {
		return g.AccessApproverUserGroupUIDs
	}

	return g.QueryApproverUserGroupUIDs
}

// ResolveServerApproverGroups is **the** approver resolution: the single
// function every decision path (grant requests, query holds, both Slack
// transports) asks who may decide for a given database. Implementing the chain
// twice would guarantee the two drift, and a drift here is an authorization
// bug.
//
// The chain, most specific first:
//
//  1. the server's own list for this kind, when non-empty;
//  2. otherwise the **union** of the lists on every server group the server
//     currently belongs to — the same union rule a definition's multiple
//     approver groups already follow;
//  3. otherwise nothing, which every caller reads as "admins only".
//
// Levels do not union with each other: naming a group on the server is how an
// operator overrides the group-level default for that one database.
//
// Every read is live. Editing a list — or moving a server between groups —
// changes who may decide immediately, including for grant requests already
// filed and statements already parked. That is the deliberate second exception
// to dbbat's "a live grant's behavior never changes under it" rule (the first
// being server-group membership itself): approver lists are operational data,
// and a departed lead's replacement has to be effective now.
func (s *Store) ResolveServerApproverGroups(
	ctx context.Context, serverUID uuid.UUID, kind ApproverKind,
) ([]uuid.UUID, error) {
	server, err := s.GetServerByUID(ctx, serverUID)
	if err != nil {
		return nil, err
	}

	if own := server.ApproverUserGroupUIDs(kind); len(own) > 0 {
		return copyUUIDs(own), nil
	}

	groups, err := s.ListServerGroupsForServer(ctx, serverUID)
	if err != nil {
		return nil, err
	}

	seen := make(map[uuid.UUID]struct{})
	union := make([]uuid.UUID, 0)

	for i := range groups {
		for _, uid := range groups[i].ApproverUserGroupUIDs(kind) {
			if _, dup := seen[uid]; dup {
				continue
			}

			seen[uid] = struct{}{}
			union = append(union, uid)
		}
	}

	return union, nil
}

// MayApproveForServer reports whether a user holding userGroupUIDs is named,
// directly or through the server's groups, as an approver of this kind for
// this server. Admin-ness is the caller's business — any admin may decide
// anything — and so is the self-approval refusal, which no membership can
// override.
//
// A server that cannot be loaded (deleted since) resolves to false: the
// fail-closed direction.
func (s *Store) MayApproveForServer(
	ctx context.Context, serverUID uuid.UUID, kind ApproverKind, userGroupUIDs []uuid.UUID,
) (bool, error) {
	if len(userGroupUIDs) == 0 {
		return false, nil
	}

	approvers, err := s.ResolveServerApproverGroups(ctx, serverUID, kind)
	if err != nil {
		return false, err
	}

	return intersectsUUIDs(approvers, userGroupUIDs), nil
}

// HasServerApproverGroups reports whether any server or server group names one
// of the given user groups as an approver, of either kind. It is the
// server-level half of the "is this user an approver *somewhere*" question the
// approvals/pending subscribe gate asks, next to HasApproverGroups' grant-level
// half.
//
// Deliberately coarse — like its sibling, it gates subscribing, not receiving;
// each event is still filtered individually.
func (s *Store) HasServerApproverGroups(ctx context.Context, groupUIDs []uuid.UUID) (bool, error) {
	if len(groupUIDs) == 0 {
		return false, nil
	}

	arr := pgdialect.Array(groupUIDs)

	onServers, err := s.db.NewSelect().
		Model((*Server)(nil)).
		Where("access_approver_user_group_uids && ?", arr).
		WhereOr("query_approver_user_group_uids && ?", arr).
		Exists(ctx)
	if err != nil {
		return false, fmt.Errorf("failed to check server approver groups: %w", err)
	}

	if onServers {
		return true, nil
	}

	onGroups, err := s.db.NewSelect().
		Model((*ServerGroup)(nil)).
		Where("access_approver_user_group_uids && ?", arr).
		WhereOr("query_approver_user_group_uids && ?", arr).
		Exists(ctx)
	if err != nil {
		return false, fmt.Errorf("failed to check server group approver groups: %w", err)
	}

	return onGroups, nil
}

// intersectsUUIDs reports whether the two sets share at least one uid. Both
// are approver-group lists, which are short by construction, so the quadratic
// scan is cheaper than building a map.
func intersectsUUIDs(a, b []uuid.UUID) bool {
	for _, x := range a {
		for _, y := range b {
			if x == y {
				return true
			}
		}
	}

	return false
}
