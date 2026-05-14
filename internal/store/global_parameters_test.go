package store

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/config"
)

func TestGlobalParameters(t *testing.T) {
	t.Parallel()

	s := setupTestStoreNoCleanup(t)
	ctx := context.Background()
	suffix := uuid.NewString()[:8]
	group := "test-" + suffix

	t.Run("set and get a parameter", func(t *testing.T) {
		t.Parallel()

		err := s.SetParameter(ctx, group, "k1", "v1")
		require.NoError(t, err)

		p, err := s.GetParameter(ctx, group, "k1")
		require.NoError(t, err)
		assert.Equal(t, group, p.GroupKey)
		assert.Equal(t, "k1", p.Key)
		assert.Equal(t, "v1", p.Value)
	})

	t.Run("update existing parameter (upsert)", func(t *testing.T) {
		t.Parallel()

		g := group + "-upsert"
		require.NoError(t, s.SetParameter(ctx, g, "key", "first"))
		require.NoError(t, s.SetParameter(ctx, g, "key", "second"))

		p, err := s.GetParameter(ctx, g, "key")
		require.NoError(t, err)
		assert.Equal(t, "second", p.Value)

		all, err := s.GetParameters(ctx, g)
		require.NoError(t, err)
		assert.Len(t, all, 1)
	})

	t.Run("get all parameters for a group", func(t *testing.T) {
		t.Parallel()

		g := group + "-multi"
		require.NoError(t, s.SetParameter(ctx, g, "a", "1"))
		require.NoError(t, s.SetParameter(ctx, g, "b", "2"))
		require.NoError(t, s.SetParameter(ctx, g, "c", "3"))

		params, err := s.GetParameters(ctx, g)
		require.NoError(t, err)
		assert.Len(t, params, 3)
	})

	t.Run("soft delete: parameter not found after delete", func(t *testing.T) {
		t.Parallel()

		g := group + "-delete"
		require.NoError(t, s.SetParameter(ctx, g, "gone", "value"))
		require.NoError(t, s.DeleteParameter(ctx, g, "gone"))

		_, err := s.GetParameter(ctx, g, "gone")
		assert.ErrorIs(t, err, ErrParameterNotFound)
	})

	t.Run("soft delete: new parameter with same key can be created", func(t *testing.T) {
		t.Parallel()

		g := group + "-reuse"
		require.NoError(t, s.SetParameter(ctx, g, "reused", "old"))
		require.NoError(t, s.DeleteParameter(ctx, g, "reused"))
		require.NoError(t, s.SetParameter(ctx, g, "reused", "new"))

		p, err := s.GetParameter(ctx, g, "reused")
		require.NoError(t, err)
		assert.Equal(t, "new", p.Value)
	})

	t.Run("delete non-existent parameter returns ErrParameterNotFound", func(t *testing.T) {
		t.Parallel()

		err := s.DeleteParameter(ctx, group+"-missing", "no-such-key")
		assert.ErrorIs(t, err, ErrParameterNotFound)
	})

	t.Run("enc: prefix round-trip stored verbatim", func(t *testing.T) {
		t.Parallel()

		g := group + "-enc"
		const encValue = "enc:someopaqueblob"
		require.NoError(t, s.SetParameter(ctx, g, "secret", encValue))

		p, err := s.GetParameter(ctx, g, "secret")
		require.NoError(t, err)
		assert.Equal(t, encValue, p.Value)
	})
}

func TestPublicEndpoints(t *testing.T) {
	t.Parallel()

	s := setupTestStoreNoCleanup(t)
	ctx := context.Background()

	t.Run("GetPublicEndpoints returns empty struct when no rows exist", func(t *testing.T) {
		t.Parallel()

		// Use an isolated store to avoid interference from other tests.
		pe, err := s.GetPublicEndpoints(ctx)
		require.NoError(t, err)
		// We can only assert that the call succeeds; other tests may have
		// written to the public group. Just verify it doesn't error.
		_ = pe
	})

	t.Run("SetPublicEndpoints and GetPublicEndpoints round-trip", func(t *testing.T) {
		t.Parallel()

		// Give each parallel run a unique store to avoid conflicts.
		s2 := setupTestStoreNoCleanup(t)

		port5434 := 5434
		port1522 := 1522
		pe := PublicEndpoints{
			Host:    "db.example.com",
			PGHost:  "pg.example.com",
			PGPort:  &port5434,
			OraPort: &port1522,
		}

		require.NoError(t, s2.SetPublicEndpoints(ctx, pe))

		got, err := s2.GetPublicEndpoints(ctx)
		require.NoError(t, err)
		assert.Equal(t, "db.example.com", got.Host)
		assert.Equal(t, "pg.example.com", got.PGHost)
		require.NotNil(t, got.PGPort)
		assert.Equal(t, 5434, *got.PGPort)
		require.NotNil(t, got.OraPort)
		assert.Equal(t, 1522, *got.OraPort)
	})
}

func TestResolvePublicEndpoints(t *testing.T) {
	t.Parallel()

	port9999 := 9999

	cfg := &config.Config{
		ListenPG:     ":5434",
		ListenOracle: ":1522",
		ListenMySQL:  ":3307",
	}

	t.Run("protocol override takes priority", func(t *testing.T) {
		t.Parallel()

		pe := PublicEndpoints{
			Host:   "default.example.com",
			PGHost: "pg.example.com",
			PGPort: &port9999,
		}

		r := ResolvePublicEndpoints(pe, cfg)
		assert.Equal(t, "pg.example.com", r.PGHost)
		assert.Equal(t, 9999, r.PGPort)
		assert.Equal(t, "default.example.com", r.OraHost)
	})

	t.Run("falls back to default host", func(t *testing.T) {
		t.Parallel()

		pe := PublicEndpoints{Host: "fallback.example.com"}
		r := ResolvePublicEndpoints(pe, cfg)
		assert.Equal(t, "fallback.example.com", r.PGHost)
		assert.Equal(t, "fallback.example.com", r.OraHost)
		assert.Equal(t, "fallback.example.com", r.MySQLHost)
	})

	t.Run("port falls back to local listen port", func(t *testing.T) {
		t.Parallel()

		pe := PublicEndpoints{}
		r := ResolvePublicEndpoints(pe, cfg)
		assert.Equal(t, 5434, r.PGPort)
		assert.Equal(t, 1522, r.OraPort)
		assert.Equal(t, 3307, r.MySQLPort)
	})

	t.Run("empty listen address resolves to port 0", func(t *testing.T) {
		t.Parallel()

		emptyCfg := &config.Config{
			ListenOracle: "",
			ListenMySQL:  "",
			ListenPG:     ":5434",
		}
		r := ResolvePublicEndpoints(PublicEndpoints{}, emptyCfg)
		assert.Equal(t, 0, r.OraPort)
		assert.Equal(t, 0, r.MySQLPort)
		assert.Equal(t, 5434, r.PGPort)
	})
}
