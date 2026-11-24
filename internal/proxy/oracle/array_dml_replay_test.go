package oracle

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/dump"
	"github.com/fclairamb/dbbat/internal/proxy/shared"
	"github.com/fclairamb/dbbat/internal/store"
)

// arrayDMLDump is python-oracledb thin running the shapes an audit log has to
// get right for DML that runs more than once per statement: an executemany, the
// same executemany re-run on the parsed cursor, an execute() loop, a SELECT
// re-executed with fresh binds, and VARCHAR2 values past the 252-byte short
// form. Regenerate with TestCapture_PythonThinArrayDML; every value is
// synthetic.
//
// It exists because of a real session where an execute() loop of 3 757 UPDATEs
// was logged as the 8 parses among them, each with rows_affected 1 and the
// binds of its first row, while the table showed 816 rows changed. The
// re-executions carried their bind row behind the four header integers, so
// decodeCursorReexec refused them as "not a re-execution" and they went
// upstream unlogged and ungated.
const arrayDMLDump = "python_thin_array_dml.pcapng"

// arrayDMLUpdate is the UPDATE every DML step of the recording runs.
const arrayDMLUpdate = "UPDATE dbbat_array_dml SET name = :nouveau WHERE id = :n AND name = :ancien"

// replayArrayDML replays the recording with result capture off, so every
// statement reaches the store through completeQuery's create-now branch, and
// waits for the asynchronous writes to land.
func replayArrayDML(t *testing.T) []*store.Query {
	t.Helper()

	s := newTestSession(&store.Grant{Definition: &store.GrantDefinition{}})
	recorder := &collectingCompletionStore{}
	s.completionStore = recorder

	replayObservingCLRChunks(t, s, arrayDMLDump)

	// The DROP that ends the recording is the last statement; once it is in,
	// everything before it is too (each create is its own goroutine, so this
	// waits on the count rather than on any one of them).
	var queries []*store.Query

	require.Eventually(t, func() bool {
		queries = recorder.createdQueries()

		for _, q := range queries {
			if q.SQLText == "DROP TABLE dbbat_array_dml" {
				return true
			}
		}

		return false
	}, 5*time.Second, 10*time.Millisecond, "the recording's last statement never reached the store")

	time.Sleep(50 * time.Millisecond) // the creates before it run on their own goroutines

	return recorder.createdQueries()
}

// replayObservingCLRChunks is replaySession plus the one pre-auth observation a
// decoder needs: whether the session negotiated the big CLR chunk form, which
// relay_preauth reads off the Set Protocol reply in production.
func replayObservingCLRChunks(t *testing.T, s *session, name string) {
	t.Helper()

	s.clientConn = drainedPipe(t)

	for _, pkt := range loadTestDump(t, name).Packets {
		if pkt.Direction == dump.DirServerToClient && observeBigClrChunksFlag(pkt.Data) {
			s.clientBigClrChunks = true
		}

		tns, err := parseTNSFromDumpPacket(pkt.Data)
		if err != nil || tns.Type != TNSPacketTypeData {
			continue
		}

		if pkt.Direction == dump.DirClientToServer {
			s.interceptClientMessage(tns)

			continue
		}

		s.interceptUpstreamMessage(tns)
	}
}

// queriesWithSQL returns the recorded queries whose text is exactly sql, keyed
// by the first value of their first bind row.
func queriesWithSQL(queries []*store.Query, sql string) map[string]*store.Query {
	out := map[string]*store.Query{}

	for _, q := range queries {
		if q.SQLText != sql {
			continue
		}

		key := ""
		if q.Parameters != nil && len(q.Parameters.Values) > 0 {
			key = q.Parameters.Values[0]
		}

		out[key] = q
	}

	return out
}

func rowsAffected(q *store.Query) int64 {
	if q == nil || q.RowsAffected == nil {
		return -1
	}

	return *q.RowsAffected
}

// TestDumpReplay_ArrayDMLRecordsEveryBindRow is the executemany half: one
// execute carrying 30 bind rows (29 of which match a row) must be recorded once,
// with the server's own row count and all 30 rows — and so must the same
// executemany re-run as a re-execution of the parsed cursor.
func TestDumpReplay_ArrayDMLRecordsEveryBindRow(t *testing.T) {
	t.Parallel()

	byFirstBind := queriesWithSQL(replayArrayDML(t), arrayDMLUpdate)

	parse := byFirstBind["Bloc 1 bis"]
	require.NotNil(t, parse, "the executemany parse must be recorded")
	assert.Equal(t, int64(29), rowsAffected(parse), "rows_affected is the server's total over the batch")
	require.NotNil(t, parse.Parameters)
	assert.Equal(t, []string{"Bloc 1 bis", "1", "Bloc 1"}, parse.Parameters.Values,
		"values is the first bind row, in placeholder order")
	assert.Equal(t, 30, parse.Parameters.BatchSize)
	require.Len(t, parse.Parameters.Batch, 30, "every bind row of the batch is recorded")
	assert.Equal(t, []string{"Bloc 13 bis", "999", "Bloc 13"}, parse.Parameters.Batch[12])
	assert.Equal(t, []string{"Bloc 30 bis", "30", "Bloc 30"}, parse.Parameters.Batch[29])
	assert.False(t, parse.Parameters.BatchTruncated)

	reexec := byFirstBind["Bloc 31 bis"]
	require.NotNil(t, reexec, "an executemany re-run on the parsed cursor is a re-execution and must be recorded")
	assert.Equal(t, int64(10), rowsAffected(reexec))
	require.NotNil(t, reexec.Parameters)
	assert.Equal(t, 10, reexec.Parameters.BatchSize)
	require.Len(t, reexec.Parameters.Batch, 10)
	assert.Equal(t, []string{"Bloc 40 bis", "40", "Bloc 40"}, reexec.Parameters.Batch[9])
}

// TestDumpReplay_ExecuteLoopRecordsEachExecution is the shape the real session
// had: the same UPDATE run by an execute() loop, each run a `03 04`
// re-execution carrying its own bind row. Each one is a statement of its own,
// with its own binds and its own row count.
func TestDumpReplay_ExecuteLoopRecordsEachExecution(t *testing.T) {
	t.Parallel()

	byFirstBind := queriesWithSQL(replayArrayDML(t), arrayDMLUpdate)

	for _, id := range []string{"41", "42", "43", "44"} {
		q := byFirstBind["Bloc "+id+" ter"]
		require.NotNilf(t, q, "execution with n=%s must be recorded", id)
		assert.Equalf(t, []string{"Bloc " + id + " ter", id, "Bloc " + id}, q.Parameters.Values,
			"execution with n=%s must carry its own binds", id)
		assert.Equalf(t, int64(1), rowsAffected(q), "execution with n=%s updated one row", id)
		assert.Emptyf(t, q.Parameters.Batch, "a single-row execution has no batch (n=%s)", id)
	}

	miss := byFirstBind["Bloc 999 ter"]
	require.NotNil(t, miss, "the execution matching nothing must be recorded too")
	assert.Equal(t, int64(0), rowsAffected(miss))

	assert.Len(t, byFirstBind, 7, "1 executemany + 1 executemany re-run + 5 loop executions")
}

// TestDumpReplay_SelectReexecWithBindsIsItsOwnStatement: a SELECT re-run with
// fresh binds is a `03 4e` carrying them. It is recorded as a query of its own,
// with its own binds, rather than its rows being credited to the first run.
func TestDumpReplay_SelectReexecWithBindsIsItsOwnStatement(t *testing.T) {
	t.Parallel()

	const sel = "SELECT id, name FROM dbbat_array_dml WHERE id BETWEEN :lo AND :hi ORDER BY id"

	byFirstBind := queriesWithSQL(replayArrayDML(t), sel)

	require.Contains(t, byFirstBind, "1")
	require.Contains(t, byFirstBind, "41", "the re-execution must be recorded")
	assert.Equal(t, []string{"1", "5"}, byFirstBind["1"].Parameters.Values)
	assert.Equal(t, []string{"41", "45"}, byFirstBind["41"].Parameters.Values)
}

// TestDumpReplay_LongFormVarcharRowsDecode is the row-capture half: a VARCHAR2
// value past 252 bytes arrives in the CLR long form, `fe` then chunks then
// `00`. The scalar reader used to take the `fe` for a length of 254, store 254
// bytes of hex, read the next column out of the middle of the value and lose
// the rest of the packet. A server whose character set differs from the
// client's sends short values with non-ASCII text in them the same way, which
// is how the real session lost most of its accented names.
func TestDumpReplay_LongFormVarcharRowsDecode(t *testing.T) {
	t.Parallel()

	rows := replayCapturingRows(t, arrayDMLDump, "SELECT id, note, name FROM dbbat_array_dml")
	require.Len(t, rows, 8, "every row of the result set is captured")

	for i, row := range rows {
		id := i + 1
		assert.Equal(t, strconv.Itoa(id), row["ID"], "row %d id", id)

		wantName := "Bloc " + strconv.Itoa(id) + " bis"
		assert.Equal(t, wantName, row["NAME"], "row %d name", id)

		if id > 6 {
			assert.Empty(t, row["NOTE"], "row %d has no note", id)

			continue
		}

		prefix := "Bâtiment " + strconv.Itoa(id)
		want := prefix + strings.Repeat(".", 300-len([]rune(prefix)))
		assert.Equal(t, want, row["NOTE"], "row %d note", id)
	}
}

// replayCapturingRows replays a recording with result capture on and returns
// the rows captured for the first statement whose text starts with sqlPrefix.
func replayCapturingRows(t *testing.T, name, sqlPrefix string) []map[string]string {
	t.Helper()

	rowStore := &captureRowStore{}
	writer := shared.NewRowWriter(rowStore, testLogger())

	s := newTestSessionWithStorage(&store.Grant{Definition: &store.GrantDefinition{}}, true, 100000, captureTestMaxBytes)
	s.rowWriter = writer
	s.completionStore = &collectingCompletionStore{}
	s.clientConn = drainedPipe(t)

	var want uuid.UUID

	for _, pkt := range loadTestDump(t, name).Packets {
		if pkt.Direction == dump.DirServerToClient && observeBigClrChunksFlag(pkt.Data) {
			s.clientBigClrChunks = true
		}

		tns, err := parseTNSFromDumpPacket(pkt.Data)
		if err != nil || tns.Type != TNSPacketTypeData {
			continue
		}

		if pkt.Direction == dump.DirClientToServer {
			s.interceptClientMessage(tns)

			continue
		}

		// session.store is a concrete *store.Store, so persistQueryRecord cannot
		// run here: put the in-flight query in the state it leaves behind.
		s.trackerMu.Lock()

		if p := s.tracker.pendingQuery; p != nil && p.cursor != nil && !p.queryPersisted {
			p.queryPersisted = true
			p.queryUID = uuid.Must(uuid.NewV7())
			p.rowSink = writer.NewSinkFor(p.queryUID)

			if want == uuid.Nil && strings.HasPrefix(p.cursor.sql, sqlPrefix) {
				want = p.queryUID
			}
		}

		s.trackerMu.Unlock()

		s.interceptUpstreamMessage(tns)
	}

	writer.Close(context.Background())
	require.NotEqual(t, want, uuid.Nil, "no statement starting with %q", sqlPrefix)

	var out []map[string]string

	rowStore.mu.Lock()
	defer rowStore.mu.Unlock()

	for _, batch := range rowStore.batches {
		for _, row := range batch {
			if row.QueryID != want {
				continue
			}

			var decoded map[string]any
			require.NoError(t, json.Unmarshal(row.RowData, &decoded))

			flat := map[string]string{}
			for k, v := range decoded {
				if s, ok := v.(string); ok {
					flat[k] = s
				}
			}

			out = append(out, flat)
		}
	}

	return out
}

// TestDumpReplay_BitlessStatusOEREndsItsCall is the evidence behind
// completeFromStandaloneStatusOER, measured on every recording in testdata/.
//
// The reading accepts a standalone func 0x04 reporting success without the
// end-of-call bit, which used to be refused because a standalone 0x04 is routed
// on its first byte alone. Outside a row stream, the claim is that such a
// packet is always the call's last word: so for every packet the reading
// accepts, the next thing in the recording must be the client speaking (or the
// end of the recording). A packet accepted with more server bytes behind it
// would be a call ended early.
func TestDumpReplay_BitlessStatusOEREndsItsCall(t *testing.T) {
	t.Parallel()

	names, err := filepath.Glob("testdata/*.pcapng")
	require.NoError(t, err)

	accepted := map[string]int{}

	for _, path := range names {
		name := filepath.Base(path)

		s := newTestSession(&store.Grant{Definition: &store.GrantDefinition{}})
		s.completionStore = &collectingCompletionStore{}
		s.clientConn = drainedPipe(t)

		packets := loadTestDump(t, name).Packets

		for i, pkt := range packets {
			tns, err := parseTNSFromDumpPacket(pkt.Data)
			if err != nil || tns.Type != TNSPacketTypeData {
				continue
			}

			if pkt.Direction == dump.DirClientToServer {
				s.interceptClientMessage(tns)

				continue
			}

			if ttc := extractTTCPayload(tns.Payload); len(ttc) > 0 && ttc[0] == byte(TTCFuncOERR) {
				s.learnOERTail(ttc)

				if bitlessStatusWouldEndTheCall(s, ttc) {
					accepted[name]++

					if next := nextDataPacket(packets, i); next >= 0 {
						assert.Equalf(t, dump.DirClientToServer, packets[next].Direction,
							"%s packet #%d was taken as the end of the call, but the server kept talking", name, i)
					}
				}
			}

			s.interceptUpstreamMessage(tns)
		}
	}

	t.Logf("bit-less status OERs accepted as the end of a call: %v", accepted)
	// The executemany re-run and four of the execute() loop's five runs. The
	// fifth bound a longer value, so the client resent the bind metadata in a
	// statement-less `03 5e`, which the server answers with a Response.
	assert.Equal(t, 5, accepted[arrayDMLDump],
		"each DML re-execution of the recording ends on one")
}

// bitlessStatusWouldEndTheCall is completeFromStandaloneStatusOER's predicate,
// evaluated without acting on it, for the packet handleOERStatus would hand it.
func bitlessStatusWouldEndTheCall(s *session, ttc []byte) bool {
	s.trackerMu.Lock()
	defer s.trackerMu.Unlock()

	shape := s.oerShapeSnapshot()

	if info := decodeOERAt(shape, ttc, 0); info != nil && s.statusOERMayEndTheCall(info) {
		return false
	}

	if s.tracker.pendingQuery == nil || s.rowStreamActive() || (shape.tailLearned && shape.fixedWidth) {
		return false
	}

	if decodeErrorOER(shape, ttc) != nil {
		return false
	}

	info, _ := decodeOERFieldsAt(ttc, 0)

	return plausibleStatusOER(info)
}

// nextDataPacket is the index of the next TNS Data packet after i, -1 at the
// end of the recording.
func nextDataPacket(packets []dump.Packet, i int) int {
	for j := i + 1; j < len(packets); j++ {
		if tns, err := parseTNSFromDumpPacket(packets[j].Data); err == nil && tns.Type == TNSPacketTypeData {
			return j
		}
	}

	return -1
}
