package store

import (
	"context"
	"strconv"
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

// bulkSessions seeds `total` sessions in one pass, `live(i)` deciding which of
// them are still open.
//
// The rows are inserted directly instead of going through CreateConnection and
// CloseConnection, which is a deliberate exception made for the planner tests
// alone. Those two writers also append this session's audit entries and seal
// its query chain — milliseconds per session, which is nothing at four hundred
// rows and half a minute at the thousands a plan has to be judged on. Nothing
// under test here reads that evidence: what is being judged is the access path
// into `connections`, and the rows still go through the bun model, so they
// cannot drift from the real schema. That the ActiveOnly predicate selects the
// right sessions when written by the real writers is
// TestActiveOnlyFilterIsAppliedInTheDatabase's job, not this one's.
func (f *obsFixture) bulkSessions(t *testing.T, total int, live func(i int) bool) {
	t.Helper()

	const chunk = 1000

	openedAt := time.Now()
	closedAt := openedAt.Add(time.Minute)
	rows := make([]Connection, 0, chunk)

	flush := func() {
		if len(rows) == 0 {
			return
		}

		if _, err := f.store.db.NewInsert().Model(&rows).Exec(f.ctx); err != nil {
			t.Fatalf("seed connections: %v", err)
		}

		rows = rows[:0]
	}

	for i := range total {
		// UUIDv7, as CreateConnection mints them: the listing orders on uid, so
		// a fixture whose uids were not time-ordered would order differently
		// from the real table.
		row := Connection{
			UID:            newUIDv7(),
			UserID:         f.user.UID,
			DatabaseID:     f.dbA.UID,
			SourceIP:       "10.0.0.1",
			ConnectedAt:    openedAt,
			LastActivityAt: openedAt,
		}

		if !live(i) {
			disconnectedAt := closedAt
			row.DisconnectedAt = &disconnectedAt
		}

		rows = append(rows, row)

		if len(rows) == chunk {
			flush()
		}
	}

	flush()
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

// TestActiveOnlyFilterIsAppliedInTheDatabase is the regression the ActiveOnly
// filter exists for.
//
// "Active only" used to narrow the *already fetched* page in the browser, so a
// live session that had fallen off the current page simply did not exist as
// far as the operator was concerned — the table went empty and read as
// "nothing is running".
//
// The fixture is that situation exactly: one live session opened first, then
// more closed sessions than fit in a page, all of them newer. A page of the
// unfiltered list (uid DESC) therefore contains no live session at all, which
// is asserted rather than assumed — without that assertion the test would
// still pass against a client-side narrowing.
func TestActiveOnlyFilterIsAppliedInTheDatabase(t *testing.T) {
	t.Parallel()

	f := newObsFixture(t, "activeonly")

	const pageSize = 5

	// Opened first, so its UUIDv7 sorts oldest and every closed session below
	// is reached before it.
	live := f.session(t, f.dbA.UID, nil)

	closed := make([]uuid.UUID, 0, pageSize+3)

	for range pageSize + 3 {
		conn := f.session(t, f.dbA.UID, nil)
		if err := f.store.CloseConnection(f.ctx, conn.UID); err != nil {
			t.Fatalf("CloseConnection() error = %v", err)
		}

		closed = append(closed, conn.UID)
	}

	page := f.connectionUIDs(t, ConnectionFilter{Limit: pageSize})
	if len(page) != pageSize {
		t.Fatalf("unfiltered page returned %d rows, want %d", len(page), pageSize)
	}

	for _, uid := range page {
		if uid == live.UID {
			t.Fatal("the live session is on the first unfiltered page, so this fixture " +
				"cannot distinguish a server-side filter from a client-side one")
		}
	}

	// Server-side, the same page size finds the session the browser could not.
	assertSameSet(t, "active only",
		f.connectionUIDs(t, ConnectionFilter{ActiveOnly: true, Limit: pageSize}),
		[]uuid.UUID{live.UID})

	// False is "unfiltered", not "closed only": every session is still listed.
	assertSameSet(t, "active only off",
		f.connectionUIDs(t, ConnectionFilter{ActiveOnly: false}),
		append([]uuid.UUID{live.UID}, closed...))

	// And it composes with AND like every other filter — a server the live
	// session never touched intersects it away.
	assertSameSet(t, "active only ∩ dbB",
		f.connectionUIDs(t, ConnectionFilter{ActiveOnly: true, DatabaseID: &f.dbB.UID}), nil)
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
			t.Parallel()

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
// enable_seqscan is turned off for the connection, and that is load-bearing
// rather than a convenience: the fixture below holds a few hundred rows, and at
// that size a sequential scan really is cheapest, so a planner left to its own
// devices answers "seq scan, then sort" and tells us nothing about how the
// query behaves against a store with millions of sessions. Turning it off makes
// the planner reveal the index-ordered plan it *would* choose at scale — which
// is the thing under test. (Verified: with seqscan enabled, the connections
// table is sequentially scanned for the widest filter shape, and every
// assertion below becomes vacuous.)
//
// The small lookup tables the subqueries touch — grant_requests,
// access_grants, grant_definitions — are irrelevant here; only the access path
// into connections/queries is being judged.
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

	// The connection goes back to a shared pool, so put the planner setting
	// back before it does. Harmless today — every test owns its own database —
	// but a session-level GUC leaking into someone else's query is the kind of
	// thing that is impossible to debug once it does bite.
	defer func() {
		if _, err := conn.ExecContext(ctx, "RESET enable_seqscan"); err != nil {
			t.Errorf("reset enable_seqscan: %v", err)
		}
	}()

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

// sortNodeIndex returns the position of the first Sort *node* in a plan, or -1.
//
// It matches the node line rather than the string "Sort", which also appears on
// the "Sort Key:" and "Sort Method:" detail lines beneath it.
func sortNodeIndex(lines []string) int {
	for i, line := range lines {
		node := strings.TrimSpace(line)
		node = strings.TrimPrefix(node, "->  ")

		if strings.HasPrefix(node, "Sort  (") {
			return i
		}
	}

	return -1
}

// leadingSpaces is a plan line's indentation, which is how EXPLAIN's text
// format encodes tree depth.
func leadingSpaces(line string) int {
	return len(line) - len(strings.TrimLeft(line, " "))
}

// subtreeOf returns the lines belonging to the node at lines[at]: everything
// after it indented more deeply, stopping at the first line that is not.
//
// This is the whole reason the checks below are not simple substring searches.
// EXPLAIN prints depth-first, so "appears after the Sort line" and "is an input
// to the Sort" are different claims — an InitPlan, a SubPlan or an entirely
// separate sibling subtree all print after it too.
func subtreeOf(lines []string, at int) []string {
	indent := leadingSpaces(lines[at])

	for i := at + 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "" {
			continue
		}

		if leadingSpaces(lines[i]) <= indent {
			return lines[at+1 : i]
		}
	}

	return lines[at+1:]
}

// rowEstimate reads the `rows=N` the planner put on a node line, or -1.
func rowEstimate(line string) int {
	at := strings.Index(line, "rows=")
	if at < 0 {
		return -1
	}

	rest := line[at+len("rows="):]

	end := 0
	for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
		end++
	}

	if end == 0 {
		return -1
	}

	n, err := strconv.Atoi(rest[:end])
	if err != nil {
		return -1
	}

	return n
}

// nodeAccessing returns the index of the plan node that reads `table`, or -1.
func nodeAccessing(lines []string, table string) int {
	for i, line := range lines {
		if strings.Contains(line, " on "+table+" ") {
			return i
		}
	}

	return -1
}

// assertPaginationNeverSortsTheTable is the invariant every filtered page must
// satisfy, whatever shape its filters take: the LIMIT is never paid for by
// sorting a large, unrestricted row set.
//
// Exactly two plan shapes satisfy it, and both are correct:
//
//  1. **No Sort at all** — the plan walks the uid index backwards and stops
//     after n rows. This is what the spec's "still uses an index rather than
//     sorting the table" describes, and it is what every ordinary filter shape
//     gets (see the table in TestFilteredPaginationUsesAnIndex).
//
//  2. **A small Sort over an index-restricted read of the paginated table.**
//     This is what a filter combination the planner estimates will match a
//     handful of rows gets, and it is the *better* plan for that case: when
//     only one or two rows can match, fetching them and ordering them beats
//     walking the uid index until a match happens to turn up. Forbidding it
//     would be demanding a worse plan.
//
// Shape 2 is pinned by two independent teeth, because either alone is too
// weak:
//
//   - **The restriction must be on `table` itself, inside the Sort's own
//     subtree.** Scanning the whole plan text for any `Index Cond:` is close to
//     a rubber stamp here: the provenance filter is a correlated EXISTS over
//     grant_requests that prints its own `Index Cond: (resulting_grant_id = …)`,
//     so a plan that sequentially scanned connections and sorted all of it
//     would still have found one. What has to be true is that the read of the
//     paginated table is itself index-restricted.
//
//   - **The Sort's own row estimate must be bounded.** "Index-restricted" with
//     no cardinality bound still permits "sort all ten million sessions of one
//     user to serve a fifty-row page", which is exactly the failure the spec
//     asks to prevent. maxRows is normally the page size: if the planner is
//     going to sort at all, it must be sorting no more than a page's worth.
//
// There is deliberately no "this index is partial on the filter's predicate, so
// scanning all of it is fine" allowance. `ActiveOnly` used to need one — its
// only usable index was idx_connections_instance_id_open, partial on
// `disconnected_at IS NULL`, which restricts by *existing* and so prints no
// Index Cond for the tooth to read. idx_connections_open_uid (20260823000000)
// carries `uid DESC` as its key, so that shape now streams with no Sort at all
// and the allowance has no remaining user. Reintroducing one would be a way to
// wave through exactly the plan this test exists to catch.
func assertPaginationNeverSortsTheTable(
	t *testing.T,
	label, plan, table string,
	maxRows int,
) {
	t.Helper()

	lines := strings.Split(strings.TrimRight(plan, "\n"), "\n")

	sortAt := sortNodeIndex(lines)
	if sortAt < 0 {
		return // shape 1: the index supplied the order.
	}

	subtree := subtreeOf(lines, sortAt)

	accessAt := nodeAccessing(subtree, table)
	if accessAt < 0 {
		t.Errorf("%s sorts, but no read of %s appears beneath the Sort, so the sorted row "+
			"set cannot be shown to be restricted:\n%s", label, table, plan)

		return
	}

	access := subtree[accessAt]
	if strings.Contains(access, "Seq Scan on "+table+" ") {
		t.Errorf("%s sorts a sequential scan of %s — the whole table is being ordered to "+
			"serve one page:\n%s", label, table, plan)

		return
	}

	restricted := false

	for _, line := range subtreeOf(subtree, accessAt) {
		if strings.Contains(line, "Index Cond:") || strings.Contains(line, "Recheck Cond:") {
			restricted = true

			break
		}
	}

	if !restricted {
		t.Errorf("%s sorts a read of %s that no index restricted (%s):\n%s",
			label, table, strings.TrimSpace(access), plan)
	}

	if rows := rowEstimate(lines[sortAt]); rows < 0 || rows > maxRows {
		t.Errorf("%s sorts an estimated %d rows of %s, want at most %d — a sort is only "+
			"acceptable here when it is bounded by roughly a page:\n%s",
			label, rows, table, maxRows, plan)
	}
}

// assertOrderedIndexWalk is the stronger property: no Sort whatsoever, and the
// walk that supplies the order happens on the table being paginated.
//
// The index is deliberately not pinned by name — the widest /queries shape is
// answered from queries_pending_approval_idx rather than queries_pkey, and
// either is fine: what matters is that *some* index on that table returns the
// rows in uid DESC order so the LIMIT can stop early. "Scan using" rather than
// "Index Scan using" for the same reason: an Index *Only* Scan is equally good
// and must not read as a failure.
//
// Direction is not pinned either, because it is a property of the index rather
// than of the plan: an index keyed `uid` ascending (connections_pkey, and the
// queries indexes) is read *backwards* to produce uid DESC, while one keyed
// `uid DESC` (idx_connections_open_uid) is read forwards to produce the same
// order. Both stream and stop at the LIMIT, which is the whole claim. Nothing
// weaker slips through: the ORDER BY is not optional, so a plan with no Sort
// node has no other way to satisfy it.
//
// Pinning the table, though, is essential: an ordered walk of the *joined*
// table would satisfy a name-blind check while the paginated one still got
// sorted.
func assertOrderedIndexWalk(t *testing.T, label, plan, table string) {
	t.Helper()

	lines := strings.Split(strings.TrimRight(plan, "\n"), "\n")

	if at := sortNodeIndex(lines); at >= 0 {
		t.Errorf("%s sorts instead of walking an index in uid order:\n%s", label, plan)

		return
	}

	for _, line := range lines {
		walk := strings.Contains(line, "Scan using ") || strings.Contains(line, "Scan Backward using ")
		if walk && strings.Contains(line, " on "+table+" ") {
			return
		}
	}

	t.Errorf("%s never walks an index on %s in order, so ORDER BY uid DESC is not "+
		"served by the index:\n%s", label, table, plan)
}

// TestFilteredPaginationUsesAnIndex is the EXPLAIN check the spec asks for:
// a *filtered* `ORDER BY uid DESC LIMIT n` page must not be answered by sorting
// the table.
//
// It matters because every filter added by this feature is a WHERE clause the
// planner could choose to satisfy by scanning and ordering — fine at four
// hundred rows, ruinous at ten million, which is exactly the size at which
// somebody reaches for these filters.
//
// The expectation is deliberately *not* a blanket "no Sort node anywhere". That
// was the first draft, and it was wrong: measured against a real planner, the
// widest filter combination on /connections (user + server group + grant
// instance + grant policy + provenance, all at once) is estimated to match
// about one row, and for that estimate PostgreSQL correctly prefers to fetch
// the index-restricted matches and order them rather than walk the uid index
// hunting for one. Demanding "no Sort" there would be demanding a worse plan.
// So each shape below is checked against the invariant that actually matters
// (assertPaginationNeverSortsTheTable), and the shapes that genuinely can
// stream from the index are additionally pinned to that stronger property.
func TestFilteredPaginationUsesAnIndex(t *testing.T) {
	t.Parallel()

	f := newObsFixture(t, "explain")

	grant := f.grant(t, f.dbA.UID)

	for range 400 {
		conn := f.session(t, f.dbA.UID, &grant.UID)
		f.statement(t, conn, "SELECT 1")
	}

	if err := f.store.AddServerToGroup(f.ctx, f.group.UID, f.dbA.UID); err != nil {
		t.Fatalf("AddServerToGroup() error = %v", err)
	}

	// Without stats the planner works off hard-coded defaults and its shape
	// choices say nothing about the data.
	if _, err := f.store.db.ExecContext(f.ctx, "ANALYZE"); err != nil {
		t.Fatalf("ANALYZE: %v", err)
	}

	cursor := uuid.Must(uuid.NewV7())
	pending := ApprovalPending

	// The page size every shape below asks for, and the bound a Sort must stay
	// under: sorting more than a page's worth to serve a page is the failure.
	const pageSize = 50

	connectionShapes := []struct {
		name string
		// streams is true when this shape must be answered by an ordered index
		// walk with no Sort at all.
		streams bool
		filter  ConnectionFilter
	}{
		{
			name: "cursor only", streams: true,
			filter: ConnectionFilter{BeforeUID: &cursor, Limit: 50},
		},
		{
			name: "user + cursor", streams: true,
			filter: ConnectionFilter{UserID: &f.user.UID, BeforeUID: &cursor, Limit: 50},
		},
		{
			// The server-group filter is an IN over live membership and the
			// provenance filter an anti-join; neither may cost the ordered walk.
			name: "server group + provenance", streams: true,
			filter: ConnectionFilter{
				UserID: &f.user.UID, ServerGroupUID: &f.group.UID,
				GrantProvenance: []GrantProvenance{GrantProvenanceDirect},
				BeforeUID:       &cursor, Limit: 50,
			},
		},
		{
			// The lineage subquery is correlated through grant_uid; it must not
			// force a sort either.
			name: "grant policy + provenance", streams: true,
			filter: ConnectionFilter{
				UserID: &f.user.UID, GrantDefinitionUID: &f.def.UID,
				GrantProvenance: []GrantProvenance{GrantProvenanceDirect},
				BeforeUID:       &cursor, Limit: 50,
			},
		},
		{
			// The deep-link from a connection row: "every session under this
			// grant". idx_connections_grant_uid carries uid DESC as its second
			// column precisely so this one can stream.
			name: "grant instance", streams: true,
			filter: ConnectionFilter{
				UserID: &f.user.UID, GrantUID: &grant.UID,
				BeforeUID: &cursor, Limit: 50,
			},
		},
		{
			// Every filter at once. The planner estimates ~1 matching row and
			// sorts, which is correct — see the doc comment. Only the invariant
			// is required here.
			name: "every filter at once", streams: false,
			filter: ConnectionFilter{
				UserID: &f.user.UID, ServerGroupUID: &f.group.UID,
				GrantUID: &grant.UID, GrantDefinitionUID: &f.def.UID,
				GrantProvenance: []GrantProvenance{GrantProvenanceDirect},
				BeforeUID:       &cursor, Limit: 50,
			},
		},
	}

	for _, shape := range connectionShapes {
		var conns []Connection

		q, err := f.store.buildListConnectionsQuery(f.ctx, &conns, shape.filter)
		if err != nil {
			t.Fatalf("buildListConnectionsQuery(%s) error = %v", shape.name, err)
		}

		label := "connections / " + shape.name
		plan := explainPlan(t, f.store, q.String())

		assertPaginationNeverSortsTheTable(t, label, plan, "connections", pageSize)

		if shape.streams {
			assertOrderedIndexWalk(t, label, plan, "connections")
		}
	}

	// The /queries half, held to the *same* bar rather than a weaker one — an
	// earlier draft only checked that it reached some index, which would have
	// passed on a plan that sorted every statement in the store. Measured, it
	// streams for every shape, the widest one included: the join to connections
	// is driven from the queries uid index, so the ordering survives.
	queryShapes := []struct {
		name   string
		filter QueryFilter
	}{
		{
			name:   "cursor only",
			filter: QueryFilter{BeforeUID: &cursor, Limit: 50},
		},
		{
			name: "server group + provenance",
			filter: QueryFilter{
				UserID: &f.user.UID, ServerGroupUID: &f.group.UID,
				GrantProvenance: []GrantProvenance{GrantProvenanceDirect},
				BeforeUID:       &cursor, Limit: 50,
			},
		},
		{
			name: "grant instance",
			filter: QueryFilter{
				UserID: &f.user.UID, GrantUID: &grant.UID,
				BeforeUID: &cursor, Limit: 50,
			},
		},
		{
			name: "every filter at once",
			filter: QueryFilter{
				UserID: &f.user.UID, ServerGroupUID: &f.group.UID,
				GrantUID: &grant.UID, GrantDefinitionUID: &f.def.UID,
				GrantProvenance: []GrantProvenance{GrantProvenanceDirect},
				ApprovalStatus:  &pending,
				BeforeUID:       &cursor, Limit: 50,
			},
		},
	}

	for _, shape := range queryShapes {
		var queries []Query

		q, err := f.store.buildListQueriesQuery(f.ctx, &queries, shape.filter)
		if err != nil {
			t.Fatalf("buildListQueriesQuery(%s) error = %v", shape.name, err)
		}

		label := "queries / " + shape.name
		plan := explainPlan(t, f.store, q.String())

		assertPaginationNeverSortsTheTable(t, label, plan, "queries", pageSize)
		assertOrderedIndexWalk(t, label, plan, "queries")
	}
}

// TestGrantInstanceIndexServesTheOrdering pins the reason
// idx_connections_grant_uid is a (grant_uid, uid DESC) composite rather than
// the single-column index the earlier draft of the migration created.
//
// The deep-linked "sessions under this grant" page is always the same shape —
// `WHERE grant_uid = ? ORDER BY uid DESC LIMIT n` — and a single-column index
// answers only the equality, leaving PostgreSQL to sort every session of that
// grant on every page. The trailing column makes the index supply the ordering
// as well. This asserts the index exists with both columns in the right
// direction, which is what a future "tidy up the indexes" pass would otherwise
// silently undo.
func TestGrantInstanceIndexServesTheOrdering(t *testing.T) {
	t.Parallel()

	s := setupTestStore(t)
	ctx := context.Background()

	var indexDef string

	err := s.db.NewRaw(
		"SELECT indexdef FROM pg_indexes WHERE tablename = 'connections' AND indexname = ?",
		"idx_connections_grant_uid",
	).Scan(ctx, &indexDef)
	if err != nil {
		t.Fatalf("read index definition: %v", err)
	}

	// Either direction on the trailing column serves `ORDER BY uid DESC` — a
	// plain (grant_uid, uid) is walked backwards just as happily — so both are
	// accepted. What is *not* accepted is the single-column index this replaced,
	// which can only answer the equality and leaves the ordering to a sort. That
	// is the revert this guards against, and the EXPLAIN test above would not
	// catch it: all 400 fixture sessions share one grant_uid, so even a
	// single-column index lets the "grant instance" shape stream from
	// connections_pkey with no Sort at all.
	if !strings.Contains(indexDef, "(grant_uid, uid DESC)") &&
		!strings.Contains(indexDef, "(grant_uid, uid)") {
		t.Errorf("idx_connections_grant_uid = %q; want a (grant_uid, uid) composite so the "+
			"deep-linked per-grant page streams instead of sorting that grant's whole history", indexDef)
	}

	if !strings.Contains(indexDef, "WHERE (grant_uid IS NOT NULL)") {
		t.Errorf("idx_connections_grant_uid = %q; want it partial on grant_uid IS NOT NULL — "+
			"sessions with no grant match no grant filter and do not belong in it", indexDef)
	}
}

// TestOpenSessionsIndexRestrictsAndOrders pins both halves of
// idx_connections_open_uid, because the EXPLAIN test above only notices one of
// them.
//
// The key is what removes the Sort, and losing it is caught. The *predicate* is
// not: an index over `(uid DESC)` with no WHERE clause would let the active
// page stream just as happily — walking every session ever opened and
// discarding the closed ones as it went — so every EXPLAIN assertion would
// still pass while the plan quietly became unbounded again on an instance where
// live sessions are rare. That is precisely the shape this index exists to
// avoid, so it is asserted directly.
func TestOpenSessionsIndexRestrictsAndOrders(t *testing.T) {
	t.Parallel()

	s := setupTestStore(t)
	ctx := context.Background()

	var indexDef string

	err := s.db.NewRaw(
		"SELECT indexdef FROM pg_indexes WHERE tablename = 'connections' AND indexname = ?",
		"idx_connections_open_uid",
	).Scan(ctx, &indexDef)
	if err != nil {
		t.Fatalf("read index definition: %v", err)
	}

	// Either direction serves `ORDER BY uid DESC` — an ascending index is
	// simply walked backwards — so both are accepted.
	if !strings.Contains(indexDef, "(uid DESC)") && !strings.Contains(indexDef, "(uid)") {
		t.Errorf("idx_connections_open_uid = %q; want it keyed on uid so a page of live "+
			"sessions streams in uid order instead of being sorted", indexDef)
	}

	if !strings.Contains(indexDef, "WHERE (disconnected_at IS NULL)") {
		t.Errorf("idx_connections_open_uid = %q; want it partial on disconnected_at IS NULL — "+
			"without the predicate it indexes every session ever opened and the ?active=true "+
			"page walks closed sessions looking for live ones", indexDef)
	}
}

// TestActiveOnlyPaginationUsesAnIndex holds the "still open" predicate to the
// strongest property in this file: a page of live sessions must stream out of
// an index already in uid order, never be assembled by reading every live
// session and sorting the result.
//
// It gets its own fixture rather than another row in
// TestFilteredPaginationUsesAnIndex's table because it is the only shape whose
// *data* has to be arranged a particular way, and it runs over two arrangements
// because the sort it forbids costs differently in each:
//
//   - **a handful live** — the everyday distribution: a few hundred sessions of
//     history, four still running. This is what the plan used to be measured on,
//     and it sorted: with no index ordering the live sessions, PostgreSQL read
//     the whole of idx_connections_instance_id_open (the crash reconcile's
//     index, partial on `disconnected_at IS NULL`) and ordered the four rows it
//     found. Cheap, which is precisely why it went unnoticed.
//
//   - **hundreds live** — the busy instance that sort actually hurts on, where
//     "read every live session and order it" means hundreds of rows read and
//     sorted to return fifty.
//
// Neither arrangement is anywhere near all-open on purpose: with every session
// live the predicate matches every row and any plan looks fine.
//
// Note which index does *not* do the work. 20260803010000's
// idx_connections_disconnected_at is partial on `disconnected_at IS NOT NULL` —
// the retention sweep's half of the column — and cannot serve this predicate at
// all. What serves it is idx_connections_open_uid (20260823000000): partial on
// `disconnected_at IS NULL` so it holds exactly the live sessions, keyed on
// `uid DESC` so walking it *is* the ordering the listing asks for.
func TestActiveOnlyPaginationUsesAnIndex(t *testing.T) {
	t.Parallel()

	const (
		pageSize = 50
		history  = 4000
	)

	arrangements := []struct {
		name string
		// live reports whether session i of the history is still open. Both
		// spread the live sessions evenly through the history rather than
		// clustering them at one end: the planner has no cross-column stats to
		// see such a cluster with, so clustering would only make the fixture
		// unlike the deployment it stands for.
		live func(i int) bool
	}{
		{name: "a handful live", live: func(i int) bool { return i%1000 == 0 }}, // 4 of 4000
		{name: "hundreds live", live: func(i int) bool { return i%13 == 0 }},    // ~308 of 4000
	}

	for _, arrangement := range arrangements {
		t.Run(arrangement.name, func(t *testing.T) {
			t.Parallel()

			f := newObsFixture(t, "activeplan_"+uuid.NewString()[:8])

			f.bulkSessions(t, history, arrangement.live)

			if _, err := f.store.db.ExecContext(f.ctx, "ANALYZE"); err != nil {
				t.Fatalf("ANALYZE: %v", err)
			}

			cursor := uuid.Must(uuid.NewV7())

			shapes := []struct {
				name   string
				filter ConnectionFilter
			}{
				{
					name:   "active only",
					filter: ConnectionFilter{ActiveOnly: true, BeforeUID: &cursor, Limit: pageSize},
				},
				{
					name: "active only + user",
					filter: ConnectionFilter{
						ActiveOnly: true, UserID: &f.user.UID,
						BeforeUID: &cursor, Limit: pageSize,
					},
				},
			}

			for _, shape := range shapes {
				var conns []Connection

				q, err := f.store.buildListConnectionsQuery(f.ctx, &conns, shape.filter)
				if err != nil {
					t.Fatalf("buildListConnectionsQuery(%s) error = %v", shape.name, err)
				}

				label := "connections / " + arrangement.name + " / " + shape.name
				plan := explainPlan(t, f.store, q.String())

				assertPaginationNeverSortsTheTable(t, label, plan, "connections", pageSize)
				assertOrderedIndexWalk(t, label, plan, "connections")
			}
		})
	}
}

// TestPlanSubtreeScoping pins the hole that made the earlier version of
// assertPaginationNeverSortsTheTable a rubber stamp.
//
// EXPLAIN prints depth-first, so "later in the text" is not "beneath this
// node". The plan below is the false pass in its purest form: connections is
// sequentially scanned and the whole 400-row table is sorted to serve a 50-row
// page — an unambiguous regression — while a SubPlan that is a *sibling* of
// the Sort carries an `Index Cond:` further down the text. A check that
// scanned every line after the Sort found that Index Cond and passed.
//
// This runs against synthetic text on purpose: it needs no container, and it
// keeps working when a future PostgreSQL reshuffles the plan the real query
// gets.
func TestPlanSubtreeScoping(t *testing.T) {
	t.Parallel()

	plan := strings.Join([]string{
		"Limit  (cost=100.00..100.01 rows=50 width=135)",
		"  ->  Sort  (cost=99.00..99.50 rows=400 width=135)",
		"        Sort Key: c.uid DESC",
		"        ->  Seq Scan on connections c  (cost=0.00..80.00 rows=400 width=110)",
		"              Filter: (user_id = 'x'::uuid)",
		"  SubPlan 1",
		"    ->  Index Only Scan using idx_grant_requests_resulting_grant on grant_requests fgr  (cost=0.12..8.14 rows=1 width=16)",
		"          Index Cond: (resulting_grant_id = c.grant_uid)",
	}, "\n")

	lines := strings.Split(plan, "\n")

	sortAt := sortNodeIndex(lines)
	if sortAt != 1 {
		t.Fatalf("sortNodeIndex = %d, want 1", sortAt)
	}

	subtree := subtreeOf(lines, sortAt)

	// The sibling SubPlan, and the Index Cond that made this plan look
	// restricted, must be outside the Sort's subtree.
	for _, line := range subtree {
		if strings.Contains(line, "Index Cond:") {
			t.Errorf("the Sort's subtree wrongly includes a sibling SubPlan's Index Cond: %q", line)
		}

		if strings.Contains(line, "SubPlan") {
			t.Errorf("the Sort's subtree wrongly includes a sibling SubPlan: %q", line)
		}
	}

	// The read of the paginated table inside that subtree is the Seq Scan,
	// which is what makes this plan a regression.
	accessAt := nodeAccessing(subtree, "connections")
	if accessAt < 0 {
		t.Fatalf("nodeAccessing(connections) found nothing in:\n%s", strings.Join(subtree, "\n"))
	}

	if !strings.Contains(subtree[accessAt], "Seq Scan on connections c") {
		t.Errorf("nodeAccessing picked %q, want the Seq Scan on connections", subtree[accessAt])
	}

	// And nothing restricts it.
	for _, line := range subtreeOf(subtree, accessAt) {
		if strings.Contains(line, "Index Cond:") || strings.Contains(line, "Recheck Cond:") {
			t.Errorf("the Seq Scan's subtree wrongly looks index-restricted: %q", line)
		}
	}

	// The cardinality tooth reads the Sort's own estimate, not the Limit's.
	if got := rowEstimate(lines[sortAt]); got != 400 {
		t.Errorf("rowEstimate(sort line) = %d, want 400", got)
	}

	if got := rowEstimate("  ->  Foo  (cost=1.00..2.00 width=8)"); got != -1 {
		t.Errorf("rowEstimate(no rows=) = %d, want -1", got)
	}
}
