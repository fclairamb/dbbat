package postgresql

import (
	"testing"

	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func rowDesc(name string, oid uint32) *pgproto3.RowDescription {
	return &pgproto3.RowDescription{Fields: []pgproto3.FieldDescription{{Name: []byte(name), DataTypeOID: oid}}}
}

// pgx describes a statement in its own exchange, then only sends Bind/Execute:
// the Execute's batch carries no RowDescription.
func TestDescribeCapture_StatementDescribedInEarlierBatch(t *testing.T) {
	t.Parallel()

	s := newTestSession("write")
	s.extendedState.preparedStatements["stmt1"] = &preparedStatement{sql: "SELECT id FROM t"}

	// Batch 1: Parse/Describe(S)/Sync.
	s.handleDescribe(&pgproto3.Describe{ObjectType: 'S', Name: "stmt1"})
	s.captureRowDescription(rowDesc("id", 23))

	// Batch 2: Bind/Execute/Sync, binary result.
	s.handleBind(&pgproto3.Bind{PreparedStatement: "stmt1", ResultFormatCodes: []int16{1}})
	require.NoError(t, s.handleExecute(&pgproto3.Execute{Portal: ""}))

	q := s.extendedState.pendingQueries[0]
	assert.Equal(t, []string{"id"}, q.columnNames)
	assert.Equal(t, []uint32{23}, q.columnOIDs)

	row := s.convertDataRow([][]byte{{0, 0, 0, 42}}, q.columnNames, q.columnOIDs, q.resultFormats...)
	assert.JSONEq(t, `{"id":42}`, string(row.RowData))
}

// A portal paged with Execute(maxRows) is described once.
func TestDescribeCapture_PortalPagedThreeTimes(t *testing.T) {
	t.Parallel()

	s := newTestSession("write")
	s.extendedState.preparedStatements[""] = &preparedStatement{sql: "SELECT n FROM t"}

	s.handleBind(&pgproto3.Bind{DestinationPortal: "p"})
	s.handleDescribe(&pgproto3.Describe{ObjectType: 'P', Name: "p"})
	require.NoError(t, s.handleExecute(&pgproto3.Execute{Portal: "p", MaxRows: 2}))
	// The RowDescription arrives while the first Execute is queued.
	s.captureRowDescription(rowDesc("n", 20))
	assert.Equal(t, []string{"n"}, s.extendedState.pendingQueries[0].columnNames)

	for range 2 {
		require.NoError(t, s.handleExecute(&pgproto3.Execute{Portal: "p", MaxRows: 2}))
	}

	require.Len(t, s.extendedState.pendingQueries, 3)

	for i, q := range s.extendedState.pendingQueries {
		assert.Equal(t, []string{"n"}, q.columnNames, "page %d", i+1)
		assert.Equal(t, []uint32{20}, q.columnOIDs, "page %d", i+1)
	}
}

// A Describe the server answers with NoData must not shift the answers of the
// next ones, and a statement Describe must not leak onto an unrelated Execute.
func TestDescribeCapture_NoDataKeepsQueueAligned(t *testing.T) {
	t.Parallel()

	s := newTestSession("write")
	s.handleDescribe(&pgproto3.Describe{ObjectType: 'S', Name: "ins"})
	s.handleDescribe(&pgproto3.Describe{ObjectType: 'S', Name: "sel"})

	_, ok := s.popDescribe() // NoData for "ins"
	require.True(t, ok)

	s.captureRowDescription(rowDesc("a", 25))

	assert.Contains(t, s.extendedState.stmtColumns, "sel")
	assert.NotContains(t, s.extendedState.stmtColumns, "ins")
	assert.Empty(t, s.extendedState.describeQueue)
}

func TestResultFormat(t *testing.T) {
	t.Parallel()

	assert.Equal(t, int16(0), resultFormat(nil, 3))
	assert.Equal(t, int16(1), resultFormat([]int16{1}, 3))
	assert.Equal(t, int16(1), resultFormat([]int16{0, 1}, 1))
	assert.Equal(t, int16(0), resultFormat([]int16{0, 1}, 5))
}

// A Describe whose batch the upstream closed (ReadyForQuery) without answering
// it is dropped; one from the current batch is kept.
func TestDropStaleDescribes(t *testing.T) {
	t.Parallel()

	s := newTestSession("write")
	s.extendedState.describeQueue = []describeTarget{
		{name: "old", epoch: 1},
		{name: "current", epoch: 2},
	}

	s.bookMu.Lock()
	s.upstreamReadyEpoch = 1
	s.dropStaleDescribes()
	s.bookMu.Unlock()

	// upstreamReadyEpoch=1: epoch 1 is not < 1, so both are still pending.
	require.Len(t, s.extendedState.describeQueue, 2)

	s.bookMu.Lock()
	s.upstreamReadyEpoch = 2
	s.dropStaleDescribes()
	s.bookMu.Unlock()

	require.Len(t, s.extendedState.describeQueue, 1)
	assert.Equal(t, "current", s.extendedState.describeQueue[0].name)

	s.bookMu.Lock()
	s.upstreamReadyEpoch = 3
	s.dropStaleDescribes()
	s.bookMu.Unlock()

	assert.Empty(t, s.extendedState.describeQueue)
}
