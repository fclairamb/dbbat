package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// obsFixture is a small world to filter over: one user, three servers, a
// server group, a grant definition and whatever sessions a test opens.
type obsFixture struct {
	store *Store
	ctx   context.Context //nolint:containedctx // a test fixture, not a request-scoped struct
	admin *User
	user  *User
	dbA   *Server
	dbB   *Server
	dbC   *Server
	group *ServerGroup
	def   *GrantDefinition
}

func newObsFixture(t *testing.T, suffix string) *obsFixture {
	t.Helper()

	s := setupTestStore(t)
	ctx := context.Background()

	admin := createTestAdmin(t, ctx, s, "obs_"+suffix)
	user, dbA := createTestUserAndDatabase(t, ctx, s, "obs_"+suffix)
	dbB := newTestTargetServer(t, ctx, s, "obs_b_"+suffix+"_"+uuid.NewString()[:8])
	dbC := newTestTargetServer(t, ctx, s, "obs_c_"+suffix+"_"+uuid.NewString()[:8])

	group, err := s.CreateServerGroup(ctx, &ServerGroup{Name: "obs-" + uuid.NewString()[:8]})
	if err != nil {
		t.Fatalf("CreateServerGroup() error = %v", err)
	}

	def := newTestGrantDefinition(t, ctx, s, admin.UID, GrantDefinition{
		Controls: []string{ControlReadOnly},
	})

	return &obsFixture{
		store: s, ctx: ctx, admin: admin, user: user,
		dbA: dbA, dbB: dbB, dbC: dbC, group: group, def: def,
	}
}

// session opens a connection on a database, optionally stamped with a grant.
func (f *obsFixture) session(t *testing.T, databaseID uuid.UUID, grantUID *uuid.UUID) *Connection {
	t.Helper()

	opts := []ConnectionOption{}
	if grantUID != nil {
		opts = append(opts, WithGrantUID(*grantUID))
	}

	conn, err := f.store.CreateConnection(f.ctx, f.user.UID, databaseID, "10.0.0.1", opts...)
	if err != nil {
		t.Fatalf("CreateConnection() error = %v", err)
	}

	return conn
}

// statement logs one query on a session, so the queries listing has something
// to select and every session-scoped filter can be asserted on both endpoints.
func (f *obsFixture) statement(t *testing.T, conn *Connection, sql string) *Query {
	t.Helper()

	q, err := f.store.CreateQuery(f.ctx, &Query{ConnectionID: conn.UID, SQLText: sql})
	if err != nil {
		t.Fatalf("CreateQuery() error = %v", err)
	}

	return q
}

// grant issues a grant straight through CreateGrant — the `direct` provenance
// route (an admin using POST /grants: a human act, but never an approval).
func (f *obsFixture) grant(t *testing.T, databaseID uuid.UUID) *Grant {
	t.Helper()

	now := time.Now()

	return newTestGrant(t, f.ctx, f.store, f.def, f.user.UID, databaseID, f.admin.UID,
		now.Add(-time.Minute), now.Add(time.Hour))
}

// connectionUIDs lists the sessions a filter selects, scoped to this fixture's
// user so a parallel test's rows can never leak in.
func (f *obsFixture) connectionUIDs(t *testing.T, filter ConnectionFilter) []uuid.UUID {
	t.Helper()

	filter.UserID = &f.user.UID

	conns, err := f.store.ListConnections(f.ctx, filter)
	if err != nil {
		t.Fatalf("ListConnections() error = %v", err)
	}

	uids := make([]uuid.UUID, 0, len(conns))
	for i := range conns {
		uids = append(uids, conns[i].UID)
	}

	return uids
}

// queryUIDs is the /queries counterpart of connectionUIDs.
func (f *obsFixture) queryUIDs(t *testing.T, filter QueryFilter) []uuid.UUID {
	t.Helper()

	filter.UserID = &f.user.UID

	queries, err := f.store.ListQueries(f.ctx, filter)
	if err != nil {
		t.Fatalf("ListQueries() error = %v", err)
	}

	uids := make([]uuid.UUID, 0, len(queries))
	for i := range queries {
		uids = append(uids, queries[i].UID)
	}

	return uids
}

func assertSameSet(t *testing.T, label string, got, want []uuid.UUID) {
	t.Helper()

	if len(got) != len(want) {
		t.Fatalf("%s selected %d rows, want %d (got %v, want %v)", label, len(got), len(want), got, want)
	}

	index := make(map[uuid.UUID]bool, len(got))
	for _, uid := range got {
		index[uid] = true
	}

	for _, uid := range want {
		if !index[uid] {
			t.Fatalf("%s did not select %s (got %v)", label, uid, got)
		}
	}
}

// TestServerGroupFilterResolvesMembershipLive is the documented semantic: a
// filtered view is a view of the group's membership *right now*, not of
// membership at session time. Adding a server retroactively widens it, removing
// one retroactively narrows it — the same rule grant scope follows.
func TestServerGroupFilterResolvesMembershipLive(t *testing.T) {
	t.Parallel()

	f := newObsFixture(t, "sglive")

	onA := f.session(t, f.dbA.UID, nil)
	onB := f.session(t, f.dbB.UID, nil)
	f.session(t, f.dbC.UID, nil)

	stmtA := f.statement(t, onA, "SELECT 1")
	stmtB := f.statement(t, onB, "SELECT 2")

	// Empty group: zero rows, never "unfiltered". This is the one that would
	// silently show everything if the IN clause were skipped instead of made
	// impossible.
	assertSameSet(t, "empty group (connections)",
		f.connectionUIDs(t, ConnectionFilter{ServerGroupUID: &f.group.UID}), nil)
	assertSameSet(t, "empty group (queries)",
		f.queryUIDs(t, QueryFilter{ServerGroupUID: &f.group.UID}), nil)

	if err := f.store.AddServerToGroup(f.ctx, f.group.UID, f.dbA.UID); err != nil {
		t.Fatalf("AddServerToGroup() error = %v", err)
	}

	assertSameSet(t, "group{A}",
		f.connectionUIDs(t, ConnectionFilter{ServerGroupUID: &f.group.UID}),
		[]uuid.UUID{onA.UID})
	assertSameSet(t, "group{A} (queries)",
		f.queryUIDs(t, QueryFilter{ServerGroupUID: &f.group.UID}),
		[]uuid.UUID{stmtA.UID})

	// The same two calls, after a membership change and with no session
	// touched in between, must answer differently.
	if err := f.store.AddServerToGroup(f.ctx, f.group.UID, f.dbB.UID); err != nil {
		t.Fatalf("AddServerToGroup() error = %v", err)
	}

	assertSameSet(t, "group{A,B}",
		f.connectionUIDs(t, ConnectionFilter{ServerGroupUID: &f.group.UID}),
		[]uuid.UUID{onA.UID, onB.UID})
	assertSameSet(t, "group{A,B} (queries)",
		f.queryUIDs(t, QueryFilter{ServerGroupUID: &f.group.UID}),
		[]uuid.UUID{stmtA.UID, stmtB.UID})

	if err := f.store.RemoveServerFromGroup(f.ctx, f.group.UID, f.dbA.UID); err != nil {
		t.Fatalf("RemoveServerFromGroup() error = %v", err)
	}

	assertSameSet(t, "group{B}",
		f.connectionUIDs(t, ConnectionFilter{ServerGroupUID: &f.group.UID}),
		[]uuid.UUID{onB.UID})
}

// TestFiltersComposeWithAND: server_group_uid + database_id is an
// intersection, not a union — usually a single server, or empty.
func TestFiltersComposeWithAND(t *testing.T) {
	t.Parallel()

	f := newObsFixture(t, "andcompose")

	onA := f.session(t, f.dbA.UID, nil)
	f.session(t, f.dbB.UID, nil)

	for _, uid := range []uuid.UUID{f.dbA.UID, f.dbB.UID} {
		if err := f.store.AddServerToGroup(f.ctx, f.group.UID, uid); err != nil {
			t.Fatalf("AddServerToGroup() error = %v", err)
		}
	}

	assertSameSet(t, "group ∩ dbA",
		f.connectionUIDs(t, ConnectionFilter{ServerGroupUID: &f.group.UID, DatabaseID: &f.dbA.UID}),
		[]uuid.UUID{onA.UID})

	// A server outside the group intersects to nothing.
	assertSameSet(t, "group ∩ dbC",
		f.connectionUIDs(t, ConnectionFilter{ServerGroupUID: &f.group.UID, DatabaseID: &f.dbC.UID}), nil)
}

// TestGrantUIDFilterSelectsOneInstance covers the exact-instance filter that
// the connection detail page deep-links from.
func TestGrantUIDFilterSelectsOneInstance(t *testing.T) {
	t.Parallel()

	f := newObsFixture(t, "grantuid")

	first := f.grant(t, f.dbA.UID)
	second := f.grant(t, f.dbA.UID)

	underFirst := f.session(t, f.dbA.UID, &first.UID)
	underSecond := f.session(t, f.dbA.UID, &second.UID)
	ungranted := f.session(t, f.dbA.UID, nil)

	stmtFirst := f.statement(t, underFirst, "SELECT 1")
	f.statement(t, underSecond, "SELECT 2")
	f.statement(t, ungranted, "SELECT 3")

	assertSameSet(t, "grant_uid = first",
		f.connectionUIDs(t, ConnectionFilter{GrantUID: &first.UID}),
		[]uuid.UUID{underFirst.UID})
	assertSameSet(t, "grant_uid = first (queries)",
		f.queryUIDs(t, QueryFilter{GrantUID: &first.UID}),
		[]uuid.UUID{stmtFirst.UID})
	assertSameSet(t, "grant_uid = second",
		f.connectionUIDs(t, ConnectionFilter{GrantUID: &second.UID}),
		[]uuid.UUID{underSecond.UID})
}

// TestGrantDefinitionFilterMatchesAcrossLineage is the resolved open question:
// the policy filter must be matched across the definition's lineage, not by
// uid equality, so an edit-archival does not split the history. Filtering by
// *either* version's uid returns the sessions that ran under *both*.
func TestGrantDefinitionFilterMatchesAcrossLineage(t *testing.T) {
	t.Parallel()

	f := newObsFixture(t, "lineage")

	// A session under the original version of the policy.
	oldGrant := f.grant(t, f.dbA.UID)
	underOld := f.session(t, f.dbA.UID, &oldGrant.UID)
	stmtOld := f.statement(t, underOld, "SELECT 'old'")

	// Edit the definition: the current row is archived and a successor is
	// inserted, sharing its lineage_uid.
	edited := *f.def
	edited.Name = f.def.Name + "-v2"
	edited.Slug = f.def.Slug + "-v2"

	successor, err := f.store.UpdateGrantDefinition(f.ctx, &edited)
	if err != nil {
		t.Fatalf("UpdateGrantDefinition() error = %v", err)
	}

	if successor.UID == f.def.UID {
		t.Fatal("UpdateGrantDefinition() did not version the definition")
	}

	if successor.LineageUID != f.def.LineageUID {
		t.Fatalf("successor lineage = %s, want %s", successor.LineageUID, f.def.LineageUID)
	}

	// A session under the new version.
	now := time.Now()
	newGrant := newTestGrant(t, f.ctx, f.store, successor, f.user.UID, f.dbA.UID, f.admin.UID,
		now.Add(-time.Minute), now.Add(time.Hour))
	underNew := f.session(t, f.dbA.UID, &newGrant.UID)
	stmtNew := f.statement(t, underNew, "SELECT 'new'")

	// A session under an unrelated policy, which must never be selected.
	otherDef := newTestGrantDefinition(t, f.ctx, f.store, f.admin.UID, GrantDefinition{
		Controls: []string{ControlBlockDDL},
	})
	otherGrant := newTestGrant(t, f.ctx, f.store, otherDef, f.user.UID, f.dbA.UID, f.admin.UID,
		now.Add(-time.Minute), now.Add(time.Hour))
	underOther := f.session(t, f.dbA.UID, &otherGrant.UID)
	f.statement(t, underOther, "SELECT 'other'")

	both := []uuid.UUID{underOld.UID, underNew.UID}
	bothStatements := []uuid.UUID{stmtOld.UID, stmtNew.UID}

	// Either version's uid selects both sessions — including the archived
	// predecessor's, which is what makes a bookmarked filter survive an edit.
	for label, uid := range map[string]uuid.UUID{
		"archived predecessor": f.def.UID,
		"live successor":       successor.UID,
	} {
		filterUID := uid

		assertSameSet(t, "grant_definition_uid = "+label,
			f.connectionUIDs(t, ConnectionFilter{GrantDefinitionUID: &filterUID}), both)
		assertSameSet(t, "grant_definition_uid = "+label+" (queries)",
			f.queryUIDs(t, QueryFilter{GrantDefinitionUID: &filterUID}), bothStatements)
	}

	assertSameSet(t, "grant_definition_uid = unrelated policy",
		f.connectionUIDs(t, ConnectionFilter{GrantDefinitionUID: &otherDef.UID}),
		[]uuid.UUID{underOther.UID})
}

// requestedGrant files a grant request and resolves it, returning the grant it
// materialized.
//
// Deliberately built through ApproveGrantRequest / AutoApproveGrantRequest
// rather than by inserting grant_requests rows by hand: the whole provenance
// filter rests on the decided_by convention those two functions establish
// (a named human, versus NULL for "the policy decided"), so this test must
// break if that convention ever moves.
func (f *obsFixture) requestedGrant(t *testing.T, auto bool) *Grant {
	t.Helper()

	req, err := f.store.CreateGrantRequest(f.ctx, &GrantRequest{
		UserID:            f.user.UID,
		GrantDefinitionID: f.def.UID,
		DatabaseID:        f.dbA.UID,
		Justification:     "provenance fixture",
	})
	if err != nil {
		t.Fatalf("CreateGrantRequest() error = %v", err)
	}

	var grant *Grant

	if auto {
		grant, _, err = f.store.AutoApproveGrantRequest(f.ctx, req.UID, f.user.UID)
	} else {
		grant, _, err = f.store.ApproveGrantRequest(f.ctx, req.UID, f.admin.UID)
	}

	if err != nil {
		t.Fatalf("approve request (auto=%v) error = %v", auto, err)
	}

	return grant
}

// TestGrantProvenanceFilter walks one session per route — human-approved,
// auto-approved, admin-issued, and a session with no grant at all — and
// asserts each value selects exactly its own, that a NULL grant_uid matches
// none of them, and that several values OR together.
func TestGrantProvenanceFilter(t *testing.T) {
	t.Parallel()

	f := newObsFixture(t, "provenance")

	approvedGrant := f.requestedGrant(t, false)
	autoGrant := f.requestedGrant(t, true)
	directGrant := f.grant(t, f.dbA.UID)

	approved := f.session(t, f.dbA.UID, &approvedGrant.UID)
	auto := f.session(t, f.dbA.UID, &autoGrant.UID)
	direct := f.session(t, f.dbA.UID, &directGrant.UID)
	// A session that ran without a grant: predates the stamp, or ran with none.
	ungranted := f.session(t, f.dbA.UID, nil)

	stmtApproved := f.statement(t, approved, "SELECT 'approved'")
	stmtAuto := f.statement(t, auto, "SELECT 'auto'")
	stmtDirect := f.statement(t, direct, "SELECT 'direct'")
	f.statement(t, ungranted, "SELECT 'none'")

	cases := []struct {
		values    []GrantProvenance
		wantConns []uuid.UUID
		wantStmts []uuid.UUID
		label     string
	}{
		{
			label:     "approved",
			values:    []GrantProvenance{GrantProvenanceApproved},
			wantConns: []uuid.UUID{approved.UID},
			wantStmts: []uuid.UUID{stmtApproved.UID},
		},
		{
			label:     "auto",
			values:    []GrantProvenance{GrantProvenanceAuto},
			wantConns: []uuid.UUID{auto.UID},
			wantStmts: []uuid.UUID{stmtAuto.UID},
		},
		{
			// Note what `direct` really asserts: "no approval on record". The
			// ungranted session below is NOT direct, which is the whole point
			// of the IS NOT NULL guard.
			label:     "direct",
			values:    []GrantProvenance{GrantProvenanceDirect},
			wantConns: []uuid.UUID{direct.UID},
			wantStmts: []uuid.UUID{stmtDirect.UID},
		},
		{
			label:     "approved,direct",
			values:    []GrantProvenance{GrantProvenanceApproved, GrantProvenanceDirect},
			wantConns: []uuid.UUID{approved.UID, direct.UID},
			wantStmts: []uuid.UUID{stmtApproved.UID, stmtDirect.UID},
		},
		{
			// Every value at once still leaves the ungranted session out: a
			// NULL grant_uid matches no provenance value whatsoever.
			label:     "all three",
			values:    ValidGrantProvenances,
			wantConns: []uuid.UUID{approved.UID, auto.UID, direct.UID},
			wantStmts: []uuid.UUID{stmtApproved.UID, stmtAuto.UID, stmtDirect.UID},
		},
	}

	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			assertSameSet(t, "provenance="+tc.label,
				f.connectionUIDs(t, ConnectionFilter{GrantProvenance: tc.values}), tc.wantConns)
			assertSameSet(t, "provenance="+tc.label+" (queries)",
				f.queryUIDs(t, QueryFilter{GrantProvenance: tc.values}), tc.wantStmts)
		})
	}

	// Sanity: with no provenance filter every session is visible, so the
	// exclusions above are the filter working and not a missing fixture.
	assertSameSet(t, "no provenance filter",
		f.connectionUIDs(t, ConnectionFilter{}),
		[]uuid.UUID{approved.UID, auto.UID, direct.UID, ungranted.UID})
}

// TestApprovalStatusFilter narrows /queries to the statements a human held —
// a per-statement property, and a different axis from grant provenance.
func TestApprovalStatusFilter(t *testing.T) {
	t.Parallel()

	f := newObsFixture(t, "approvalstatus")

	conn := f.session(t, f.dbA.UID, nil)

	pending := ApprovalPending
	approved := ApprovalApproved

	held, err := f.store.CreateQuery(f.ctx, &Query{
		ConnectionID: conn.UID, SQLText: "DELETE FROM t", ApprovalStatus: &pending,
	})
	if err != nil {
		t.Fatalf("CreateQuery() error = %v", err)
	}

	released, err := f.store.CreateQuery(f.ctx, &Query{
		ConnectionID: conn.UID, SQLText: "UPDATE t SET x = 1", ApprovalStatus: &approved,
	})
	if err != nil {
		t.Fatalf("CreateQuery() error = %v", err)
	}

	// A statement no pattern ever matched carries no status at all, and is
	// excluded by any value of the filter.
	f.statement(t, conn, "SELECT 1")

	assertSameSet(t, "approval_status=pending",
		f.queryUIDs(t, QueryFilter{ApprovalStatus: &pending}), []uuid.UUID{held.UID})
	assertSameSet(t, "approval_status=approved",
		f.queryUIDs(t, QueryFilter{ApprovalStatus: &approved}), []uuid.UUID{released.UID})
}

// TestParseGrantProvenance keeps the API layer's validation honest: only the
// three documented values parse, so a typo can be answered with a 400 instead
// of silently becoming "no filter".
func TestParseGrantProvenance(t *testing.T) {
	t.Parallel()

	for _, valid := range ValidGrantProvenances {
		if got, ok := ParseGrantProvenance(string(valid)); !ok || got != valid {
			t.Errorf("ParseGrantProvenance(%q) = %q, %v; want %q, true", valid, got, ok, valid)
		}
	}

	for _, invalid := range []string{"", "APPROVED", "manual", "auto ", "approved,direct"} {
		if _, ok := ParseGrantProvenance(invalid); ok {
			t.Errorf("ParseGrantProvenance(%q) accepted an unknown value", invalid)
		}
	}
}

// TestListLastConnectionPerUser: one row per user, the newest session, and
// absence meaning "never connected" rather than "fell off a page".
func TestListLastConnectionPerUser(t *testing.T) {
	t.Parallel()

	f := newObsFixture(t, "lastconn")

	neverConnected, err := f.store.CreateUser(f.ctx, "obs_never_"+uuid.NewString()[:8], "hash", []string{RoleConnector})
	if err != nil {
		t.Fatalf("CreateUser() error = %v", err)
	}

	older, err := f.store.CreateConnectionAt(f.ctx, f.user.UID, f.dbA.UID, "10.0.0.1",
		time.Now().Add(-48*time.Hour))
	if err != nil {
		t.Fatalf("CreateConnectionAt() error = %v", err)
	}

	newest, err := f.store.CreateConnectionAt(f.ctx, f.user.UID, f.dbB.UID, "10.0.0.2",
		time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("CreateConnectionAt() error = %v", err)
	}

	rows, err := f.store.ListLastConnectionPerUser(f.ctx)
	if err != nil {
		t.Fatalf("ListLastConnectionPerUser() error = %v", err)
	}

	byUser := make(map[uuid.UUID]UserLastConnection, len(rows))
	for _, row := range rows {
		byUser[row.UserID] = row
	}

	got, ok := byUser[f.user.UID]
	if !ok {
		t.Fatalf("user %s missing from the listing", f.user.UID)
	}

	if !got.ConnectedAt.Equal(newest.ConnectedAt) {
		t.Errorf("connected_at = %s, want the newest session's %s (older was %s)",
			got.ConnectedAt, newest.ConnectedAt, older.ConnectedAt)
	}

	if got.Username != f.user.Username {
		t.Errorf("username = %q, want %q", got.Username, f.user.Username)
	}

	if _, present := byUser[neverConnected.UID]; present {
		t.Error("a user who never connected must be absent, not present with a zero time")
	}
}

// TestTouchUserLastLogin stamps the column and leaves updated_at alone —
// updated_at tracks administrative edits to the account, and moving it on every
// sign-in would make it useless for that.
func TestTouchUserLastLogin(t *testing.T) {
	t.Parallel()

	s := setupTestStore(t)
	ctx := context.Background()

	user, err := s.CreateUser(ctx, "obs_login_"+uuid.NewString()[:8], "hash", []string{RoleConnector})
	if err != nil {
		t.Fatalf("CreateUser() error = %v", err)
	}

	if user.LastLoginAt != nil {
		t.Fatalf("a fresh account has LastLoginAt = %v, want nil", user.LastLoginAt)
	}

	before := time.Now().Add(-time.Second)

	if err := s.TouchUserLastLogin(ctx, user.UID); err != nil {
		t.Fatalf("TouchUserLastLogin() error = %v", err)
	}

	stamped, err := s.GetUserByUID(ctx, user.UID)
	if err != nil {
		t.Fatalf("GetUserByUID() error = %v", err)
	}

	if stamped.LastLoginAt == nil {
		t.Fatal("LastLoginAt is still nil after TouchUserLastLogin")
	}

	if stamped.LastLoginAt.Before(before) {
		t.Errorf("LastLoginAt = %s, want at or after %s", stamped.LastLoginAt, before)
	}

	if !stamped.UpdatedAt.Equal(user.UpdatedAt) {
		t.Errorf("updated_at moved from %s to %s; a sign-in must not count as an edit",
			user.UpdatedAt, stamped.UpdatedAt)
	}

	// A UID that names no row is not an error: the caller has already
	// authenticated somebody and must never be failed over bookkeeping.
	if err := s.TouchUserLastLogin(ctx, uuid.New()); err != nil {
		t.Errorf("TouchUserLastLogin(unknown uid) error = %v, want nil", err)
	}
}

// explainPlan renders the plan PostgreSQL would use for a built listing query.
//
// enable_seqscan is turned off for the statement so the planner has to show
// what index-ordered plan is *available*: on a test table of a few hundred
// rows a sequential scan plus a sort is genuinely cheapest, so leaving it on
// would prove nothing about how the query behaves against a real store. What
// the assertion below cares about is that an index-ordered plan exists at all —
// if `ORDER BY uid DESC LIMIT n` could only ever be answered by sorting, no
// amount of data would change that.
func explainPlan(t *testing.T, s *Store, sql string) string {
	t.Helper()

	ctx := context.Background()

	conn, err := s.db.Conn(ctx)
	if err != nil {
		t.Fatalf("acquire connection: %v", err)
	}
	defer func() { _ = conn.Close() }()

	if _, err := conn.ExecContext(ctx, "SET enable_seqscan = off"); err != nil {
		t.Fatalf("disable seqscan: %v", err)
	}

	rows, err := conn.QueryContext(ctx, "EXPLAIN "+sql)
	if err != nil {
		t.Fatalf("EXPLAIN failed: %v\nSQL: %s", err, sql)
	}
	defer func() { _ = rows.Close() }()

	var plan string

	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan plan line: %v", err)
		}

		plan += line + "\n"
	}

	if err := rows.Err(); err != nil {
		t.Fatalf("read plan: %v", err)
	}

	return plan
}

// TestFilteredPaginationUsesAnIndex is the EXPLAIN check the spec asks for: a
// *filtered* `ORDER BY uid DESC LIMIT n` page must still be answered by walking
// an index backwards, not by sorting the whole table.
//
// It matters because every filter added here is a WHERE clause the planner
// could decide to satisfy by scanning and sorting — which is fine at a hundred
// rows and ruinous at ten million, exactly the size at which someone reaches
// for these filters.
func TestFilteredPaginationUsesAnIndex(t *testing.T) {
	t.Parallel()

	f := newObsFixture(t, "explain")

	grant := f.grant(t, f.dbA.UID)

	for range 200 {
		conn := f.session(t, f.dbA.UID, &grant.UID)
		f.statement(t, conn, "SELECT 1")
	}

	if err := f.store.AddServerToGroup(f.ctx, f.group.UID, f.dbA.UID); err != nil {
		t.Fatalf("AddServerToGroup() error = %v", err)
	}

	cursor := uuid.Must(uuid.NewV7())

	connFilter := ConnectionFilter{
		UserID:             &f.user.UID,
		ServerGroupUID:     &f.group.UID,
		GrantUID:           &grant.UID,
		GrantDefinitionUID: &f.def.UID,
		GrantProvenance:    []GrantProvenance{GrantProvenanceDirect},
		BeforeUID:          &cursor,
		Limit:              50,
	}

	var conns []Connection

	connQuery, err := f.store.buildListConnectionsQuery(f.ctx, &conns, connFilter)
	if err != nil {
		t.Fatalf("buildListConnectionsQuery() error = %v", err)
	}

	plan := explainPlan(t, f.store, connQuery.String())
	if strings.Contains(plan, "Sort") {
		t.Errorf("the filtered connections page sorts instead of walking an index:\n%s", plan)
	}

	if !strings.Contains(plan, "Index Scan Backward") && !strings.Contains(plan, "connections_pkey") {
		t.Errorf("the filtered connections page does not reach the uid index:\n%s", plan)
	}

	pending := ApprovalPending
	queryFilter := QueryFilter{
		UserID:          &f.user.UID,
		ServerGroupUID:  &f.group.UID,
		GrantUID:        &grant.UID,
		GrantProvenance: []GrantProvenance{GrantProvenanceDirect},
		ApprovalStatus:  &pending,
		BeforeUID:       &cursor,
		Limit:           50,
	}

	var queries []Query

	queryQuery, err := f.store.buildListQueriesQuery(f.ctx, &queries, queryFilter)
	if err != nil {
		t.Fatalf("buildListQueriesQuery() error = %v", err)
	}

	// The queries listing joins connections, so a hash/merge join may legally
	// introduce a sort of the *inner* side. What must not happen is the
	// pagination itself being answered by sorting `queries`, which is what a
	// missing index scan on q.uid would look like.
	plan = explainPlan(t, f.store, queryQuery.String())
	if !strings.Contains(plan, "Index Scan Backward") && !strings.Contains(plan, "queries_pkey") {
		t.Errorf("the filtered queries page does not reach the uid index:\n%s", plan)
	}
}
