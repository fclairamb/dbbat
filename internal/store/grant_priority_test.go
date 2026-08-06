package store

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestAutoPriority(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		controls []string
		want     int16
	}{
		{"no controls is full write", nil, PriorityFullWrite},
		{"empty controls is full write", []string{}, PriorityFullWrite},
		{"block_copy is restricted write", []string{ControlBlockCopy}, PriorityRestrictedWrite},
		{"block_ddl is restricted write", []string{ControlBlockDDL}, PriorityRestrictedWrite},
		{"both write blocks stay restricted write", []string{ControlBlockCopy, ControlBlockDDL}, PriorityRestrictedWrite},
		{"read_only alone", []string{ControlReadOnly}, PriorityReadOnly},
		// read_only is the most restrictive control, so it decides the tier no
		// matter what else rides along or in what order.
		{"read_only wins over other controls", []string{ControlBlockCopy, ControlReadOnly}, PriorityReadOnly},
		{"read_only wins whatever the order", []string{ControlReadOnly, ControlBlockDDL}, PriorityReadOnly},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := AutoPriority(tt.controls); got != tt.want {
				t.Errorf("AutoPriority(%v) = %d, want %d", tt.controls, got, tt.want)
			}
		})
	}

	// The tiers must stay strictly descending — the whole point is that a more
	// capable grant beats a less capable one.
	tiers := []int16{PriorityFullWrite, PriorityRestrictedWrite, PriorityReadOnly}
	for i := 1; i < len(tiers); i++ {
		if tiers[i-1] <= tiers[i] {
			t.Fatalf("tiers are not strictly descending: %v", tiers)
		}
	}
}

func TestResolvePriority(t *testing.T) {
	t.Parallel()

	// Zero reads as "unset" and falls back to the tier.
	if got := ResolvePriority(0, []string{ControlReadOnly}); got != PriorityReadOnly {
		t.Errorf("ResolvePriority(0, read_only) = %d, want %d", got, PriorityReadOnly)
	}

	// Anything non-zero is taken verbatim, including values below every tier
	// and negative ones.
	for _, explicit := range []int16{1, 75, 500, -5} {
		if got := ResolvePriority(explicit, nil); got != explicit {
			t.Errorf("ResolvePriority(%d, nil) = %d, want %d", explicit, got, explicit)
		}
	}
}

// grantPriorityFixture creates a user, a database and an admin for the
// selection tests, keeping each test's rows isolated from its siblings.
func grantPriorityFixture(t *testing.T, ctx context.Context, s *Store, suffix string) (*User, *Server, *User) {
	t.Helper()

	user, database := createTestUserAndDatabase(t, ctx, s, suffix)

	admin, err := s.CreateUser(ctx, "prioadmin_"+suffix, "hash", []string{RoleAdmin})
	if err != nil {
		t.Fatalf("CreateUser() error = %v", err)
	}

	return user, database, admin
}

// TestGetActiveGrant_PrioritySelection is the core regression guard: with two
// overlapping active grants the proxy must pick the more capable one, and it
// must do so regardless of which was created first. Before priorities, the
// newest-created grant won — so the read/write grant was silently ignored
// whenever it happened to be created first.
func TestGetActiveGrant_PrioritySelection(t *testing.T) {
	t.Parallel()

	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Now()

	newGrant := func(user, database, admin uuid.UUID, controls []string, priority int16, expiresIn time.Duration) *Grant {
		t.Helper()

		created, err := s.CreateGrant(ctx, &Grant{
			UserID:     user,
			DatabaseID: database,
			Controls:   controls,
			GrantedBy:  admin,
			StartsAt:   now.Add(-time.Hour),
			ExpiresAt:  now.Add(expiresIn),
			Priority:   priority,
		})
		if err != nil {
			t.Fatalf("CreateGrant() error = %v", err)
		}

		return created
	}

	t.Run("read-write wins when created last", func(t *testing.T) {
		t.Parallel()

		user, database, admin := grantPriorityFixture(t, ctx, s, "prio_rw_last")

		readOnly := newGrant(user.UID, database.UID, admin.UID, []string{ControlReadOnly}, 0, time.Hour)
		readWrite := newGrant(user.UID, database.UID, admin.UID, nil, 0, time.Hour)

		got, err := s.GetActiveGrant(ctx, user.UID, database.UID)
		if err != nil {
			t.Fatalf("GetActiveGrant() error = %v", err)
		}

		if got.UID != readWrite.UID {
			t.Errorf("GetActiveGrant() = %v (read-only: %v), want the read/write grant %v",
				got.UID, readOnly.UID, readWrite.UID)
		}
	})

	t.Run("read-write wins when created first", func(t *testing.T) {
		t.Parallel()

		user, database, admin := grantPriorityFixture(t, ctx, s, "prio_rw_first")

		readWrite := newGrant(user.UID, database.UID, admin.UID, nil, 0, time.Hour)
		readOnly := newGrant(user.UID, database.UID, admin.UID, []string{ControlReadOnly}, 0, time.Hour)

		got, err := s.GetActiveGrant(ctx, user.UID, database.UID)
		if err != nil {
			t.Fatalf("GetActiveGrant() error = %v", err)
		}

		if got.UID != readWrite.UID {
			t.Errorf("GetActiveGrant() = %v (read-only: %v), want the read/write grant %v",
				got.UID, readOnly.UID, readWrite.UID)
		}
	})

	t.Run("explicit low priority demotes the read-write grant", func(t *testing.T) {
		t.Parallel()

		user, database, admin := grantPriorityFixture(t, ctx, s, "prio_override")

		// An admin deliberately ranks the read/write grant below the
		// read-only one — the override has to actually win over the tier.
		newGrant(user.UID, database.UID, admin.UID, nil, 1, time.Hour)
		readOnly := newGrant(user.UID, database.UID, admin.UID, []string{ControlReadOnly}, 0, time.Hour)

		got, err := s.GetActiveGrant(ctx, user.UID, database.UID)
		if err != nil {
			t.Fatalf("GetActiveGrant() error = %v", err)
		}

		if got.UID != readOnly.UID {
			t.Errorf("GetActiveGrant() = %v, want the read-only grant %v", got.UID, readOnly.UID)
		}

		if got.Priority != PriorityReadOnly {
			t.Errorf("selected grant Priority = %d, want %d", got.Priority, PriorityReadOnly)
		}
	})

	t.Run("priority tie goes to the latest expiry", func(t *testing.T) {
		t.Parallel()

		user, database, admin := grantPriorityFixture(t, ctx, s, "prio_tie_expiry")

		// Same tier (both read-only), created newest-last: without the expiry
		// tie-break the newest would win, so the assertion only passes if
		// expires_at is consulted first.
		longer := newGrant(user.UID, database.UID, admin.UID, []string{ControlReadOnly}, 0, 4*time.Hour)
		newGrant(user.UID, database.UID, admin.UID, []string{ControlReadOnly}, 0, time.Hour)

		got, err := s.GetActiveGrant(ctx, user.UID, database.UID)
		if err != nil {
			t.Fatalf("GetActiveGrant() error = %v", err)
		}

		if got.UID != longer.UID {
			t.Errorf("GetActiveGrant() = %v, want the longer-lived grant %v", got.UID, longer.UID)
		}
	})

	t.Run("priority and expiry tie goes to the newest", func(t *testing.T) {
		t.Parallel()

		user, database, admin := grantPriorityFixture(t, ctx, s, "prio_tie_newest")

		expiry := now.Add(3 * time.Hour)

		first, err := s.CreateGrant(ctx, &Grant{
			UserID:     user.UID,
			DatabaseID: database.UID,
			Controls:   []string{ControlReadOnly},
			GrantedBy:  admin.UID,
			StartsAt:   now.Add(-time.Hour),
			ExpiresAt:  expiry,
		})
		if err != nil {
			t.Fatalf("CreateGrant() error = %v", err)
		}

		second, err := s.CreateGrant(ctx, &Grant{
			UserID:     user.UID,
			DatabaseID: database.UID,
			Controls:   []string{ControlReadOnly},
			GrantedBy:  admin.UID,
			StartsAt:   now.Add(-time.Hour),
			ExpiresAt:  expiry,
		})
		if err != nil {
			t.Fatalf("CreateGrant() error = %v", err)
		}

		got, err := s.GetActiveGrant(ctx, user.UID, database.UID)
		if err != nil {
			t.Fatalf("GetActiveGrant() error = %v", err)
		}

		if got.UID != second.UID {
			t.Errorf("GetActiveGrant() = %v (first: %v), want the newest grant %v",
				got.UID, first.UID, second.UID)
		}
	})
}

// TestCreateGrant_AutoPriority proves the store resolves the tier itself, so a
// caller that never heard of priorities (seed data, an older API client) can't
// insert a grant that ranks below everything.
func TestCreateGrant_AutoPriority(t *testing.T) {
	t.Parallel()

	s := setupTestStore(t)
	ctx := context.Background()
	user, database, admin := grantPriorityFixture(t, ctx, s, "prio_auto")
	now := time.Now()

	tests := []struct {
		name     string
		controls []string
		explicit int16
		want     int16
	}{
		{"full write", nil, 0, PriorityFullWrite},
		{"restricted write", []string{ControlBlockDDL}, 0, PriorityRestrictedWrite},
		{"read only", []string{ControlReadOnly}, 0, PriorityReadOnly},
		{"explicit override is kept verbatim", []string{ControlReadOnly}, 90, 90},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			created, err := s.CreateGrant(ctx, &Grant{
				UserID:     user.UID,
				DatabaseID: database.UID,
				Controls:   tt.controls,
				GrantedBy:  admin.UID,
				StartsAt:   now,
				ExpiresAt:  now.Add(time.Hour),
				Priority:   tt.explicit,
			})
			if err != nil {
				t.Fatalf("CreateGrant() error = %v", err)
			}

			if created.Priority != tt.want {
				t.Errorf("created.Priority = %d, want %d", created.Priority, tt.want)
			}

			// The value must survive the round trip, not just the RETURNING
			// clause of the insert.
			fetched, err := s.GetGrantByUID(ctx, created.UID)
			if err != nil {
				t.Fatalf("GetGrantByUID() error = %v", err)
			}

			if fetched.Priority != tt.want {
				t.Errorf("fetched.Priority = %d, want %d", fetched.Priority, tt.want)
			}
		})
	}
}

// TestBuildGrantFromDefinition_Priority covers the definition-materialization
// path: nil on the definition means "compute from the controls", a value means
// "copy it verbatim".
func TestBuildGrantFromDefinition_Priority(t *testing.T) {
	t.Parallel()

	userID, databaseID, grantedBy := uuid.New(), uuid.New(), uuid.New()
	now := time.Now()

	pinned := int16(77)

	tests := []struct {
		name string
		def  *GrantDefinition
		want int16
	}{
		{
			name: "nil priority derives from controls",
			def:  &GrantDefinition{DurationSeconds: 3600, Controls: []string{ControlReadOnly}},
			want: PriorityReadOnly,
		},
		{
			name: "nil priority on a writable definition",
			def:  &GrantDefinition{DurationSeconds: 3600},
			want: PriorityFullWrite,
		},
		{
			name: "explicit priority is copied verbatim",
			def:  &GrantDefinition{DurationSeconds: 3600, Controls: []string{ControlReadOnly}, Priority: &pinned},
			want: pinned,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := BuildGrantFromDefinition(tt.def, userID, databaseID, grantedBy, now)
			if got.Priority != tt.want {
				t.Errorf("BuildGrantFromDefinition() Priority = %d, want %d", got.Priority, tt.want)
			}
		})
	}
}

// TestGetActiveGrant_ReselectsAfterTheWinnerExpires covers the reconnect half
// of "a session lives and dies with its grant": a session admitted under the
// winning grant is torn down when that grant expires (proven at the
// LimitGuard level in internal/proxy/shared), and the client's *reconnect* is
// then admitted under the next-best still-active grant rather than being
// refused.
func TestGetActiveGrant_ReselectsAfterTheWinnerExpires(t *testing.T) {
	t.Parallel()

	s := setupTestStore(t)
	ctx := context.Background()
	user, database, admin := grantPriorityFixture(t, ctx, s, "prio_reselect")
	now := time.Now()

	// A: full write, so it wins the initial selection, but short-lived.
	shortLived, err := s.CreateGrant(ctx, &Grant{
		UserID: user.UID, DatabaseID: database.UID,
		GrantedBy: admin.UID, StartsAt: now.Add(-time.Minute),
		ExpiresAt: now.Add(700 * time.Millisecond),
	})
	if err != nil {
		t.Fatalf("CreateGrant() error = %v", err)
	}

	// B: read-only, lower priority, but outlives A.
	longLived, err := s.CreateGrant(ctx, &Grant{
		UserID: user.UID, DatabaseID: database.UID, Controls: []string{ControlReadOnly},
		GrantedBy: admin.UID, StartsAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("CreateGrant() error = %v", err)
	}

	admitted, err := s.GetActiveGrant(ctx, user.UID, database.UID)
	if err != nil {
		t.Fatalf("GetActiveGrant() error = %v", err)
	}

	if admitted.UID != shortLived.UID {
		t.Fatalf("initial GetActiveGrant() = %v, want the full-write grant %v", admitted.UID, shortLived.UID)
	}

	// Let A lapse. Selection is a plain `expires_at > NOW()` filter, so this
	// needs real time to pass — the margin is generous enough to absorb clock
	// skew between the test process and Postgres.
	time.Sleep(1500 * time.Millisecond)

	reconnected, err := s.GetActiveGrant(ctx, user.UID, database.UID)
	if err != nil {
		t.Fatalf("GetActiveGrant() after expiry error = %v", err)
	}

	if reconnected.UID != longLived.UID {
		t.Errorf("GetActiveGrant() after expiry = %v, want the still-active grant %v",
			reconnected.UID, longLived.UID)
	}

	// And the reconnect is admitted under B's terms, not A's — the session
	// does not inherit the expired grant's write access.
	if !reconnected.IsReadOnly() {
		t.Error("reconnect should be pinned read-only by the surviving grant")
	}
}

// TestListGrants_OrdersLikeSelection pins the UI listing to the proxy's own
// ordering, so the grant shown on top of a (user, database) group is the one a
// new session would actually be admitted under.
func TestListGrants_OrdersLikeSelection(t *testing.T) {
	t.Parallel()

	s := setupTestStore(t)
	ctx := context.Background()
	user, database, admin := grantPriorityFixture(t, ctx, s, "prio_list")
	now := time.Now()

	// Created read-only first, then read/write, then a longer-lived read-only:
	// no single column reproduces the expected order by accident.
	readOnlyShort, err := s.CreateGrant(ctx, &Grant{
		UserID: user.UID, DatabaseID: database.UID, Controls: []string{ControlReadOnly},
		GrantedBy: admin.UID, StartsAt: now.Add(-time.Hour), ExpiresAt: now.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("CreateGrant() error = %v", err)
	}

	readWrite, err := s.CreateGrant(ctx, &Grant{
		UserID: user.UID, DatabaseID: database.UID,
		GrantedBy: admin.UID, StartsAt: now.Add(-time.Hour), ExpiresAt: now.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("CreateGrant() error = %v", err)
	}

	readOnlyLong, err := s.CreateGrant(ctx, &Grant{
		UserID: user.UID, DatabaseID: database.UID, Controls: []string{ControlReadOnly},
		GrantedBy: admin.UID, StartsAt: now.Add(-time.Hour), ExpiresAt: now.Add(5 * time.Hour),
	})
	if err != nil {
		t.Fatalf("CreateGrant() error = %v", err)
	}

	listed, err := s.ListGrants(ctx, GrantFilter{UserID: &user.UID, DatabaseID: &database.UID})
	if err != nil {
		t.Fatalf("ListGrants() error = %v", err)
	}

	want := []uuid.UUID{readWrite.UID, readOnlyLong.UID, readOnlyShort.UID}
	if len(listed) != len(want) {
		t.Fatalf("ListGrants() returned %d grants, want %d", len(listed), len(want))
	}

	for i, uid := range want {
		if listed[i].UID != uid {
			t.Errorf("ListGrants()[%d] = %v, want %v", i, listed[i].UID, uid)
		}
	}

	// The head of the list must be exactly what the proxy would select.
	selected, err := s.GetActiveGrant(ctx, user.UID, database.UID)
	if err != nil {
		t.Fatalf("GetActiveGrant() error = %v", err)
	}

	if selected.UID != listed[0].UID {
		t.Errorf("GetActiveGrant() = %v, but ListGrants() puts %v first", selected.UID, listed[0].UID)
	}
}
