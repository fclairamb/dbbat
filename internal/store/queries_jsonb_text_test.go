package store

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStorableParameters(t *testing.T) {
	t.Parallel()

	require.Nil(t, storableParameters(nil))

	clean := &QueryParameters{Values: []string{"café", "€✓", ""}, Raw: []string{"YQA="}}
	require.Same(t, clean, storableParameters(clean), "valid values are kept as is")

	dirty := &QueryParameters{Values: []string{"ok", "ab\x00cd", "x\xff"}, Raw: []string{"YWIAY2Q="}}
	got := storableParameters(dirty)
	require.Equal(t, []string{"ok", "ab�cd", "x�"}, got.Values)
	require.Equal(t, dirty.Raw, got.Raw, "base64 raw bytes are untouched")
	require.Equal(t, "ab\x00cd", dirty.Values[1], "the caller's parameters are not mutated")
}

func TestStorableRowData(t *testing.T) {
	t.Parallel()

	require.Nil(t, storableRowData(nil))

	clean := json.RawMessage(`{"a":"café €✓","b":"\u00e9","c":"\\u0000 is literal"}`)
	require.Equal(t, string(clean), string(storableRowData(clean)), "no over-escaping")

	require.JSONEq(t, `["ab\ufffdcd","\\u0000",1]`,
		string(storableRowData(json.RawMessage(`["ab\u0000cd","\\u0000",1]`))))
	require.Equal(t, `["\\\ufffd"]`, string(storableRowData(json.RawMessage(`["\\\u0000"]`))),
		"an escaped backslash before a NUL escape is kept and the NUL repaired")
	require.Equal(t, "[\"x�\"]", string(storableRowData(json.RawMessage("[\"x\xff\"]"))))
}

// TestCreateQueryNULInParametersAndRows: a NUL in a bound value or a captured
// cell used to make Postgres refuse the jsonb insert, dropping an executed
// statement from the history and from the audit chain.
func TestCreateQueryNULInParametersAndRows(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	conn := createTestConnection(t, ctx, store, "nul-jsonb")

	created, err := store.CreateQuery(ctx, &Query{
		ConnectionID: conn.UID,
		SQLText:      "SELECT $1, $2",
		Parameters:   &QueryParameters{Values: []string{"ab\x00cd", "café €✓"}},
	})
	require.NoError(t, err)

	pending, err := store.CreatePendingQuery(ctx, &Query{
		ConnectionID: conn.UID,
		SQLText:      "DROP TABLE t",
		Parameters:   &QueryParameters{Values: []string{"x\x00"}},
	}, "drop")
	require.NoError(t, err)
	require.Equal(t, []string{"x�"}, pending.Parameters.Values)

	nulRow, err := json.Marshal([]any{"ab\x00cd", 1})
	require.NoError(t, err)

	cleanRow := json.RawMessage(`["café €✓",2]`)

	require.NoError(t, store.StoreQueryRows(ctx, []PendingQueryRow{
		{QueryID: created.UID, RowNumber: 1, RowData: nulRow, RowSizeBytes: int64(len(nulRow))},
		{QueryID: created.UID, RowNumber: 2, RowData: cleanRow, RowSizeBytes: int64(len(cleanRow))},
	}))

	got, err := store.GetQuery(ctx, created.UID)
	require.NoError(t, err)
	require.Equal(t, []string{"ab�cd", "café €✓"}, got.Parameters.Values)

	rows, err := store.GetQueryRows(ctx, created.UID, "", 10)
	require.NoError(t, err)
	require.Len(t, rows.Rows, 2)

	var first []any
	require.NoError(t, json.Unmarshal(rows.Rows[0].RowData, &first))
	require.Equal(t, "ab�cd", first[0])

	var second []any
	require.NoError(t, json.Unmarshal(rows.Rows[1].RowData, &second))
	require.Equal(t, "café €✓", second[0], "multi-byte text is stored unchanged")

	if store.ChainEnabled() {
		result, err := store.VerifyQueryChain(ctx, conn.UID)
		require.NoError(t, err)
		require.Nil(t, result.Break, "the repaired parameters are what the chain sealed")

		rowResult, err := store.VerifyRowChain(ctx, created.UID)
		require.NoError(t, err)
		require.Nil(t, rowResult.Break, "the repaired rows are what the chain sealed")
		require.Equal(t, int64(2), rowResult.Verified)
	}
}
