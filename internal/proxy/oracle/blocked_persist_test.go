package oracle

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/store"
)

// awaitCreated waits for the asynchronous insert of a blocked-statement row.
func (r *recordingCompletionStore) awaitCreated(t *testing.T) *store.Query {
	t.Helper()

	select {
	case got := <-r.created:
		return got
	case <-time.After(5 * time.Second):
		t.Fatal("the refused statement was never written to the store")

		return nil
	}
}

// assertNoFurtherCreate fails if a second row shows up for one refusal.
func (r *recordingCompletionStore) assertNoFurtherCreate(t *testing.T) {
	t.Helper()

	select {
	case extra := <-r.created:
		t.Fatalf("a refused statement must write exactly one row, got a second: %q", extra.SQLText)
	case <-time.After(250 * time.Millisecond):
	}
}

// TestBlockedStatements_ArePersisted is the Oracle half of spec
// 2026-08-09-log-blocked-statements-pg-oracle: a statement refused by
// read_only or block_ddl used to leave nothing behind but an slog WARN. It now
// writes a queries row carrying the refusal as `error`, with duration_ms and
// rows_affected at 0 because the statement never reached upstream.
//
// Every statement-carrying TTC op is covered, because they refuse
// independently: legacy OALL8, the v315+ piggyback exec, the JDBC thin
// driver's func=0x11 exec, and a re-execution re-gated against its cursor.
func TestBlockedStatements_ArePersisted(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		controls []string
		sql      string
		wantErr  string
		payload  func(sql string) []byte
	}{
		{
			name:     "OALL8 write under read_only",
			controls: []string{store.ControlReadOnly},
			sql:      "INSERT INTO emp (id) VALUES (1)",
			wantErr:  "read-only",
			payload:  func(sql string) []byte { return buildOALL8(sql, nil, 7) },
		},
		{
			name:     "OALL8 CREATE TABLE under block_ddl",
			controls: []string{store.ControlBlockDDL},
			sql:      "CREATE TABLE blocked_ddl (id NUMBER)",
			wantErr:  "DDL",
			payload:  func(sql string) []byte { return buildOALL8(sql, nil, 7) },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := newTestSession(&store.Grant{Definition: &store.GrantDefinition{Controls: tt.controls}})
			recorder := newRecordingCompletionStore()
			s.completionStore = recorder

			require.Error(t, s.handleOALL8(tt.payload(tt.sql)), "the statement must be refused")

			got := recorder.awaitCreated(t)
			assertBlockedOracleRow(t, got, tt.sql, tt.wantErr)
			assert.Equal(t, s.connectionUID, got.ConnectionID,
				"a refusal row must still be attributed to the connection")

			// A refused statement is not in flight: nothing is left pending
			// that a later response could complete into a second row.
			assert.Nil(t, s.tracker.pendingQuery)
			recorder.assertNoFurtherCreate(t)
		})
	}
}

// TestBlockedCursorReexecution_IsPersisted covers the path a client takes when
// it re-runs a statement it already parsed: the SQL rides no frame of its own,
// so the refusal has to be recorded against the SQL the cursor holds.
func TestBlockedCursorReexecution_IsPersisted(t *testing.T) {
	t.Parallel()

	const sql = "UPDATE emp SET id = 2"

	s := newTestSession(&store.Grant{Definition: &store.GrantDefinition{}})
	recorder := newRecordingCompletionStore()
	s.completionStore = recorder

	// Parse it while the grant still allows writes, so the cursor is tracked.
	require.NoError(t, s.handleOALL8(buildOALL8(sql, nil, 11)))

	// Drain the row the successful parse inserted (persistQueryRecord goes
	// through s.store, which is nil here, so nothing is queued — but be
	// explicit about starting from an empty channel).
	require.Empty(t, recorder.created)

	// Narrow the grant, then re-execute the very same cursor.
	s.grant = &store.Grant{Definition: &store.GrantDefinition{Controls: []string{store.ControlReadOnly}}}

	require.Error(t, s.handleCursorReexec(11), "the re-execution must be refused")

	got := recorder.awaitCreated(t)
	assertBlockedOracleRow(t, got, sql, "read-only")
	recorder.assertNoFurtherCreate(t)
}

// assertBlockedOracleRow checks the shape every refusal row shares across the
// five protocols.
func assertBlockedOracleRow(t *testing.T, row *store.Query, wantSQL, wantErrFragment string) {
	t.Helper()

	require.NotNil(t, row)
	assert.Equal(t, wantSQL, row.SQLText)
	require.NotNil(t, row.Error, "a refused statement must be persisted with its refusal as error")
	assert.Contains(t, *row.Error, wantErrFragment)
	require.NotNil(t, row.DurationMs)
	assert.InDelta(t, 0, *row.DurationMs, 0.001, "a refused statement never ran")
	require.NotNil(t, row.RowsAffected)
	assert.Equal(t, int64(0), *row.RowsAffected)
}
