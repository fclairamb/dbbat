package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

func setupRequestFixtures(t *testing.T, ctx context.Context, s *Store, suffix string) (*User, *User, *Server, *GrantDefinition) {
	t.Helper()

	admin := createTestAdmin(t, ctx, s, "req_admin_"+suffix)

	user, err := s.CreateUser(ctx, "requser_"+suffix, "hash", []string{RoleConnector})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	key := testEncryptionKey()

	db, err := s.CreateServer(ctx, &Server{
		Name:         "reqdb_" + suffix,
		Host:         "localhost",
		Port:         5432,
		DatabaseName: "db",
		Username:     "u",
		Password:     "p",
		SSLMode:      "disable",
	}, key)
	if err != nil {
		t.Fatalf("CreateServer: %v", err)
	}

	def, err := s.CreateGrantDefinition(ctx, &GrantDefinition{
		Name:            "req-def-" + suffix,
		Slug:            "req-def-" + suffix,
		DurationSeconds: 3600,
		Controls:        []string{ControlReadOnly},
		CreatedBy:       admin.UID,
	})
	if err != nil {
		t.Fatalf("CreateGrantDefinition: %v", err)
	}

	return admin, user, db, def
}

func TestCreateGrantRequest(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	_, user, db, def := setupRequestFixtures(t, ctx, store, "create")

	req, err := store.CreateGrantRequest(ctx, &GrantRequest{
		UserID:            user.UID,
		GrantDefinitionID: def.UID,
		DatabaseID:        db.UID,
		Justification:     "investigating bug 1234",
	})
	if err != nil {
		t.Fatalf("CreateGrantRequest: %v", err)
	}

	if req.Status != GrantRequestPending {
		t.Errorf("status = %q, want pending", req.Status)
	}

	if req.UID == uuid.Nil {
		t.Error("UID is nil")
	}
}

func TestApproveGrantRequest_CreatesGrant(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	admin, user, db, def := setupRequestFixtures(t, ctx, store, "approve")

	req, err := store.CreateGrantRequest(ctx, &GrantRequest{
		UserID:            user.UID,
		GrantDefinitionID: def.UID,
		DatabaseID:        db.UID,
	})
	if err != nil {
		t.Fatal(err)
	}

	grant, updated, err := store.ApproveGrantRequest(ctx, req.UID, admin.UID)
	if err != nil {
		t.Fatalf("ApproveGrantRequest: %v", err)
	}

	if grant == nil || grant.UID == uuid.Nil {
		t.Fatal("grant not created")
	}

	if grant.UserID != user.UID || grant.DatabaseID != db.UID {
		t.Errorf("grant user/db = %v/%v, want %v/%v",
			grant.UserID, grant.DatabaseID, user.UID, db.UID)
	}

	if !grant.IsReadOnly() {
		t.Error("expected grant to inherit read_only control")
	}

	if updated.Status != GrantRequestApproved {
		t.Errorf("status = %q, want approved", updated.Status)
	}

	if updated.ResultingGrantID == nil || *updated.ResultingGrantID != grant.UID {
		t.Error("resulting_grant_id not linked")
	}

	// Second approve should be a no-op error
	if _, _, err := store.ApproveGrantRequest(ctx, req.UID, admin.UID); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("double-approve err = %v, want ErrInvalidTransition", err)
	}
}

// TestApprovedGrantIsUsableWhenTheStoreClockLagsTheProcess pins the clock a
// grant's window is stamped from, which has to be the clock the auth path
// compares it against: GetActiveGrant filters `starts_at <= NOW()`, and
// PostgreSQL evaluates that against *its* clock. dbbat and its store are two
// machines, so a window stamped from time.Now() is refused by every one of the
// five proxies for as long as the process runs ahead of the store — an
// "approved but not usable yet" hole exactly as wide as the skew, and what
// made TestApprovedRequestBindsTheGrantToTheServerGroup flake on a laptop
// whose Docker VM had drifted a few milliseconds.
//
// Five seconds of lag is a comically large skew for a machine and a very small
// one for a test: it fails deterministically against a window stamped from the
// process clock, and is invisible to one stamped from the store's.
func TestApprovedGrantIsUsableWhenTheStoreClockLagsTheProcess(t *testing.T) {
	t.Parallel()

	store := setupTestStoreWithClockSkew(t, -5*time.Second)
	ctx := context.Background()

	admin, user, db, def := setupRequestFixtures(t, ctx, store, "dbclock")

	req, err := store.CreateGrantRequest(ctx, &GrantRequest{
		UserID:            user.UID,
		GrantDefinitionID: def.UID,
		DatabaseID:        db.UID,
	})
	if err != nil {
		t.Fatal(err)
	}

	grant, _, err := store.ApproveGrantRequest(ctx, req.UID, admin.UID)
	if err != nil {
		t.Fatalf("ApproveGrantRequest: %v", err)
	}

	// The grant authorizes a session the instant it exists — no waiting for
	// two clocks to converge.
	got, err := store.GetActiveGrant(ctx, user.UID, db.UID)
	if err != nil {
		t.Fatalf("GetActiveGrant right after approval: %v", err)
	}

	if got.UID != grant.UID {
		t.Errorf("GetActiveGrant = %v, want the approved grant %v", got.UID, grant.UID)
	}
}

func TestAutoApproveGrantRequest_CreatesGrantWithNoDecider(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	admin, user, db, _ := setupRequestFixtures(t, ctx, store, "auto")

	def, err := store.CreateGrantDefinition(ctx, &GrantDefinition{
		Name:            "auto-approve-def",
		Slug:            "auto-approve-def",
		DurationSeconds: 3600,
		Controls:        []string{ControlReadOnly},
		AutoApprove:     true,
		CreatedBy:       admin.UID,
	})
	if err != nil {
		t.Fatalf("CreateGrantDefinition: %v", err)
	}

	req, err := store.CreateGrantRequest(ctx, &GrantRequest{
		UserID:            user.UID,
		GrantDefinitionID: def.UID,
		DatabaseID:        db.UID,
		Justification:     "auto-approved routine access",
	})
	if err != nil {
		t.Fatal(err)
	}

	grant, updated, err := store.AutoApproveGrantRequest(ctx, req.UID, user.UID)
	if err != nil {
		t.Fatalf("AutoApproveGrantRequest: %v", err)
	}

	if grant == nil || grant.UID == uuid.Nil {
		t.Fatal("grant not created")
	}

	if grant.GrantedBy != user.UID {
		t.Errorf("grant.GrantedBy = %v, want requester %v", grant.GrantedBy, user.UID)
	}

	if updated.Status != GrantRequestApproved {
		t.Errorf("status = %q, want approved", updated.Status)
	}

	if updated.DecidedBy != nil {
		t.Errorf("DecidedBy = %v, want nil (no human decider)", *updated.DecidedBy)
	}

	if updated.ResultingGrantID == nil || *updated.ResultingGrantID != grant.UID {
		t.Error("resulting_grant_id not linked")
	}

	// Second approve should fail — already decided.
	if _, _, err := store.AutoApproveGrantRequest(ctx, req.UID, user.UID); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("double auto-approve err = %v, want ErrInvalidTransition", err)
	}
}

func TestApproveGrantRequest_DefinitionInactive(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	admin, user, db, def := setupRequestFixtures(t, ctx, store, "inactive")

	req, err := store.CreateGrantRequest(ctx, &GrantRequest{
		UserID:            user.UID,
		GrantDefinitionID: def.UID,
		DatabaseID:        db.UID,
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := store.DeactivateGrantDefinition(ctx, def.UID); err != nil {
		t.Fatal(err)
	}

	if _, _, err := store.ApproveGrantRequest(ctx, req.UID, admin.UID); !errors.Is(err, ErrDefinitionInactive) {
		t.Errorf("err = %v, want ErrDefinitionInactive", err)
	}
}

func TestDenyGrantRequest(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	admin, user, db, def := setupRequestFixtures(t, ctx, store, "deny")

	req, err := store.CreateGrantRequest(ctx, &GrantRequest{
		UserID:            user.UID,
		GrantDefinitionID: def.UID,
		DatabaseID:        db.UID,
	})
	if err != nil {
		t.Fatal(err)
	}

	updated, err := store.DenyGrantRequest(ctx, req.UID, admin.UID, "out of scope")
	if err != nil {
		t.Fatalf("DenyGrantRequest: %v", err)
	}

	if updated.Status != GrantRequestDenied {
		t.Errorf("status = %q", updated.Status)
	}

	if updated.DecisionReason == nil || *updated.DecisionReason != "out of scope" {
		t.Error("reason not persisted")
	}
}

func TestCancelGrantRequest(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	_, user, db, def := setupRequestFixtures(t, ctx, store, "cancel")

	req, err := store.CreateGrantRequest(ctx, &GrantRequest{
		UserID:            user.UID,
		GrantDefinitionID: def.UID,
		DatabaseID:        db.UID,
	})
	if err != nil {
		t.Fatal(err)
	}

	updated, err := store.CancelGrantRequest(ctx, req.UID, user.UID)
	if err != nil {
		t.Fatalf("CancelGrantRequest: %v", err)
	}

	if updated.Status != GrantRequestCancelled {
		t.Errorf("status = %q, want cancelled", updated.Status) //nolint:misspell // status value matches DB
	}
}

func TestHasPendingRequest(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	_, user, db, def := setupRequestFixtures(t, ctx, store, "pending")

	yes, err := store.HasPendingRequest(ctx, user.UID, def.UID, db.UID)
	if err != nil {
		t.Fatal(err)
	}

	if yes {
		t.Error("expected no pending request initially")
	}

	if _, err := store.CreateGrantRequest(ctx, &GrantRequest{
		UserID:            user.UID,
		GrantDefinitionID: def.UID,
		DatabaseID:        db.UID,
	}); err != nil {
		t.Fatal(err)
	}

	yes, err = store.HasPendingRequest(ctx, user.UID, def.UID, db.UID)
	if err != nil {
		t.Fatal(err)
	}

	if !yes {
		t.Error("expected pending request after create")
	}
}

// TestGrantRequestDefinitionFollowsLineage is the regression test for a
// request whose definition has been edited out from under it: the edit
// archives the version the request pins and inserts a successor, and every
// read path has to hand back that successor.
//
// It used to hand back nothing usable — the web UI resolved the pinned uid
// against the live definitions listing, which by construction no longer
// contained it, and rendered a bare uid fragment where the definition's name
// and auto-approve state belong.
func TestGrantRequestDefinitionFollowsLineage(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	admin, user, db, def := setupRequestFixtures(t, ctx, store, "lineage")

	req, err := store.CreateGrantRequest(ctx, &GrantRequest{
		UserID:            user.UID,
		GrantDefinitionID: def.UID,
		DatabaseID:        db.UID,
	})
	if err != nil {
		t.Fatalf("CreateGrantRequest: %v", err)
	}

	if req.Definition == nil || req.Definition.UID != def.UID {
		t.Fatalf("create: definition = %v, want %s", req.Definition, def.UID)
	}

	// Edit the definition — this archives def and inserts a successor.
	edited := *def
	edited.AutoApprove = true

	next, err := store.UpdateGrantDefinition(ctx, &edited)
	if err != nil {
		t.Fatalf("UpdateGrantDefinition: %v", err)
	}

	if next.UID == def.UID {
		t.Fatal("expected the edit to insert a new version")
	}

	// The request still pins the archived version...
	fetched, err := store.GetGrantRequest(ctx, req.UID)
	if err != nil {
		t.Fatalf("GetGrantRequest: %v", err)
	}

	if fetched.GrantDefinitionID != def.UID {
		t.Errorf("pinned definition = %s, want %s", fetched.GrantDefinitionID, def.UID)
	}

	// ...but reads it as the live one.
	if fetched.Definition == nil {
		t.Fatal("get: no definition attached")
	}

	if fetched.Definition.UID != next.UID {
		t.Errorf("get: definition = %s, want the live version %s", fetched.Definition.UID, next.UID)
	}

	if !fetched.Definition.AutoApprove {
		t.Error("get: attached definition should carry the edit's auto_approve")
	}

	listed, err := store.ListGrantRequests(ctx, GrantRequestFilter{UserID: &user.UID})
	if err != nil {
		t.Fatalf("ListGrantRequests: %v", err)
	}

	if len(listed) != 1 {
		t.Fatalf("listed %d requests, want 1", len(listed))
	}

	if listed[0].Definition == nil || listed[0].Definition.UID != next.UID {
		t.Errorf("list: definition = %v, want the live version %s", listed[0].Definition, next.UID)
	}

	// Approving hands back the same live version it materialized the grant
	// from, so a caller never has to re-read to render the outcome.
	_, approved, err := store.ApproveGrantRequest(ctx, req.UID, admin.UID)
	if err != nil {
		t.Fatalf("ApproveGrantRequest: %v", err)
	}

	if approved.Definition == nil || approved.Definition.UID != next.UID {
		t.Errorf("approve: definition = %v, want the live version %s", approved.Definition, next.UID)
	}
}
