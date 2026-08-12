package store

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestLogAuditEvent(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	// Create users for the audit events
	user, err := store.CreateUser(ctx, "audituser", "hash", []string{RoleConnector})
	if err != nil {
		t.Fatalf("CreateUser() error = %v", err)
	}
	admin, err := store.CreateUser(ctx, "auditadmin", "hash", []string{RoleAdmin, RoleConnector})
	if err != nil {
		t.Fatalf("CreateUser() error = %v", err)
	}

	t.Run("log event with all fields", func(t *testing.T) {
		t.Parallel()

		details := json.RawMessage(`{"action": "grant_created", "database_id": 1}`)
		event := &AuditEvent{
			EventType:   "grant_created",
			UserID:      &user.UID,
			PerformedBy: &admin.UID,
			Details:     details,
		}

		err := store.LogAuditEvent(ctx, event)
		if err != nil {
			t.Fatalf("LogAuditEvent() error = %v", err)
		}
	})

	t.Run("log event without user", func(t *testing.T) {
		t.Parallel()

		details := json.RawMessage(`{"message": "system startup"}`)
		event := &AuditEvent{
			EventType: "system_event",
			Details:   details,
		}

		err := store.LogAuditEvent(ctx, event)
		if err != nil {
			t.Fatalf("LogAuditEvent() error = %v", err)
		}
	})

	t.Run("log event without details", func(t *testing.T) {
		t.Parallel()

		event := &AuditEvent{
			EventType:   "user_login",
			UserID:      &user.UID,
			PerformedBy: &user.UID,
		}

		err := store.LogAuditEvent(ctx, event)
		if err != nil {
			t.Fatalf("LogAuditEvent() error = %v", err)
		}
	})
}

func TestListAuditEvents(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	// Create users
	user1, _ := store.CreateUser(ctx, "listaudit1", "hash", []string{RoleConnector})
	user2, _ := store.CreateUser(ctx, "listaudit2", "hash", []string{RoleConnector})
	admin, _ := store.CreateUser(ctx, "listauditadmin", "hash", []string{RoleAdmin, RoleConnector})

	// Create audit events
	events := []*AuditEvent{
		{EventType: "grant_created", UserID: &user1.UID, PerformedBy: &admin.UID, Details: json.RawMessage(`{}`)},
		{EventType: "grant_revoked", UserID: &user1.UID, PerformedBy: &admin.UID, Details: json.RawMessage(`{}`)},
		{EventType: "user_created", UserID: &user2.UID, PerformedBy: &admin.UID, Details: json.RawMessage(`{}`)},
	}

	for _, e := range events {
		err := store.LogAuditEvent(ctx, e)
		if err != nil {
			t.Fatalf("LogAuditEvent() error = %v", err)
		}
		// Small delay to ensure different timestamps
		time.Sleep(10 * time.Millisecond)
	}

	t.Run("list all", func(t *testing.T) {
		t.Parallel()

		result, err := store.ListAuditEvents(ctx, AuditFilter{})
		if err != nil {
			t.Fatalf("ListAuditEvents() error = %v", err)
		}
		if len(result) < 3 {
			t.Errorf("ListAuditEvents() len = %d, want >= 3", len(result))
		}
	})

	t.Run("filter by event type", func(t *testing.T) {
		t.Parallel()

		eventType := "grant_created"
		result, err := store.ListAuditEvents(ctx, AuditFilter{EventType: &eventType})
		if err != nil {
			t.Fatalf("ListAuditEvents() error = %v", err)
		}
		if len(result) != 1 {
			t.Errorf("ListAuditEvents() len = %d, want 1", len(result))
		}
		if result[0].EventType != "grant_created" {
			t.Errorf("ListAuditEvents()[0].EventType = %q, want %q", result[0].EventType, "grant_created")
		}
	})

	t.Run("filter by user", func(t *testing.T) {
		t.Parallel()

		result, err := store.ListAuditEvents(ctx, AuditFilter{UserID: &user1.UID})
		if err != nil {
			t.Fatalf("ListAuditEvents() error = %v", err)
		}
		if len(result) != 2 {
			t.Errorf("ListAuditEvents() len = %d, want 2", len(result))
		}
	})

	t.Run("filter by performed_by", func(t *testing.T) {
		t.Parallel()

		result, err := store.ListAuditEvents(ctx, AuditFilter{PerformedBy: &admin.UID})
		if err != nil {
			t.Fatalf("ListAuditEvents() error = %v", err)
		}
		if len(result) < 3 {
			t.Errorf("ListAuditEvents() len = %d, want >= 3", len(result))
		}
	})

	t.Run("filter by time range", func(t *testing.T) {
		t.Parallel()

		startTime := time.Now().Add(-1 * time.Hour)
		endTime := time.Now().Add(1 * time.Hour)
		result, err := store.ListAuditEvents(ctx, AuditFilter{StartTime: &startTime, EndTime: &endTime})
		if err != nil {
			t.Fatalf("ListAuditEvents() error = %v", err)
		}
		if len(result) < 3 {
			t.Errorf("ListAuditEvents() len = %d, want >= 3", len(result))
		}
	})

	t.Run("with limit", func(t *testing.T) {
		t.Parallel()

		result, err := store.ListAuditEvents(ctx, AuditFilter{Limit: 2})
		if err != nil {
			t.Fatalf("ListAuditEvents() error = %v", err)
		}
		if len(result) != 2 {
			t.Errorf("ListAuditEvents() len = %d, want 2", len(result))
		}
	})

	t.Run("with offset", func(t *testing.T) {
		t.Parallel()

		result, err := store.ListAuditEvents(ctx, AuditFilter{Limit: 10, Offset: 2})
		if err != nil {
			t.Fatalf("ListAuditEvents() error = %v", err)
		}
		if len(result) < 1 {
			t.Errorf("ListAuditEvents() len = %d, want >= 1", len(result))
		}
	})

	t.Run("combined filters", func(t *testing.T) {
		t.Parallel()

		eventType := "grant_created"
		result, err := store.ListAuditEvents(ctx, AuditFilter{
			EventType:   &eventType,
			PerformedBy: &admin.UID,
		})
		if err != nil {
			t.Fatalf("ListAuditEvents() error = %v", err)
		}
		if len(result) != 1 {
			t.Errorf("ListAuditEvents() len = %d, want 1", len(result))
		}
	})
}

func TestAuditEventFields(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	user, _ := store.CreateUser(ctx, "auditfields", "hash", []string{RoleConnector})
	admin, _ := store.CreateUser(ctx, "auditfieldsadmin", "hash", []string{RoleAdmin, RoleConnector})

	details := json.RawMessage(`{"key": "value", "count": 42}`)
	event := &AuditEvent{
		EventType:   "test_event",
		UserID:      &user.UID,
		PerformedBy: &admin.UID,
		Details:     details,
	}

	err := store.LogAuditEvent(ctx, event)
	if err != nil {
		t.Fatalf("LogAuditEvent() error = %v", err)
	}

	// Retrieve and verify
	eventType := "test_event"
	result, err := store.ListAuditEvents(ctx, AuditFilter{EventType: &eventType})
	if err != nil {
		t.Fatalf("ListAuditEvents() error = %v", err)
	}

	if len(result) == 0 {
		t.Fatal("ListAuditEvents() returned empty result")
	}

	retrieved := result[0]
	if retrieved.UID == uuid.Nil {
		t.Error("event.UID = uuid.Nil")
	}
	if retrieved.EventType != "test_event" {
		t.Errorf("event.EventType = %q, want %q", retrieved.EventType, "test_event")
	}
	if retrieved.UserID == nil || *retrieved.UserID != user.UID {
		t.Errorf("event.UserID = %v, want %s", retrieved.UserID, user.UID)
	}
	if retrieved.PerformedBy == nil || *retrieved.PerformedBy != admin.UID {
		t.Errorf("event.PerformedBy = %v, want %s", retrieved.PerformedBy, admin.UID)
	}
	if retrieved.CreatedAt.IsZero() {
		t.Error("event.CreatedAt is zero")
	}

	// Verify details JSON
	var parsedDetails map[string]interface{}
	if err := json.Unmarshal(retrieved.Details, &parsedDetails); err != nil {
		t.Fatalf("json.Unmarshal(details) error = %v", err)
	}
	if parsedDetails["key"] != "value" {
		t.Errorf("details[\"key\"] = %v, want \"value\"", parsedDetails["key"])
	}
	if parsedDetails["count"] != float64(42) {
		t.Errorf("details[\"count\"] = %v, want 42", parsedDetails["count"])
	}
}

// TestListLatestEventPerUser pins the DISTINCT ON semantics the users page
// depends on: exactly one row per user, always the newest, and nothing at all
// for a user the directory never touched — which is what lets the UI read
// "absent" as "never synced" rather than "fell out of the window".
func TestListLatestEventPerUser(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	const eventType = "user.roles_synced"

	synced, err := store.CreateUser(ctx, "syncedguy", "hash", []string{RoleConnector})
	if err != nil {
		t.Fatalf("CreateUser() error = %v", err)
	}

	never, err := store.CreateUser(ctx, "neverguy", "hash", []string{RoleConnector})
	if err != nil {
		t.Fatalf("CreateUser() error = %v", err)
	}

	other, err := store.CreateUser(ctx, "otherguy", "hash", []string{RoleConnector})
	if err != nil {
		t.Fatalf("CreateUser() error = %v", err)
	}

	log := func(userID uuid.UUID, evType, details string) {
		t.Helper()

		if err := store.LogAuditEvent(ctx, &AuditEvent{
			EventType: evType,
			UserID:    &userID,
			Details:   json.RawMessage(details),
		}); err != nil {
			t.Fatalf("LogAuditEvent() error = %v", err)
		}
	}

	// Three syncs for the same user, oldest first; only the last one may show.
	log(synced.UID, eventType, `{"groups":["first"]}`)
	log(synced.UID, eventType, `{"groups":["second"]}`)
	log(synced.UID, eventType, `{"groups":["newest"]}`)

	// A different event type for the user with no sync: it must not make them
	// look synced.
	log(never.UID, "user.created", `{"groups":["ignored"]}`)

	// A second user with one sync, so the "one row per user" claim is tested
	// against more than a single group.
	log(other.UID, eventType, `{"groups":["analysts"]}`)

	// An entry with no user at all — the join must drop it rather than fail.
	if err := store.LogAuditEvent(ctx, &AuditEvent{
		EventType: eventType,
		Details:   json.RawMessage(`{"groups":["orphan"]}`),
	}); err != nil {
		t.Fatalf("LogAuditEvent() error = %v", err)
	}

	syncs, err := store.ListLatestEventPerUser(ctx, eventType)
	if err != nil {
		t.Fatalf("ListLatestEventPerUser() error = %v", err)
	}

	byUser := make(map[uuid.UUID]UserRoleSync, len(syncs))
	for _, sync := range syncs {
		if _, dup := byUser[sync.UserID]; dup {
			t.Fatalf("ListLatestEventPerUser() returned %s twice", sync.UserID)
		}
		byUser[sync.UserID] = sync
	}

	if len(byUser) != 2 {
		t.Fatalf("ListLatestEventPerUser() returned %d users, want 2", len(byUser))
	}

	if _, ok := byUser[never.UID]; ok {
		t.Error("a user with no sync of this type must be absent from the result")
	}

	got, ok := byUser[synced.UID]
	if !ok {
		t.Fatalf("ListLatestEventPerUser() missing the synced user")
	}

	if got.Username != "syncedguy" {
		t.Errorf("sync.Username = %q, want %q", got.Username, "syncedguy")
	}
	if got.EventType != eventType {
		t.Errorf("sync.EventType = %q, want %q", got.EventType, eventType)
	}
	if got.UID == uuid.Nil {
		t.Error("sync.UID is the nil UUID; the audit entry's own UID must come back")
	}
	if got.CreatedAt.IsZero() {
		t.Error("sync.CreatedAt is zero")
	}

	// The details must survive verbatim: the groups are why the roles changed.
	var details struct {
		Groups []string `json:"groups"`
	}
	if err := json.Unmarshal(got.Details, &details); err != nil {
		t.Fatalf("json.Unmarshal(details) error = %v", err)
	}
	if len(details.Groups) != 1 || details.Groups[0] != "newest" {
		t.Errorf("sync.Details groups = %v, want [newest] (the newest entry)", details.Groups)
	}

	// A deleted user drops out: the join is against live users.
	if err := store.DeleteUser(ctx, other.UID); err != nil {
		t.Fatalf("DeleteUser() error = %v", err)
	}

	syncs, err = store.ListLatestEventPerUser(ctx, eventType)
	if err != nil {
		t.Fatalf("ListLatestEventPerUser() after delete error = %v", err)
	}

	for _, sync := range syncs {
		if sync.UserID == other.UID {
			t.Error("a deleted user must not appear in the result")
		}
	}
}
