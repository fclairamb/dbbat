package store

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/uptrace/bun"
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
	resolved, err := s.ResolveServerApproverGroupsByServers(ctx, []uuid.UUID{serverUID}, kind)
	if err != nil {
		return nil, err
	}

	groups, found := resolved[serverUID]
	if !found {
		return nil, ErrServerNotFound
	}

	return groups, nil
}

// ServerApprovers is a resolved approver answer for a set of servers, keyed by
// server uid: exactly what ResolveServerApproverGroups would have returned for
// each of them, computed in one shot.
//
// A server present in the store has an entry, possibly an empty one ("nobody
// but admins"). A server that does not exist — deleted since the row naming it
// was written — is **absent**, and every read of an absent key yields nil,
// which is the same fail-closed answer the per-server path gives by erroring.
type ServerApprovers map[uuid.UUID][]uuid.UUID

// MayApprove is MayApproveForServer's decision half applied to an
// already-resolved answer. Same rule, same fail-closed direction: no user
// groups, an unknown server, or a server naming nobody all mean false.
func (a ServerApprovers) MayApprove(serverUID uuid.UUID, userGroupUIDs []uuid.UUID) bool {
	if len(userGroupUIDs) == 0 {
		return false
	}

	return intersectsUUIDs(a[serverUID], userGroupUIDs)
}

// serverApproverGroupRow is the join projection the batched resolver reads: one
// row per (server, server group) membership, carrying the *group's* two
// approver lists. It exists only so the group step costs one query for any
// number of servers.
type serverApproverGroupRow struct {
	bun.BaseModel `bun:"table:server_group_members,alias:sgm"`

	ServerUID                   uuid.UUID   `bun:"server_uid,type:uuid"`
	AccessApproverUserGroupUIDs []uuid.UUID `bun:"access_approver_user_group_uids,array"`
	QueryApproverUserGroupUIDs  []uuid.UUID `bun:"query_approver_user_group_uids,array"`
}

// approverUserGroupUIDs picks the list for one kind through ServerGroup's own
// accessor, so which column answers which kind is decided in exactly one place.
func (r *serverApproverGroupRow) approverUserGroupUIDs(kind ApproverKind) []uuid.UUID {
	group := ServerGroup{
		AccessApproverUserGroupUIDs: r.AccessApproverUserGroupUIDs,
		QueryApproverUserGroupUIDs:  r.QueryApproverUserGroupUIDs,
	}

	return group.ApproverUserGroupUIDs(kind)
}

// ResolveServerApproverGroupsByServers is the batched form of
// ResolveServerApproverGroups, and — since that function is now a thin wrapper
// over this one — the actual home of the chain documented there. There is still
// one implementation; a listing simply asks it about every row at once.
//
// Cost is two queries whatever the number of servers: one for the server rows
// and their own lists, one joining server_group_members to server_groups for
// the group-level fallback. That is what keeps a delegated approver's pending
// page from firing two round trips per row.
//
// Liveness is unchanged: both reads happen now, so the answer reflects the
// approver lists and the group membership as they stand at this instant, not as
// they stood when the request was filed or the statement parked.
func (s *Store) ResolveServerApproverGroupsByServers(
	ctx context.Context, serverUIDs []uuid.UUID, kind ApproverKind,
) (ServerApprovers, error) {
	resolved := make(ServerApprovers, len(serverUIDs))

	if len(serverUIDs) == 0 {
		return resolved, nil
	}

	var servers []Server

	err := s.db.NewSelect().
		Model(&servers).
		Column("uid", "access_approver_user_group_uids", "query_approver_user_group_uids").
		Where("uid IN (?)", bun.In(serverUIDs)).
		Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve server approver groups (servers): %w", err)
	}

	// Level 1: a server naming its own approvers overrides the group level
	// outright. Everything else starts empty and may be filled by level 2.
	needGroups := make([]uuid.UUID, 0, len(servers))

	for i := range servers {
		if own := servers[i].ApproverUserGroupUIDs(kind); len(own) > 0 {
			resolved[servers[i].UID] = copyUUIDs(own)

			continue
		}

		resolved[servers[i].UID] = make([]uuid.UUID, 0)
		needGroups = append(needGroups, servers[i].UID)
	}

	if len(needGroups) == 0 {
		return resolved, nil
	}

	// Level 2: the union of the lists on every group the server belongs to,
	// ordered by group name so the union reads the same way the per-server
	// path (which ordered its groups by name) always did.
	var rows []serverApproverGroupRow

	err = s.db.NewSelect().
		Model(&rows).
		ColumnExpr("sgm.server_uid").
		ColumnExpr("sg.access_approver_user_group_uids").
		ColumnExpr("sg.query_approver_user_group_uids").
		Join("JOIN server_groups AS sg ON sg.uid = sgm.group_uid").
		Where("sgm.server_uid IN (?)", bun.In(needGroups)).
		Order("sg.name ASC").
		Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve server approver groups (groups): %w", err)
	}

	// Only servers in needGroups may be filled from here — a server that named
	// its own approvers was excluded from the query, and level 1 wins outright.
	seen := make(map[uuid.UUID]map[uuid.UUID]struct{}, len(needGroups))
	for _, uid := range needGroups {
		seen[uid] = make(map[uuid.UUID]struct{})
	}

	for i := range rows {
		serverUID := rows[i].ServerUID

		dedup, wanted := seen[serverUID]
		if !wanted {
			continue
		}

		union := resolved[serverUID]

		for _, uid := range rows[i].approverUserGroupUIDs(kind) {
			if _, dup := dedup[uid]; dup {
				continue
			}

			dedup[uid] = struct{}{}
			union = append(union, uid)
		}

		resolved[serverUID] = union
	}

	return resolved, nil
}

// MayApproveForServer reports whether a user holding userGroupUIDs is named,
// directly or through the server's groups, as an approver of this kind for
// this server. Admin-ness is the caller's business — any admin may decide
// anything — and so is the self-approval refusal, which no membership can
// override.
//
// A server that cannot be loaded (deleted since) resolves to false: the
// fail-closed direction.
//
// This is the one-server convenience. A caller judging many rows at once
// resolves with ResolveServerApproverGroupsByServers and asks the resulting
// ServerApprovers.MayApprove instead — the same intersection over the same
// chain, minus the per-row round trips.
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
