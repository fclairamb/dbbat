package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// pendingByUID indexes a poll result so a test can assert on one session
// without depending on the order the UNION happens to return.
func pendingByUID(pending []PendingTermination) map[uuid.UUID]PendingTermination {
	byUID := make(map[uuid.UUID]PendingTermination, len(pending))
	for _, p := range pending {
		byUID[p.ConnectionUID] = p
	}

	return byUID
}

// TestPendingTerminations_RequestedConnection is the first arm: a session this
// run owns, with a termination requested on it, comes back with the requesting
// admin's name and reason attached.
func TestPendingTerminations_RequestedConnection(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := setupTestStore(t)

	user, db := createTestUserAndDatabase(t, ctx, s, "pendreq")

	admin, err := s.CreateUser(ctx, "pendadmin", "hash", []string{RoleAdmin})
	if err != nil {
		t.Fatalf("CreateUser() error = %v", err)
	}

	conn, err := s.CreateConnection(ctx, user.UID, db.UID, "10.0.0.1")
	if err != nil {
		t.Fatalf("CreateConnection() error = %v", err)
	}

	if _, err := s.RequestConnectionTermination(ctx, conn.UID, admin.UID, "runaway report"); err != nil {
		t.Fatalf("RequestConnectionTermination() error = %v", err)
	}

	pending, err := s.PendingTerminations(ctx)
	if err != nil {
		t.Fatalf("PendingTerminations() error = %v", err)
	}

	got, ok := pendingByUID(pending)[conn.UID]
	if !ok {
		t.Fatalf("PendingTerminations() did not return the requested connection, got %+v", pending)
	}

	if got.Reason != TerminationAdminTerminated {
		t.Errorf("reason = %q, want %q", got.Reason, TerminationAdminTerminated)
	}

	if got.RequestedBy != "pendadmin" {
		t.Errorf("requested_by = %q, want %q", got.RequestedBy, "pendadmin")
	}

	if got.Detail != "runaway report" {
		t.Errorf("detail = %q, want %q", got.Detail, "runaway report")
	}
}

// TestPendingTerminations_OnlyOwnRun is the whole point of scoping the poll by
// run id: a process must only tear down the sessions it is actually serving.
func TestPendingTerminations_OnlyOwnRun(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := setupTestStore(t)

	user, db := createTestUserAndDatabase(t, ctx, s, "pendrun")

	admin, err := s.CreateUser(ctx, "pendrunadmin", "hash", []string{RoleAdmin})
	if err != nil {
		t.Fatalf("CreateUser() error = %v", err)
	}

	conn, err := s.CreateConnection(ctx, user.UID, db.UID, "10.0.0.2")
	if err != nil {
		t.Fatalf("CreateConnection() error = %v", err)
	}

	if _, err := s.RequestConnectionTermination(ctx, conn.UID, admin.UID, ""); err != nil {
		t.Fatalf("RequestConnectionTermination() error = %v", err)
	}

	// Hand the row to a different run, as a second replica would own it.
	if _, err := s.db.NewUpdate().
		Model((*Connection)(nil)).
		Where("uid = ?", conn.UID).
		Set("run_id = ?", uuid.NewString()).
		Exec(ctx); err != nil {
		t.Fatalf("failed to reassign the connection's run: %v", err)
	}

	pending, err := s.PendingTerminations(ctx)
	if err != nil {
		t.Fatalf("PendingTerminations() error = %v", err)
	}

	if _, ok := pendingByUID(pending)[conn.UID]; ok {
		t.Fatal("PendingTerminations() returned a connection owned by another run")
	}
}

// TestPendingTerminations_ClosedConnectionDropsOut verifies the request row is
// not cleared when acted on — the *close* is what takes the session out of the
// poll, which is what keeps the poller idempotent.
func TestPendingTerminations_ClosedConnectionDropsOut(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := setupTestStore(t)

	user, db := createTestUserAndDatabase(t, ctx, s, "pendclosed")

	admin, err := s.CreateUser(ctx, "pendclosedadmin", "hash", []string{RoleAdmin})
	if err != nil {
		t.Fatalf("CreateUser() error = %v", err)
	}

	conn, err := s.CreateConnection(ctx, user.UID, db.UID, "10.0.0.3")
	if err != nil {
		t.Fatalf("CreateConnection() error = %v", err)
	}

	if _, err := s.RequestConnectionTermination(ctx, conn.UID, admin.UID, ""); err != nil {
		t.Fatalf("RequestConnectionTermination() error = %v", err)
	}

	if err := s.CloseConnectionWithReason(ctx, conn.UID, Termination{
		Reason: TerminationAdminTerminated,
		By:     "pendclosedadmin",
	}); err != nil {
		t.Fatalf("CloseConnectionWithReason() error = %v", err)
	}

	pending, err := s.PendingTerminations(ctx)
	if err != nil {
		t.Fatalf("PendingTerminations() error = %v", err)
	}

	if _, ok := pendingByUID(pending)[conn.UID]; ok {
		t.Fatal("PendingTerminations() still returns a closed session")
	}

	reloaded, err := s.GetConnectionByUID(ctx, conn.UID)
	if err != nil {
		t.Fatalf("GetConnectionByUID() error = %v", err)
	}

	if reloaded.TerminateRequestedAt == nil {
		t.Error("the request row should survive the close, as the record of who asked")
	}

	if reloaded.TerminationReason == nil || *reloaded.TerminationReason != TerminationAdminTerminated {
		t.Errorf("termination_reason = %v, want %q", reloaded.TerminationReason, TerminationAdminTerminated)
	}
}

// TestPendingTerminations_RevokedGrant is the second arm, and the fix for the
// pre-existing bug: revoking a grant only ever signaled the in-process
// registry, so sessions on other replicas ran on until they expired. With the
// store as the source of truth, every replica finds its own.
func TestPendingTerminations_RevokedGrant(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := setupTestStore(t)

	user, db := createTestUserAndDatabase(t, ctx, s, "pendrevoke")

	admin, err := s.CreateUser(ctx, "pendrevokeadmin", "hash", []string{RoleAdmin})
	if err != nil {
		t.Fatalf("CreateUser() error = %v", err)
	}

	def := newTestGrantDefinition(t, ctx, s, admin.UID, GrantDefinition{})
	grant := newTestGrant(t, ctx, s, def, user.UID, db.UID, admin.UID,
		time.Now().Add(-time.Minute), time.Now().Add(time.Hour))

	conn, err := s.CreateConnection(ctx, user.UID, db.UID, "10.0.0.4", WithGrantUID(grant.UID))
	if err != nil {
		t.Fatalf("CreateConnection() error = %v", err)
	}

	// Nothing is pending while the grant stands.
	pending, err := s.PendingTerminations(ctx)
	if err != nil {
		t.Fatalf("PendingTerminations() error = %v", err)
	}

	if _, ok := pendingByUID(pending)[conn.UID]; ok {
		t.Fatal("a session under a live grant must not be pending termination")
	}

	if err := s.RevokeGrant(ctx, grant.UID, admin.UID); err != nil {
		t.Fatalf("RevokeGrant() error = %v", err)
	}

	pending, err = s.PendingTerminations(ctx)
	if err != nil {
		t.Fatalf("PendingTerminations() error = %v", err)
	}

	got, ok := pendingByUID(pending)[conn.UID]
	if !ok {
		t.Fatalf("PendingTerminations() did not return the session under the revoked grant, got %+v", pending)
	}

	// The reason recorded must be the revocation, not admin_terminated: the
	// same signal carries both, and conflating them would misreport why every
	// cross-replica revocation ended a session.
	if got.Reason != TerminationGrantRevoked {
		t.Errorf("reason = %q, want %q", got.Reason, TerminationGrantRevoked)
	}

	if got.RequestedBy != "" {
		t.Errorf("requested_by = %q, want empty: nobody asked for this session to end", got.RequestedBy)
	}
}

// TestRequestConnectionTermination_ClosedSession verifies "already closed" is
// its own answer, distinct from "no such session" — the API maps them to 409
// and 404.
func TestRequestConnectionTermination_ClosedSession(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := setupTestStore(t)

	user, db := createTestUserAndDatabase(t, ctx, s, "reqclosed")

	admin, err := s.CreateUser(ctx, "reqclosedadmin", "hash", []string{RoleAdmin})
	if err != nil {
		t.Fatalf("CreateUser() error = %v", err)
	}

	conn, err := s.CreateConnection(ctx, user.UID, db.UID, "10.0.0.5")
	if err != nil {
		t.Fatalf("CreateConnection() error = %v", err)
	}

	if err := s.CloseConnection(ctx, conn.UID); err != nil {
		t.Fatalf("CloseConnection() error = %v", err)
	}

	_, err = s.RequestConnectionTermination(ctx, conn.UID, admin.UID, "")
	if !errors.Is(err, ErrConnectionAlreadyClosed) {
		t.Fatalf("RequestConnectionTermination() error = %v, want ErrConnectionAlreadyClosed", err)
	}
}

// TestRequestConnectionTermination_UnknownConnection verifies an unknown uid is
// reported as not found.
func TestRequestConnectionTermination_UnknownConnection(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := setupTestStore(t)

	_, err := s.RequestConnectionTermination(ctx, uuid.New(), uuid.New(), "")
	if !errors.Is(err, ErrConnectionNotFound) {
		t.Fatalf("RequestConnectionTermination() error = %v, want ErrConnectionNotFound", err)
	}
}

// TestReconcileStampsInstanceLost verifies the crash reconcile names why it
// closed a row, so termination_reason has no unexplained NULLs on closed rows:
// without it a crash-orphaned session is indistinguishable from a client that
// hung up politely.
func TestReconcileStampsInstanceLost(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := setupTestStore(t)

	user, db := createTestUserAndDatabase(t, ctx, s, "instlost")

	// Open the session as a run that then dies: no instances row vouches for
	// it, so the reclaim below is entitled to close it.
	var (
		conn *Connection
		err  error
	)

	asRun(t, s, "dead-instance-instlost", "dead-run-instlost", func() {
		conn, err = s.CreateConnection(ctx, user.UID, db.UID, "10.0.0.6")
	})

	if err != nil {
		t.Fatalf("CreateConnection() error = %v", err)
	}

	s.SetInstanceID("live-instance-instlost")
	s.SetRunID("live-run-instlost")

	if err := s.RegisterInstance(ctx); err != nil {
		t.Fatalf("RegisterInstance() error = %v", err)
	}

	reclaimed, err := s.ReclaimDeadInstanceConnections(ctx)
	if err != nil {
		t.Fatalf("ReclaimDeadInstanceConnections() error = %v", err)
	}

	if reclaimed != 1 {
		t.Fatalf("ReclaimDeadInstanceConnections() reclaimed %d, want 1", reclaimed)
	}

	reloaded, err := s.GetConnectionByUID(ctx, conn.UID)
	if err != nil {
		t.Fatalf("GetConnectionByUID() error = %v", err)
	}

	if reloaded.DisconnectedAt == nil {
		t.Fatal("the reconcile should have closed the orphaned connection")
	}

	if reloaded.TerminationReason == nil || *reloaded.TerminationReason != TerminationInstanceLost {
		t.Fatalf("termination_reason = %v, want %q", reloaded.TerminationReason, TerminationInstanceLost)
	}
}
