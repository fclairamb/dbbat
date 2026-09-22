package store

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// ServerReferences is what an edit to one server row moves: the live access
// that would reach the new target on the next connect, and the server groups
// that carry the row along.
//
// It is the per-server counterpart of ServerGroupBlastRadius, and exists for
// the same reason: editing a server's host, port or database name does not
// re-issue anything, so every grant already covering the row starts pointing
// at whatever was typed. A UI that does not say so before the save is asking
// the admin to hold that in their head.
type ServerReferences struct {
	// ActiveGrants is how many currently-authorizing grants cover this server
	// — anchored on it, or bound to a server group that holds it.
	ActiveGrants int64
	// ServerGroups is how many server groups the row belongs to. Membership is
	// live, so those groups' scopes follow the edit too.
	ServerGroups int64
}

// serverGroupMembershipSubquery is the "groups that hold this server" scalar
// subquery both the coverage predicate and the protocol guard reuse, so the
// two can never disagree on what "bound to this server" means.
const serverGroupMembershipSubquery = "SELECT group_uid FROM server_group_members WHERE server_uid = ?"

// GetServerReferences counts what currently reaches, or carries, one server row.
//
// Two queries, each of them a count:
//
//   - the live grants covering the server, under the auth path's own liveness
//     predicate (applyGrantLiveness) rather than a second spelling of it — a
//     number that disagreed with what the proxy admits would be worse than no
//     number;
//   - the server groups holding it.
//
// Coverage is "anchored here **or** bound to a group holding this server",
// which is exactly what store.GetActiveGrant resolves at connect time.
func (s *Store) GetServerReferences(ctx context.Context, serverUID uuid.UUID) (*ServerReferences, error) {
	grants, err := applyGrantLiveness(s.db.NewSelect().
		Model((*AccessGrant)(nil)).
		Where("ag.database_id = ? OR ag.server_group_uid IN ("+serverGroupMembershipSubquery+")",
			serverUID, serverUID)).
		Count(ctx)
	if err != nil {
		return nil, fmt.Errorf("count active grants for server: %w", err)
	}

	groups, err := s.db.NewSelect().
		Model((*ServerGroupMember)(nil)).
		Where("sgm.server_uid = ?", serverUID).
		Count(ctx)
	if err != nil {
		return nil, fmt.Errorf("count server groups for server: %w", err)
	}

	return &ServerReferences{
		ActiveGrants: int64(grants),
		ServerGroups: int64(groups),
	}, nil
}

// ServerHasHistory reports whether anything is hanging off this server row:
// any grant at all (revoked and expired included) or any connection, open or
// closed.
//
// Deliberately wider than GetServerReferences' liveness filter, because it
// answers a different question. The blast radius is about *who reaches the new
// target*; this is about *whether the row already means something* — and a
// revoked grant or a closed session is history that a protocol change would
// silently re-label. One round trip: two EXISTS, no counting.
func (s *Store) ServerHasHistory(ctx context.Context, serverUID uuid.UUID) (bool, error) {
	var row struct {
		Found bool `bun:"found"`
	}

	err := s.db.NewSelect().
		ColumnExpr("(EXISTS (SELECT 1 FROM access_grants ag WHERE ag.database_id = ?"+
			" OR ag.server_group_uid IN ("+serverGroupMembershipSubquery+"))"+
			" OR EXISTS (SELECT 1 FROM connections c WHERE c.database_id = ?)) AS found",
			serverUID, serverUID, serverUID).
		TableExpr("(SELECT 1) AS one").
		Scan(ctx, &row)
	if err != nil {
		return false, fmt.Errorf("check server history: %w", err)
	}

	return row.Found, nil
}
