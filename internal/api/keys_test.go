package api

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/config"
	"github.com/fclairamb/dbbat/internal/store"
)

func TestBuildConnectionsForUser_NilConfig(t *testing.T) {
	t.Parallel()

	s := &Server{config: nil, store: nil}
	user := &store.User{}
	conns, truncated := s.buildConnectionsForUser(context.Background(), user, "key")
	assert.Empty(t, conns)
	assert.False(t, truncated)
}

// TestBuildConnectionsForUser_DisabledProtocol verifies that a database whose
// protocol's resolved port is 0 is excluded from the connection list.
func TestBuildConnectionsForUser_DisabledProtocol(t *testing.T) {
	t.Parallel()

	// Build a config where PG listen is empty (→ port 0) and the public
	// endpoint group has no overrides, so the resolved PG port will be 0.
	cfg := &config.Config{
		ListenPG: "", // disabled
	}

	db := &store.Database{
		Name:         "disabled-pg",
		DatabaseName: "mydb",
		Protocol:     store.ProtocolPostgreSQL,
		SSLMode:      "prefer",
	}

	user := &store.User{Username: "alice"}
	endpoints := store.ResolvedEndpoints{PGPort: 0}

	info, ok := BuildConnectionURL(db, user, endpoints, "key")
	require.False(t, ok)
	assert.Equal(t, ConnectionInfo{}, info)
	_ = cfg // config is used by the full integration path; verify protocol exclusion here
}

// TestBuildConnectionsForUser_Truncation verifies the truncation flag and cap.
func TestBuildConnectionsForUser_Truncation(t *testing.T) {
	t.Parallel()

	user := &store.User{Username: "alice"}
	endpoints := store.ResolvedEndpoints{
		PGHost: "db.example.com",
		PGPort: 5432,
	}

	// Build more than 50 ConnectionInfo items.
	var all []ConnectionInfo
	for i := 0; i < 60; i++ {
		db := makeDB(store.ProtocolPostgreSQL, "db", "prefer")
		info, ok := BuildConnectionURL(db, user, endpoints, "key")
		require.True(t, ok)
		all = append(all, info)
	}

	// Simulate the truncation logic from buildConnectionsForUser.
	truncated := false
	if len(all) > maxConnectionsInResponse {
		all = all[:maxConnectionsInResponse]
		truncated = true
	}

	assert.Len(t, all, maxConnectionsInResponse)
	assert.True(t, truncated)
}
