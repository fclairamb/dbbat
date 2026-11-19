package store

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
)

func TestStorableSQLText(t *testing.T) {
	t.Parallel()

	require.Equal(t, "SELECT 'café'", storableSQLText("SELECT 'café'"), "valid text is unchanged")
	require.Equal(t, "SELECT 'caf�'", storableSQLText("SELECT 'caf\xc3'"), "a split rune is repaired")
	require.Equal(t, "a�b", storableSQLText("a\x00b"), "a NUL byte is replaced")
}

// TestCreateQueryInvalidUTF8 is the store-side half of the MongoDB truncation
// fix: client-chosen bytes that are not valid UTF-8 (or carry a NUL) must not
// make the insert fail, or the statement vanishes from the query history.
func TestCreateQueryInvalidUTF8(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	conn := createTestConnection(t, ctx, store, "invalid-utf8")

	// What a blind 8 KB cut leaves behind: the first byte of "é".
	raw := "find " + strings.Repeat("a", 10) + "\xc3"

	created, err := store.CreateQuery(ctx, &Query{ConnectionID: conn.UID, SQLText: raw})
	require.NoError(t, err)
	require.True(t, utf8.ValidString(created.SQLText))

	pending, err := store.CreatePendingQuery(ctx, &Query{ConnectionID: conn.UID, SQLText: "drop\x00 \xff"}, "drop")
	require.NoError(t, err)
	require.Equal(t, "drop� �", pending.SQLText)

	got, err := store.GetQuery(ctx, created.UID)
	require.NoError(t, err)
	require.Equal(t, created.SQLText, got.SQLText)

	if store.ChainEnabled() {
		result, err := store.VerifyQueryChain(ctx, conn.UID)
		require.NoError(t, err)
		require.Nil(t, result.Break, "the repaired text is what the chain sealed")
	}
}
