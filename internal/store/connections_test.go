package store

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestCreateConnection(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	user, database := createTestUserAndDatabase(t, ctx, store, "conn")

	t.Run("create connection", func(t *testing.T) {
		conn, err := store.CreateConnection(ctx, user.UID, database.UID, "192.168.1.100")
		if err != nil {
			t.Fatalf("CreateConnection() error = %v", err)
		}

		if conn.UID == uuid.Nil {
			t.Error("CreateConnection() conn.UID = uuid.Nil")
		}
		if conn.UserID != user.UID {
			t.Errorf("CreateConnection() conn.UserID = %s, want %s", conn.UserID, user.UID)
		}
		if conn.DatabaseID != database.UID {
			t.Errorf("CreateConnection() conn.DatabaseID = %s, want %s", conn.DatabaseID, database.UID)
		}
		// PostgreSQL INET type may include CIDR suffix
		if !strings.HasPrefix(conn.SourceIP, "192.168.1.100") {
			t.Errorf("CreateConnection() conn.SourceIP = %q, want prefix %q", conn.SourceIP, "192.168.1.100")
		}
		if conn.ConnectedAt.IsZero() {
			t.Error("CreateConnection() conn.ConnectedAt is zero")
		}
		if conn.DisconnectedAt != nil {
			t.Error("CreateConnection() conn.DisconnectedAt should be nil")
		}
	})

	t.Run("create connection with IPv6", func(t *testing.T) {
		conn, err := store.CreateConnection(ctx, user.UID, database.UID, "::1")
		if err != nil {
			t.Fatalf("CreateConnection() error = %v", err)
		}

		// PostgreSQL INET type may include CIDR suffix
		if !strings.HasPrefix(conn.SourceIP, "::1") {
			t.Errorf("CreateConnection() conn.SourceIP = %q, want prefix %q", conn.SourceIP, "::1")
		}
	})
}

func TestCloseConnection(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	user, database := createTestUserAndDatabase(t, ctx, store, "close")

	conn, err := store.CreateConnection(ctx, user.UID, database.UID, "10.0.0.1")
	if err != nil {
		t.Fatalf("CreateConnection() error = %v", err)
	}

	t.Run("close open connection", func(t *testing.T) {
		err := store.CloseConnection(ctx, conn.UID)
		if err != nil {
			t.Fatalf("CloseConnection() error = %v", err)
		}

		// Verify connection is closed by listing
		conns, err := store.ListConnections(ctx, ConnectionFilter{UserID: &user.UID})
		if err != nil {
			t.Fatalf("ListConnections() error = %v", err)
		}

		found := false
		for _, c := range conns {
			if c.UID == conn.UID {
				found = true
				if c.DisconnectedAt == nil {
					t.Error("conn.DisconnectedAt should not be nil after close")
				}
				break
			}
		}
		if !found {
			t.Error("connection not found after close")
		}
	})

	t.Run("close already closed connection", func(t *testing.T) {
		err := store.CloseConnection(ctx, conn.UID)
		if !errors.Is(err, ErrConnectionNotFound) {
			t.Errorf("CloseConnection() error = %v, want %v", err, ErrConnectionNotFound)
		}
	})

	t.Run("close non-existing connection", func(t *testing.T) {
		err := store.CloseConnection(ctx, uuid.New())
		if !errors.Is(err, ErrConnectionNotFound) {
			t.Errorf("CloseConnection() error = %v, want %v", err, ErrConnectionNotFound)
		}
	})
}

func TestListConnections(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	user1, db1 := createTestUserAndDatabase(t, ctx, store, "listc1")
	user2, db2 := createTestUserAndDatabase(t, ctx, store, "listc2")

	// Create connections
	_, err := store.CreateConnection(ctx, user1.UID, db1.UID, "10.0.0.1")
	if err != nil {
		t.Fatalf("CreateConnection() error = %v", err)
	}
	_, err = store.CreateConnection(ctx, user1.UID, db2.UID, "10.0.0.2")
	if err != nil {
		t.Fatalf("CreateConnection() error = %v", err)
	}
	_, err = store.CreateConnection(ctx, user2.UID, db1.UID, "10.0.0.3")
	if err != nil {
		t.Fatalf("CreateConnection() error = %v", err)
	}

	t.Run("list all", func(t *testing.T) {
		conns, err := store.ListConnections(ctx, ConnectionFilter{})
		if err != nil {
			t.Fatalf("ListConnections() error = %v", err)
		}
		if len(conns) != 3 {
			t.Errorf("ListConnections() len = %d, want 3", len(conns))
		}
	})

	t.Run("filter by user", func(t *testing.T) {
		conns, err := store.ListConnections(ctx, ConnectionFilter{UserID: &user1.UID})
		if err != nil {
			t.Fatalf("ListConnections() error = %v", err)
		}
		if len(conns) != 2 {
			t.Errorf("ListConnections() len = %d, want 2", len(conns))
		}
	})

	t.Run("filter by database", func(t *testing.T) {
		conns, err := store.ListConnections(ctx, ConnectionFilter{DatabaseID: &db1.UID})
		if err != nil {
			t.Fatalf("ListConnections() error = %v", err)
		}
		if len(conns) != 2 {
			t.Errorf("ListConnections() len = %d, want 2", len(conns))
		}
	})

	t.Run("with limit", func(t *testing.T) {
		conns, err := store.ListConnections(ctx, ConnectionFilter{Limit: 2})
		if err != nil {
			t.Fatalf("ListConnections() error = %v", err)
		}
		if len(conns) != 2 {
			t.Errorf("ListConnections() len = %d, want 2", len(conns))
		}
	})

	t.Run("with offset", func(t *testing.T) {
		conns, err := store.ListConnections(ctx, ConnectionFilter{Limit: 10, Offset: 2})
		if err != nil {
			t.Fatalf("ListConnections() error = %v", err)
		}
		if len(conns) != 1 {
			t.Errorf("ListConnections() len = %d, want 1", len(conns))
		}
	})

	t.Run("combined filters", func(t *testing.T) {
		conns, err := store.ListConnections(ctx, ConnectionFilter{UserID: &user1.UID, DatabaseID: &db1.UID})
		if err != nil {
			t.Fatalf("ListConnections() error = %v", err)
		}
		if len(conns) != 1 {
			t.Errorf("ListConnections() len = %d, want 1", len(conns))
		}
	})
}

func TestIncrementConnectionStats(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	user, database := createTestUserAndDatabase(t, ctx, store, "stats")

	conn, err := store.CreateConnection(ctx, user.UID, database.UID, "10.0.0.1")
	if err != nil {
		t.Fatalf("CreateConnection() error = %v", err)
	}

	// Verify initial values are zero
	if conn.Queries != 0 {
		t.Errorf("Initial conn.Queries = %d, want 0", conn.Queries)
	}
	if conn.BytesTransferred != 0 {
		t.Errorf("Initial conn.BytesTransferred = %d, want 0", conn.BytesTransferred)
	}

	t.Run("increment stats", func(t *testing.T) {
		err := store.IncrementConnectionStats(ctx, conn.UID, 1024)
		if err != nil {
			t.Fatalf("IncrementConnectionStats() error = %v", err)
		}

		// Fetch and verify
		conns, err := store.ListConnections(ctx, ConnectionFilter{UserID: &user.UID})
		if err != nil {
			t.Fatalf("ListConnections() error = %v", err)
		}

		var found *Connection
		for i := range conns {
			if conns[i].UID == conn.UID {
				found = &conns[i]
				break
			}
		}
		if found == nil {
			t.Fatal("connection not found")
		}

		if found.Queries != 1 {
			t.Errorf("conn.Queries = %d, want 1", found.Queries)
		}
		if found.BytesTransferred != 1024 {
			t.Errorf("conn.BytesTransferred = %d, want 1024", found.BytesTransferred)
		}
	})

	t.Run("multiple increments accumulate", func(t *testing.T) {
		err := store.IncrementConnectionStats(ctx, conn.UID, 2048)
		if err != nil {
			t.Fatalf("IncrementConnectionStats() error = %v", err)
		}
		err = store.IncrementConnectionStats(ctx, conn.UID, 512)
		if err != nil {
			t.Fatalf("IncrementConnectionStats() error = %v", err)
		}

		conns, err := store.ListConnections(ctx, ConnectionFilter{UserID: &user.UID})
		if err != nil {
			t.Fatalf("ListConnections() error = %v", err)
		}

		var found *Connection
		for i := range conns {
			if conns[i].UID == conn.UID {
				found = &conns[i]
				break
			}
		}
		if found == nil {
			t.Fatal("connection not found")
		}

		// 1 + 2 = 3 queries total
		if found.Queries != 3 {
			t.Errorf("conn.Queries = %d, want 3", found.Queries)
		}
		// 1024 + 2048 + 512 = 3584 bytes total
		if found.BytesTransferred != 3584 {
			t.Errorf("conn.BytesTransferred = %d, want 3584", found.BytesTransferred)
		}
	})
}

func TestExtractSourceIP(t *testing.T) {
	tests := []struct {
		name     string
		addr     net.Addr
		expected string
	}{
		{
			name:     "TCP IPv4",
			addr:     &net.TCPAddr{IP: net.ParseIP("192.168.1.1"), Port: 12345},
			expected: "192.168.1.1",
		},
		{
			name:     "TCP IPv6",
			addr:     &net.TCPAddr{IP: net.ParseIP("::1"), Port: 12345},
			expected: "::1",
		},
		{
			name:     "TCP IPv6 full",
			addr:     &net.TCPAddr{IP: net.ParseIP("2001:db8::1"), Port: 8080},
			expected: "2001:db8::1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ExtractSourceIP(tt.addr)
			if result != tt.expected {
				t.Errorf("ExtractSourceIP() = %q, want %q", result, tt.expected)
			}
		})
	}
}
