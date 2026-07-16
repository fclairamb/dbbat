package store

import (
	"context"
	"errors"
	"testing"

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

func TestAutoApproveGrantRequest_CreatesGrantWithNoDecider(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	admin, user, db, _ := setupRequestFixtures(t, ctx, store, "auto")

	def, err := store.CreateGrantDefinition(ctx, &GrantDefinition{
		Name:            "auto-approve-def",
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
