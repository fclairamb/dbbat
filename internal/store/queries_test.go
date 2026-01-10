package store

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
)

// createTestConnection creates a user, database, and connection for query testing.
func createTestConnection(t *testing.T, ctx context.Context, store *Store, suffix string) *Connection {
	t.Helper()

	user, database := createTestUserAndDatabase(t, ctx, store, suffix)

	conn, err := store.CreateConnection(ctx, user.UID, database.UID, "127.0.0.1")
	if err != nil {
		t.Fatalf("CreateConnection() error = %v", err)
	}

	return conn
}

func TestCreateQuery(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	conn := createTestConnection(t, ctx, store, "query")

	t.Run("create query without results", func(t *testing.T) {
		duration := 15.5
		rowsAffected := int64(10)
		query := &Query{
			ConnectionID: conn.UID,
			SQLText:      "UPDATE users SET active = true",
			ExecutedAt:   time.Now(),
			DurationMs:   &duration,
			RowsAffected: &rowsAffected,
		}

		created, err := store.CreateQuery(ctx, query)
		if err != nil {
			t.Fatalf("CreateQuery() error = %v", err)
		}

		if created.UID == uuid.Nil {
			t.Error("CreateQuery() query.UID = uuid.Nil")
		}
		if created.SQLText != "UPDATE users SET active = true" {
			t.Errorf("CreateQuery() query.SQLText = %q", created.SQLText)
		}
		if created.DurationMs == nil || *created.DurationMs != 15.5 {
			t.Errorf("CreateQuery() query.DurationMs = %v, want 15.5", created.DurationMs)
		}
		if created.RowsAffected == nil || *created.RowsAffected != 10 {
			t.Errorf("CreateQuery() query.RowsAffected = %v, want 10", created.RowsAffected)
		}
		if created.Error != nil {
			t.Errorf("CreateQuery() query.Error = %v, want nil", created.Error)
		}
	})

	t.Run("create query with error", func(t *testing.T) {
		errorMsg := "relation does not exist"
		query := &Query{
			ConnectionID: conn.UID,
			SQLText:      "SELECT * FROM nonexistent",
			ExecutedAt:   time.Now(),
			Error:        &errorMsg,
		}

		created, err := store.CreateQuery(ctx, query)
		if err != nil {
			t.Fatalf("CreateQuery() error = %v", err)
		}

		if created.Error == nil || *created.Error != "relation does not exist" {
			t.Errorf("CreateQuery() query.Error = %v, want %q", created.Error, "relation does not exist")
		}
	})
}

func TestStoreQueryRows(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	conn := createTestConnection(t, ctx, store, "rows")

	query := &Query{
		ConnectionID: conn.UID,
		SQLText:      "SELECT id, name FROM items",
		ExecutedAt:   time.Now(),
	}
	created, err := store.CreateQuery(ctx, query)
	if err != nil {
		t.Fatalf("CreateQuery() error = %v", err)
	}

	t.Run("store multiple rows", func(t *testing.T) {
		rows := []QueryRow{
			{RowNumber: 1, RowData: json.RawMessage(`{"id": 1, "name": "item1"}`), RowSizeBytes: 30},
			{RowNumber: 2, RowData: json.RawMessage(`{"id": 2, "name": "item2"}`), RowSizeBytes: 30},
			{RowNumber: 3, RowData: json.RawMessage(`{"id": 3, "name": "item3"}`), RowSizeBytes: 30},
		}

		err := store.StoreQueryRows(ctx, created.UID, rows)
		if err != nil {
			t.Fatalf("StoreQueryRows() error = %v", err)
		}

		// Verify rows are stored
		result, err := store.GetQueryWithRows(ctx, created.UID)
		if err != nil {
			t.Fatalf("GetQueryWithRows() error = %v", err)
		}

		if len(result.Rows) != 3 {
			t.Errorf("GetQueryWithRows() len(rows) = %d, want 3", len(result.Rows))
		}
	})

	t.Run("store empty rows", func(t *testing.T) {
		query2 := &Query{
			ConnectionID: conn.UID,
			SQLText:      "SELECT * FROM empty_table",
			ExecutedAt:   time.Now(),
		}
		created2, err := store.CreateQuery(ctx, query2)
		if err != nil {
			t.Fatalf("CreateQuery() error = %v", err)
		}

		err = store.StoreQueryRows(ctx, created2.UID, []QueryRow{})
		if err != nil {
			t.Fatalf("StoreQueryRows() error = %v", err)
		}
	})
}

func TestListQueries(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	conn1 := createTestConnection(t, ctx, store, "listq1")
	conn2 := createTestConnection(t, ctx, store, "listq2")

	// Create queries with different times
	now := time.Now()
	queries := []*Query{
		{ConnectionID: conn1.UID, SQLText: "SELECT 1", ExecutedAt: now.Add(-2 * time.Hour)},
		{ConnectionID: conn1.UID, SQLText: "SELECT 2", ExecutedAt: now.Add(-1 * time.Hour)},
		{ConnectionID: conn2.UID, SQLText: "SELECT 3", ExecutedAt: now},
	}

	for _, q := range queries {
		_, err := store.CreateQuery(ctx, q)
		if err != nil {
			t.Fatalf("CreateQuery() error = %v", err)
		}
	}

	t.Run("list all", func(t *testing.T) {
		result, err := store.ListQueries(ctx, QueryFilter{})
		if err != nil {
			t.Fatalf("ListQueries() error = %v", err)
		}
		if len(result) != 3 {
			t.Errorf("ListQueries() len = %d, want 3", len(result))
		}
	})

	t.Run("filter by connection", func(t *testing.T) {
		result, err := store.ListQueries(ctx, QueryFilter{ConnectionID: &conn1.UID})
		if err != nil {
			t.Fatalf("ListQueries() error = %v", err)
		}
		if len(result) != 2 {
			t.Errorf("ListQueries() len = %d, want 2", len(result))
		}
	})

	t.Run("filter by time range", func(t *testing.T) {
		startTime := now.Add(-90 * time.Minute)
		endTime := now.Add(-30 * time.Minute)
		result, err := store.ListQueries(ctx, QueryFilter{StartTime: &startTime, EndTime: &endTime})
		if err != nil {
			t.Fatalf("ListQueries() error = %v", err)
		}
		if len(result) != 1 {
			t.Errorf("ListQueries() len = %d, want 1", len(result))
		}
	})

	t.Run("with limit", func(t *testing.T) {
		result, err := store.ListQueries(ctx, QueryFilter{Limit: 2})
		if err != nil {
			t.Fatalf("ListQueries() error = %v", err)
		}
		if len(result) != 2 {
			t.Errorf("ListQueries() len = %d, want 2", len(result))
		}
	})

	t.Run("with offset", func(t *testing.T) {
		result, err := store.ListQueries(ctx, QueryFilter{Limit: 10, Offset: 2})
		if err != nil {
			t.Fatalf("ListQueries() error = %v", err)
		}
		if len(result) != 1 {
			t.Errorf("ListQueries() len = %d, want 1", len(result))
		}
	})
}

func TestGetQueryWithRows(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	conn := createTestConnection(t, ctx, store, "getqr")

	duration := 5.25
	rowsAffected := int64(3)
	query := &Query{
		ConnectionID: conn.UID,
		SQLText:      "SELECT id, value FROM data",
		ExecutedAt:   time.Now(),
		DurationMs:   &duration,
		RowsAffected: &rowsAffected,
	}
	created, err := store.CreateQuery(ctx, query)
	if err != nil {
		t.Fatalf("CreateQuery() error = %v", err)
	}

	rows := []QueryRow{
		{RowNumber: 1, RowData: json.RawMessage(`{"id": 1, "value": "a"}`), RowSizeBytes: 25},
		{RowNumber: 2, RowData: json.RawMessage(`{"id": 2, "value": "b"}`), RowSizeBytes: 25},
		{RowNumber: 3, RowData: json.RawMessage(`{"id": 3, "value": "c"}`), RowSizeBytes: 25},
	}
	err = store.StoreQueryRows(ctx, created.UID, rows)
	if err != nil {
		t.Fatalf("StoreQueryRows() error = %v", err)
	}

	t.Run("get query with rows", func(t *testing.T) {
		result, err := store.GetQueryWithRows(ctx, created.UID)
		if err != nil {
			t.Fatalf("GetQueryWithRows() error = %v", err)
		}

		if result.UID != created.UID {
			t.Errorf("GetQueryWithRows() query.UID = %s, want %s", result.UID, created.UID)
		}
		if result.SQLText != "SELECT id, value FROM data" {
			t.Errorf("GetQueryWithRows() query.SQLText = %q", result.SQLText)
		}
		if result.DurationMs == nil || *result.DurationMs != 5.25 {
			t.Errorf("GetQueryWithRows() query.DurationMs = %v, want 5.25", result.DurationMs)
		}

		if len(result.Rows) != 3 {
			t.Fatalf("GetQueryWithRows() len(rows) = %d, want 3", len(result.Rows))
		}

		// Rows should be ordered by row_number
		for i, row := range result.Rows {
			if row.RowNumber != i+1 {
				t.Errorf("row[%d].RowNumber = %d, want %d", i, row.RowNumber, i+1)
			}
			if row.RowSizeBytes != 25 {
				t.Errorf("row[%d].RowSizeBytes = %d, want 25", i, row.RowSizeBytes)
			}
		}
	})

	t.Run("get query without rows", func(t *testing.T) {
		query2 := &Query{
			ConnectionID: conn.UID,
			SQLText:      "DELETE FROM old_data",
			ExecutedAt:   time.Now(),
		}
		created2, err := store.CreateQuery(ctx, query2)
		if err != nil {
			t.Fatalf("CreateQuery() error = %v", err)
		}

		result, err := store.GetQueryWithRows(ctx, created2.UID)
		if err != nil {
			t.Fatalf("GetQueryWithRows() error = %v", err)
		}

		if len(result.Rows) != 0 {
			t.Errorf("GetQueryWithRows() len(rows) = %d, want 0", len(result.Rows))
		}
	})
}

func TestGetQueryRows(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	conn := createTestConnection(t, ctx, store, "getqrows")

	// Create a query with many rows for pagination testing
	query := &Query{
		ConnectionID: conn.UID,
		SQLText:      "SELECT * FROM large_table",
		ExecutedAt:   time.Now(),
	}
	created, err := store.CreateQuery(ctx, query)
	if err != nil {
		t.Fatalf("CreateQuery() error = %v", err)
	}

	// Store 15 rows
	rows := make([]QueryRow, 15)
	for i := range rows {
		rows[i] = QueryRow{
			RowNumber:    i + 1,
			RowData:      json.RawMessage(`{"id": ` + string(rune('0'+i%10)) + `}`),
			RowSizeBytes: 10,
		}
	}
	err = store.StoreQueryRows(ctx, created.UID, rows)
	if err != nil {
		t.Fatalf("StoreQueryRows() error = %v", err)
	}

	t.Run("get first page", func(t *testing.T) {
		result, err := store.GetQueryRows(ctx, created.UID, "", 5)
		if err != nil {
			t.Fatalf("GetQueryRows() error = %v", err)
		}

		if len(result.Rows) != 5 {
			t.Errorf("GetQueryRows() len(rows) = %d, want 5", len(result.Rows))
		}
		if result.TotalRows != 15 {
			t.Errorf("GetQueryRows() TotalRows = %d, want 15", result.TotalRows)
		}
		if !result.HasMore {
			t.Error("GetQueryRows() HasMore = false, want true")
		}
		if result.NextCursor == "" {
			t.Error("GetQueryRows() NextCursor is empty, want non-empty")
		}

		// Verify first row
		if result.Rows[0].RowNumber != 1 {
			t.Errorf("GetQueryRows() first row number = %d, want 1", result.Rows[0].RowNumber)
		}
	})

	t.Run("get second page with cursor", func(t *testing.T) {
		// Get first page to get cursor
		firstPage, err := store.GetQueryRows(ctx, created.UID, "", 5)
		if err != nil {
			t.Fatalf("GetQueryRows() first page error = %v", err)
		}

		// Get second page using cursor
		result, err := store.GetQueryRows(ctx, created.UID, firstPage.NextCursor, 5)
		if err != nil {
			t.Fatalf("GetQueryRows() error = %v", err)
		}

		if len(result.Rows) != 5 {
			t.Errorf("GetQueryRows() len(rows) = %d, want 5", len(result.Rows))
		}
		if !result.HasMore {
			t.Error("GetQueryRows() HasMore = false, want true")
		}

		// Verify first row of second page
		if result.Rows[0].RowNumber != 6 {
			t.Errorf("GetQueryRows() first row number = %d, want 6", result.Rows[0].RowNumber)
		}
	})

	t.Run("get last page", func(t *testing.T) {
		// Get first two pages
		page1, _ := store.GetQueryRows(ctx, created.UID, "", 5)
		page2, _ := store.GetQueryRows(ctx, created.UID, page1.NextCursor, 5)

		// Get last page
		result, err := store.GetQueryRows(ctx, created.UID, page2.NextCursor, 5)
		if err != nil {
			t.Fatalf("GetQueryRows() error = %v", err)
		}

		if len(result.Rows) != 5 {
			t.Errorf("GetQueryRows() len(rows) = %d, want 5", len(result.Rows))
		}
		if result.HasMore {
			t.Error("GetQueryRows() HasMore = true, want false")
		}
		if result.NextCursor != "" {
			t.Errorf("GetQueryRows() NextCursor = %q, want empty", result.NextCursor)
		}

		// Verify first row of last page
		if result.Rows[0].RowNumber != 11 {
			t.Errorf("GetQueryRows() first row number = %d, want 11", result.Rows[0].RowNumber)
		}
	})

	t.Run("default limit", func(t *testing.T) {
		result, err := store.GetQueryRows(ctx, created.UID, "", 0)
		if err != nil {
			t.Fatalf("GetQueryRows() error = %v", err)
		}

		if len(result.Rows) != 15 {
			t.Errorf("GetQueryRows() len(rows) = %d, want 15 (using default limit)", len(result.Rows))
		}
	})

	t.Run("limit capped at max", func(t *testing.T) {
		result, err := store.GetQueryRows(ctx, created.UID, "", 2000)
		if err != nil {
			t.Fatalf("GetQueryRows() error = %v", err)
		}

		// Should return all 15 rows (less than capped limit)
		if len(result.Rows) != 15 {
			t.Errorf("GetQueryRows() len(rows) = %d, want 15", len(result.Rows))
		}
	})

	t.Run("query not found", func(t *testing.T) {
		nonExistentUID := uuid.New()
		_, err := store.GetQueryRows(ctx, nonExistentUID, "", 10)
		if err != ErrQueryNotFound {
			t.Errorf("GetQueryRows() error = %v, want ErrQueryNotFound", err)
		}
	})

	t.Run("invalid cursor", func(t *testing.T) {
		_, err := store.GetQueryRows(ctx, created.UID, "invalid-cursor", 10)
		if err != ErrInvalidCursor {
			t.Errorf("GetQueryRows() error = %v, want ErrInvalidCursor", err)
		}
	})

	t.Run("invalid cursor json", func(t *testing.T) {
		// Valid base64 but invalid JSON
		_, err := store.GetQueryRows(ctx, created.UID, "bm90LWpzb24=", 10)
		if err != ErrInvalidCursor {
			t.Errorf("GetQueryRows() error = %v, want ErrInvalidCursor", err)
		}
	})

	t.Run("empty result", func(t *testing.T) {
		// Create a query without rows
		query2 := &Query{
			ConnectionID: conn.UID,
			SQLText:      "SELECT * FROM empty_table",
			ExecutedAt:   time.Now(),
		}
		created2, err := store.CreateQuery(ctx, query2)
		if err != nil {
			t.Fatalf("CreateQuery() error = %v", err)
		}

		result, err := store.GetQueryRows(ctx, created2.UID, "", 10)
		if err != nil {
			t.Fatalf("GetQueryRows() error = %v", err)
		}

		if len(result.Rows) != 0 {
			t.Errorf("GetQueryRows() len(rows) = %d, want 0", len(result.Rows))
		}
		if result.TotalRows != 0 {
			t.Errorf("GetQueryRows() TotalRows = %d, want 0", result.TotalRows)
		}
		if result.HasMore {
			t.Error("GetQueryRows() HasMore = true, want false")
		}
	})
}

func TestGetQueryRowsDataSizeLimit(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	conn := createTestConnection(t, ctx, store, "datasize")

	query := &Query{
		ConnectionID: conn.UID,
		SQLText:      "SELECT * FROM large_rows",
		ExecutedAt:   time.Now(),
	}
	created, err := store.CreateQuery(ctx, query)
	if err != nil {
		t.Fatalf("CreateQuery() error = %v", err)
	}

	// Create rows with large data (each ~100KB)
	largeData := make([]byte, 100*1024)
	for i := range largeData {
		largeData[i] = 'x'
	}
	jsonData := json.RawMessage(`{"data": "` + string(largeData) + `"}`)

	rows := make([]QueryRow, 20)
	for i := range rows {
		rows[i] = QueryRow{
			RowNumber:    i + 1,
			RowData:      jsonData,
			RowSizeBytes: int64(len(jsonData)),
		}
	}
	err = store.StoreQueryRows(ctx, created.UID, rows)
	if err != nil {
		t.Fatalf("StoreQueryRows() error = %v", err)
	}

	t.Run("data size limit stops iteration", func(t *testing.T) {
		// Request 20 rows, but 1MB limit should stop us earlier
		// 100KB per row * 10 rows = 1MB
		result, err := store.GetQueryRows(ctx, created.UID, "", 20)
		if err != nil {
			t.Fatalf("GetQueryRows() error = %v", err)
		}

		// Should get fewer than 20 rows due to 1MB limit
		// With ~100KB per row, we should get about 10 rows
		if len(result.Rows) >= 20 {
			t.Errorf("GetQueryRows() len(rows) = %d, expected less than 20 due to data size limit", len(result.Rows))
		}
		if !result.HasMore {
			t.Error("GetQueryRows() HasMore = false, want true (more rows exist)")
		}
	})

	t.Run("at least one row returned even if over limit", func(t *testing.T) {
		// Create a query with a single huge row
		query2 := &Query{
			ConnectionID: conn.UID,
			SQLText:      "SELECT * FROM huge_row",
			ExecutedAt:   time.Now(),
		}
		created2, err := store.CreateQuery(ctx, query2)
		if err != nil {
			t.Fatalf("CreateQuery() error = %v", err)
		}

		// Create a row larger than 1MB
		hugeData := make([]byte, 2*1024*1024)
		for i := range hugeData {
			hugeData[i] = 'y'
		}
		hugeRows := []QueryRow{
			{RowNumber: 1, RowData: json.RawMessage(`{"data": "` + string(hugeData) + `"}`), RowSizeBytes: int64(len(hugeData))},
			{RowNumber: 2, RowData: json.RawMessage(`{"id": 2}`), RowSizeBytes: 10},
		}
		err = store.StoreQueryRows(ctx, created2.UID, hugeRows)
		if err != nil {
			t.Fatalf("StoreQueryRows() error = %v", err)
		}

		result, err := store.GetQueryRows(ctx, created2.UID, "", 10)
		if err != nil {
			t.Fatalf("GetQueryRows() error = %v", err)
		}

		// Should return at least 1 row even though it exceeds the limit
		if len(result.Rows) < 1 {
			t.Errorf("GetQueryRows() len(rows) = %d, want at least 1", len(result.Rows))
		}
	})
}
