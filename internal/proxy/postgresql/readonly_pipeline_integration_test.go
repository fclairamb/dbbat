//go:build integration

package postgresql

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// unpinViaBuiltin turns the session default off through a built-in that runs a
// query from a string, the name spelled in two halves so no text scan of the
// statement reads it. No function has to exist on the target beforehand.
const unpinViaBuiltin = "SELECT query_to_xml('select set_' || 'config(''default_transaction_' || " +
	"'read_only'', ''off'', false)', true, false, '')"

// sendPipelined writes every statement as its own simple Query, back to back,
// before reading a single response, then drains whatever the server sends until
// the session ends or the last ReadyForQuery arrives.
func sendPipelined(t *testing.T, conn *pgx.Conn, statements ...string) {
	t.Helper()

	pc := conn.PgConn()
	fe := pc.Frontend()

	for _, sql := range statements {
		fe.Send(&pgproto3.Query{String: sql})
	}

	require.NoError(t, fe.Flush())

	require.NoError(t, pc.Conn().SetReadDeadline(time.Now().Add(10*time.Second)))

	ready := 0
	for ready < len(statements) {
		msg, err := fe.Receive()
		if err != nil {
			return
		}

		if _, ok := msg.(*pgproto3.ReadyForQuery); ok {
			ready++
		}
	}
}

// TestIntegration_ReadOnlyGrant_PipelinedStatementAfterLostPin covers the
// window the lost-pin tripwire leaves open: the statement that drops the pin and
// the write that uses it are sent in one flight, so the second is already in the
// upstream's socket buffer when the tripwire reads the ParameterStatus.
func TestIntegration_ReadOnlyGrant_PipelinedStatementAfterLostPin(t *testing.T) {
	ctx := context.Background()
	f := setupFixture(ctx, t)

	seed := f.mustConnect(ctx, fixturePass)
	_, err := seed.Exec(ctx, "CREATE TABLE ro (id int); INSERT INTO ro VALUES (1), (2)")
	require.NoError(t, err)
	require.NoError(t, seed.Close(ctx))

	f.replaceGrant(ctx, []string{"read_only"})

	conn := f.mustConnect(ctx, fixturePass)
	sendPipelined(t, conn, unpinViaBuiltin, "EXPLAIN ANALYZE DELETE FROM ro")

	check := f.mustConnect(ctx, fixturePass)

	var got int
	require.NoError(t, check.QueryRow(ctx, "SELECT count(*) FROM ro").Scan(&got))
	assert.Equal(t, 2, got, "a read-only session must not have deleted anything")
}

// TestIntegration_ReadOnlyGrant_PipelinedReadsStillWork is the positive
// control: a read-only client that pipelines plain reads gets every answer.
func TestIntegration_ReadOnlyGrant_PipelinedReadsStillWork(t *testing.T) {
	ctx := context.Background()
	f := setupFixture(ctx, t)

	seed := f.mustConnect(ctx, fixturePass)
	_, err := seed.Exec(ctx, "CREATE TABLE ro (id int); INSERT INTO ro VALUES (1), (2)")
	require.NoError(t, err)
	require.NoError(t, seed.Close(ctx))

	f.replaceGrant(ctx, []string{"read_only"})

	conn := f.mustConnect(ctx, fixturePass)

	batch := &pgx.Batch{}
	batch.Queue("SELECT count(*) FROM ro")
	batch.Queue("SELECT 42")

	res := conn.SendBatch(ctx, batch)

	var count, answer int
	require.NoError(t, res.QueryRow().Scan(&count))
	require.NoError(t, res.QueryRow().Scan(&answer))
	require.NoError(t, res.Close())

	assert.Equal(t, 2, count)
	assert.Equal(t, 42, answer)
}

// TestIntegration_FunctionCallRefused covers the fast-path FunctionCall
// message: it names a function by OID, so it carries no statement text for any
// control to read and no queries row would record it. A read-only session that
// sends set_config this way, with a write pipelined behind it, must write
// nothing, and the attempt must be on record.
func TestIntegration_FunctionCallRefused(t *testing.T) {
	ctx := context.Background()
	f := setupFixture(ctx, t)

	seed := f.mustConnect(ctx, fixturePass)
	_, err := seed.Exec(ctx, "CREATE TABLE ro (id int); INSERT INTO ro VALUES (1), (2)")
	require.NoError(t, err)

	var setConfigOID uint32
	require.NoError(t, seed.QueryRow(ctx, "SELECT 'pg_catalog.set_config'::regproc::oid").Scan(&setConfigOID))
	require.NoError(t, seed.Close(ctx))

	f.replaceGrant(ctx, []string{"read_only"})

	conn := f.mustConnect(ctx, fixturePass)
	fe := conn.PgConn().Frontend()
	fe.Send(&pgproto3.FunctionCall{
		Function:       setConfigOID,
		ArgFormatCodes: []uint16{0},
		Arguments:      [][]byte{[]byte("default_transaction_read_only"), []byte("off"), []byte("false")},
	})
	fe.Send(&pgproto3.Query{String: "EXPLAIN ANALYZE DELETE FROM ro"})
	require.NoError(t, fe.Flush())
	require.NoError(t, conn.PgConn().Conn().SetReadDeadline(time.Now().Add(10*time.Second)))

	var refused bool

	for ready := 0; ready < 2; {
		msg, err := fe.Receive()
		if err != nil {
			break
		}

		switch m := msg.(type) {
		case *pgproto3.ReadyForQuery:
			ready++
		case *pgproto3.ErrorResponse:
			if ready == 0 {
				refused = true
				assert.Contains(t, m.Message, "function call")
			}
		}
	}

	assert.True(t, refused, "the FunctionCall must be refused")

	check := f.mustConnect(ctx, fixturePass)

	var got int
	require.NoError(t, check.QueryRow(ctx, "SELECT count(*) FROM ro").Scan(&got))
	assert.Equal(t, 2, got, "a read-only session must not have deleted anything")
}
