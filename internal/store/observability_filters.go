package store

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/uptrace/bun"
)

// observabilityFilters is the one place the session-shaped filters — server
// group, grant instance, grant policy, grant provenance — are turned into SQL.
//
// Connections and queries are two views over the same sessions, so "everything
// that ran on the prod group" or "everything a human approved" must select the
// same sessions on both. Two hand-written copies of these predicates would
// drift, and a filter that quietly means something different on one endpoint
// than the other is worse than no filter at all. The only per-caller variation
// is the qualified name of the columns, since /queries reaches them through
// its `JOIN connections c`.
type observabilityFilters struct {
	// databaseColumn is the qualified connections.database_id column
	// ("database_id" for the connections listing, "c.database_id" for the
	// queries listing).
	databaseColumn string
	// grantColumn is the qualified connections.grant_uid column. Nullable:
	// sessions predating the stamp, and sessions that ran without a grant.
	grantColumn string
}

// applyServerGroup narrows to the servers a group holds **right now**.
//
// Membership is read live, at query time, never snapshotted — the same rule
// grant scope follows everywhere else in dbbat (see ServerGroup). A filtered
// view is therefore a view of *current* membership: a server added to the
// group makes its past sessions appear, and removing one makes them disappear.
//
// An empty group selects zero rows, via an impossible predicate rather than by
// dropping the clause. "This group has no servers" must never render as
// "unfiltered": silently widening a filter shows an investigator more than
// they asked for and gives them no sign it happened.
func (f observabilityFilters) applyServerGroup(
	ctx context.Context,
	s *Store,
	q *bun.SelectQuery,
	groupUID *uuid.UUID,
) (*bun.SelectQuery, error) {
	if groupUID == nil {
		return q, nil
	}

	members, err := s.ListServerGroupMemberUIDs(ctx, *groupUID)
	if err != nil {
		return nil, fmt.Errorf("resolve server group members: %w", err)
	}

	if len(members) == 0 {
		return q.Where("1 = 0"), nil
	}

	return q.Where(f.databaseColumn+" IN (?)", bun.List(members)), nil
}

// applyGrant narrows to the exact grant instance a session authenticated under.
func (f observabilityFilters) applyGrant(q *bun.SelectQuery, grantUID *uuid.UUID) *bun.SelectQuery {
	if grantUID == nil {
		return q
	}

	return q.Where(f.grantColumn+" = ?", *grantUID)
}

// applyGrantDefinition narrows to a grant *policy*, matched across the whole
// lineage of the definition named — not by uid equality.
//
// Editing a definition archives the current row and inserts a successor
// sharing its lineage_uid, so uid equality would return only the sessions that
// ran under the one version whose uid was passed and silently drop the rest.
// A uid naming an archived version resolves to the same lineage and returns
// the same rows, which is what makes a bookmarked filter keep working across
// an edit.
//
// EXISTS rather than `grant_uid IN (SELECT ...)`: the column is nullable, and
// a NULL on the left of IN against a subquery is the classic three-valued-logic
// footgun. EXISTS also keeps the plan sane as access_grants grows.
func (f observabilityFilters) applyGrantDefinition(q *bun.SelectQuery, definitionUID *uuid.UUID) *bun.SelectQuery {
	if definitionUID == nil {
		return q
	}

	return q.Where(
		"EXISTS (SELECT 1 FROM access_grants AS fag"+
			" JOIN grant_definitions AS fgd ON fgd.uid = fag.grant_definition_id"+
			" WHERE fag.uid = "+f.grantColumn+
			" AND fgd.lineage_uid = (SELECT lineage_uid FROM grant_definitions WHERE uid = ?))",
		*definitionUID)
}

// applyGrantProvenance narrows to how the session's grant was issued. Several
// values OR together; an empty slice applies no filter.
//
// The routes are read off grant_requests.decided_by, the convention
// ApproveGrantRequest (a human) and AutoApproveGrantRequest (NULL — the
// definition's policy decided) establish.
//
// A NULL grant_uid — a session predating the stamp, or one that ran without a
// grant — matches **no** provenance value, `direct` included. That is why
// `direct` carries an explicit IS NOT NULL guard: NOT EXISTS over a NULL is
// true, and without the guard every ungranted session in the store would file
// itself under "issued directly by an admin".
func (f observabilityFilters) applyGrantProvenance(
	q *bun.SelectQuery,
	provenance []GrantProvenance,
) *bun.SelectQuery {
	if len(provenance) == 0 {
		return q
	}

	return q.WhereGroup(" AND ", func(group *bun.SelectQuery) *bun.SelectQuery {
		for _, value := range provenance {
			switch value {
			case GrantProvenanceApproved:
				group = group.WhereOr(f.requestExists("fgr.decided_by IS NOT NULL"))
			case GrantProvenanceAuto:
				group = group.WhereOr(f.requestExists("fgr.decided_by IS NULL"))
			case GrantProvenanceDirect:
				// Parenthesised explicitly: bun OR-joins these clauses
				// verbatim, and relying on AND binding tighter than OR is a
				// silent-widening bug waiting for the next edit.
				group = group.WhereOr(
					"(" + f.grantColumn + " IS NOT NULL AND NOT " + f.requestExists("") + ")")
			default:
				// An unknown value can only come from a caller that skipped
				// ParseGrantProvenance. Select nothing rather than widen.
				group = group.WhereOr("1 = 0")
			}
		}

		return group
	})
}

// requestExists builds the EXISTS clause linking a session's grant back to the
// request that produced it, optionally further constrained.
func (f observabilityFilters) requestExists(extra string) string {
	clause := "EXISTS (SELECT 1 FROM grant_requests AS fgr WHERE fgr.resulting_grant_id = " + f.grantColumn
	if extra != "" {
		clause += " AND " + extra
	}

	return clause + ")"
}

// apply runs every session-shaped predicate in one call, so neither listing can
// forget one.
func (f observabilityFilters) apply(
	ctx context.Context,
	s *Store,
	q *bun.SelectQuery,
	serverGroupUID, grantUID, grantDefinitionUID *uuid.UUID,
	provenance []GrantProvenance,
) (*bun.SelectQuery, error) {
	q, err := f.applyServerGroup(ctx, s, q, serverGroupUID)
	if err != nil {
		return nil, err
	}

	q = f.applyGrant(q, grantUID)
	q = f.applyGrantDefinition(q, grantDefinitionUID)
	q = f.applyGrantProvenance(q, provenance)

	return q, nil
}

// connectionObservabilityFilters targets the connections listing, whose own
// row is the session.
var connectionObservabilityFilters = observabilityFilters{
	databaseColumn: "database_id",
	grantColumn:    "grant_uid",
}

// queryObservabilityFilters targets the queries listing, which reaches the
// session through the `JOIN connections c` it already performs.
var queryObservabilityFilters = observabilityFilters{
	databaseColumn: "c.database_id",
	grantColumn:    "c.grant_uid",
}
