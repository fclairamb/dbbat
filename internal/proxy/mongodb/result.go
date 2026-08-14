package mongodb

import (
	"encoding/json"
	"log/slog"
	"strconv"
	"time"

	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/fclairamb/dbbat/internal/proxy/shared"
	"github.com/fclairamb/dbbat/internal/store"
)

// maxSQLTextLen bounds the logged command text so a large command document
// (e.g. a bulk insert body) doesn't bloat the queries table.
const maxSQLTextLen = 8 * 1024

// captureResult correlates an upstream reply to the query that produced it and
// records the query log row with any captured cursor rows / rowsAffected
// (phases 2–3). Called for every upstream→client message.
func (s *Session) captureResult(m *message) {
	if m.opCode != opCodeMsg {
		return
	}

	pq := s.takePending(m.responseTo)
	if pq == nil {
		// Unsolicited (e.g. already recorded moreToCome write) — nothing to do.
		return
	}

	parsed, err := parseOpMsg(m.body)
	if err != nil {
		s.recordQuery(pq, nil, nil, nil)

		return
	}

	body, ok := parsed.commandBody()
	if !ok {
		s.recordQuery(pq, nil, nil, nil)

		return
	}

	var queryError *string

	if !replyOK(body) {
		msg := lookupString(body, "errmsg")
		if msg == "" {
			msg = "command failed"
		}

		queryError = &msg
	}

	rowsAffected := rowsAffectedFrom(body)
	rows, truncated := s.captureCursorRows(body)
	pq.resultsTruncated = truncated

	// Maintain find→getMore cursor lineage (item 6) before logging so the
	// origin's own row carries its cursor_id.
	s.trackCursorFromReply(pq, body)

	s.recordQuery(pq, rows, rowsAffected, queryError)
}

// bytesPerMB is used to recompute listDatabases' totalSizeMb after filtering.
const bytesPerMB = 1024 * 1024

// filterListDatabasesReply rewrites a listDatabases reply so its databases
// array lists only the session's grant target (item 5), recomputing totalSize /
// totalSizeMb. It returns (rewritten, true) on success; (nil, false) when the
// reply isn't a filterable listDatabases result (e.g. an error reply), so the
// caller relays the original verbatim.
func (s *Session) filterListDatabasesReply(m *message) ([]byte, bool) {
	parsed, err := parseOpMsg(m.body)
	if err != nil {
		return nil, false
	}

	body, ok := parsed.commandBody()
	if !ok {
		return nil, false
	}

	arr, ok := body.Lookup("databases").ArrayOK()
	if !ok {
		return nil, false
	}

	values, err := arr.Values()
	if err != nil {
		return nil, false
	}

	allowed := s.database.DatabaseName

	var (
		kept      bson.A
		totalSize int64
	)

	for _, v := range values {
		doc, ok := v.DocumentOK()
		if !ok {
			continue
		}

		if lookupString(doc, "name") != allowed {
			continue
		}

		kept = append(kept, doc)

		if sz, ok := lookupInt64(doc, "sizeOnDisk"); ok {
			totalSize += sz
		}
	}

	rebuilt := rewriteDatabasesField(body, kept, totalSize)

	raw, err := buildOpMsgReply(m.requestID, m.responseTo, rebuilt)
	if err != nil {
		return nil, false
	}

	return raw, true
}

// rewriteDatabasesField rebuilds a listDatabases body preserving field order and
// every other field (ok, etc.), substituting the filtered databases array and
// the recomputed totalSize / totalSizeMb.
func rewriteDatabasesField(body bson.Raw, databases bson.A, totalSize int64) bson.D {
	if databases == nil {
		databases = bson.A{}
	}

	elems, err := body.Elements()
	if err != nil {
		return bson.D{
			{Key: "databases", Value: databases},
			{Key: "totalSize", Value: float64(totalSize)},
			{Key: "ok", Value: 1.0},
		}
	}

	out := make(bson.D, 0, len(elems))

	for _, e := range elems {
		switch e.Key() {
		case "databases":
			out = append(out, bson.E{Key: "databases", Value: databases})
		case "totalSize":
			out = append(out, bson.E{Key: "totalSize", Value: float64(totalSize)})
		case "totalSizeMb":
			out = append(out, bson.E{Key: "totalSizeMb", Value: float64(totalSize) / bytesPerMB})
		default:
			out = append(out, bson.E{Key: e.Key(), Value: e.Value()})
		}
	}

	return out
}

// rowsAffectedFrom extracts the write result count from a reply, preferring
// nModified (updates) over n (inserts/deletes).
func rowsAffectedFrom(body bson.Raw) *int64 {
	if v, ok := lookupInt64(body, "nModified"); ok {
		return &v
	}

	if v, ok := lookupInt64(body, "n"); ok {
		return &v
	}

	return nil
}

// captureCursorRows re-encodes the documents in cursor.firstBatch /
// cursor.nextBatch as Extended JSON QueryRows, honoring the query-storage
// limits. The second return reports that a limit was hit, so the rows are a
// prefix of the batch rather than the whole of it.
func (s *Session) captureCursorRows(body bson.Raw) ([]store.QueryRow, bool) {
	q := s.server.queryStorage
	if !q.StoreResults {
		return nil, false
	}

	cursor, ok := body.Lookup("cursor").DocumentOK()
	if !ok {
		return nil, false
	}

	batch, ok := cursor.Lookup("firstBatch").ArrayOK()
	if !ok {
		batch, ok = cursor.Lookup("nextBatch").ArrayOK()
		if !ok {
			return nil, false
		}
	}

	values, err := batch.Values()
	if err != nil {
		return nil, false
	}

	rows := make([]store.QueryRow, 0, len(values))

	var totalBytes int64

	var truncated bool

	for i, v := range values {
		if q.MaxResultRows > 0 && i >= q.MaxResultRows {
			truncated = true

			break
		}

		doc, ok := v.DocumentOK()
		if !ok {
			continue
		}

		extJSON, err := bson.MarshalExtJSON(doc, false, false)
		if err != nil {
			s.logger.WarnContext(s.ctx, "MongoDB row encode failed; skipping", slog.Int("row", i), slog.Any("error", err))

			continue
		}

		size := int64(len(extJSON))
		if q.MaxResultBytes > 0 && totalBytes+size > q.MaxResultBytes {
			truncated = true

			break
		}

		rows = append(rows, store.QueryRow{
			RowNumber:    i + 1,
			RowData:      json.RawMessage(extJSON),
			RowSizeBytes: size,
		})
		totalBytes += size
	}

	return rows, truncated
}

// recordQuery inserts a single query log row (asynchronously) with completion
// fields, stores captured rows, and bumps connection + grant counters. Mirrors
// the MySQL proxy's recordQuery.
func (s *Session) recordQuery(pq *pendingQuery, rows []store.QueryRow, rowsAffected *int64, queryError *string) {
	if s.connection == nil {
		return
	}

	// Last gate before the store: never persist an "error" that is not readable
	// text. See shared.SanitizeQueryError.
	queryError = shared.SanitizeQueryError(s.ctx, s.logger, queryError)

	total := s.cumulativeClientBytes()
	bytesTransferred := total - s.lastBytesSnapshot
	s.lastBytesSnapshot = total

	durationMs := float64(time.Since(pq.start).Microseconds()) / 1000.0

	record := &store.Query{
		ConnectionID:     s.connection.UID,
		SQLText:          pq.sqlText,
		Parameters:       pq.params,
		ExecutedAt:       pq.start,
		DurationMs:       &durationMs,
		RowsAffected:     rowsAffected,
		Error:            queryError,
		ResultsTruncated: pq.resultsTruncated,
	}

	// When there are rows to store, the record goes in without its completion
	// fields and is completed after the rows land: a query must never read as
	// finished while its rows are still arriving.
	hasRows := len(rows) > 0

	go shared.RunGuarded(s.ctx, s.logger, goroutineNameCommandRecord, func() {
		sink := s.server.rowWriter.NewSink()

		// The sink is created here and never escapes, but a panic between
		// Resolve and Flush would strand the rows already queued against it.
		// Fail is a no-op once Resolve has run, so this only bites on the
		// panic path.
		defer sink.Fail()

		queryUID := pq.approvalUID
		held := queryUID != uuid.Nil

		if !held {
			insert := record

			if hasRows {
				bare := *record
				bare.DurationMs = nil
				bare.RowsAffected = nil
				bare.Error = nil
				bare.ResultsTruncated = false
				insert = &bare
			}

			created, err := s.server.store.CreateQuery(s.ctx, insert)
			if err != nil {
				s.logger.ErrorContext(s.ctx, "create query log failed", slog.Any("error", err))
				sink.Fail()

				return
			}

			queryUID = created.UID
		}

		sink.Resolve(queryUID)

		// Blocking hand-off: the reply's rows are already resident, so
		// queueing them costs no extra memory and dropping them would lose a
		// capture that used to be stored whole.
		sink.AddAll(s.ctx, rows)
		sink.Flush(s.ctx)

		record.ResultsDropped = sink.Dropped()

		// A held row always needs its completion write. A row inserted here
		// needs one only when it went in bare (rows to store) or when the
		// writer turned out to have dropped some.
		if held || hasRows || record.ResultsDropped {
			if err := s.server.store.UpdateQueryCompletion(
				s.ctx, queryUID, &durationMs, rowsAffected, queryError, pq.resultsTruncated, record.ResultsDropped,
			); err != nil {
				s.logger.ErrorContext(s.ctx, "complete command log failed", slog.Any("error", err))
			}
		}

		s.stream.Query(queryUID, record)

		if bytesTransferred > 0 {
			if err := s.server.store.IncrementConnectionStats(s.ctx, s.connection.UID, bytesTransferred); err != nil {
				s.logger.DebugContext(s.ctx, "increment connection stats failed", slog.Any("error", err))
			}
		}
	})

	if s.grant != nil {
		s.grant.QueryCount++
		s.grant.BytesTransferred += bytesTransferred
	}
}

// buildSQLText renders a command as "<name> <canonical-ExtJSON-of-body>",
// truncated to maxSQLTextLen (contract: query logging).
func buildSQLText(cmd string, body bson.Raw) string {
	extJSON, err := bson.MarshalExtJSON(body, false, false)
	if err != nil {
		return cmd
	}

	text := cmd + " " + string(extJSON)
	if len(text) > maxSQLTextLen {
		text = text[:maxSQLTextLen]
	}

	return text
}

// extractParams pulls session/transaction metadata (lsid, txnNumber) into the
// query parameters, when present.
func extractParams(body bson.Raw) *store.QueryParameters {
	var values []string

	if lsid, ok := body.Lookup("lsid").DocumentOK(); ok {
		if ej, err := bson.MarshalExtJSON(lsid, false, false); err == nil {
			values = append(values, "lsid="+string(ej))
		}
	}

	if txn, ok := lookupInt64(body, "txnNumber"); ok {
		values = append(values, "txnNumber="+strconv.FormatInt(txn, 10))
	}

	if len(values) == 0 {
		return nil
	}

	return &store.QueryParameters{Values: values}
}

// lookupInt64 returns an integer field from a document, accepting int32, int64
// or double encodings.
func lookupInt64(doc bson.Raw, key string) (int64, bool) {
	v := doc.Lookup(key)

	if i, ok := v.Int32OK(); ok {
		return int64(i), true
	}

	if i, ok := v.Int64OK(); ok {
		return i, true
	}

	if d, ok := v.DoubleOK(); ok {
		return int64(d), true
	}

	return 0, false
}
