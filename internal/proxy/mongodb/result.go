package mongodb

import (
	"encoding/json"
	"log/slog"
	"strconv"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

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
	rows := s.captureCursorRows(body)

	s.recordQuery(pq, rows, rowsAffected, queryError)
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
// limits.
func (s *Session) captureCursorRows(body bson.Raw) []store.QueryRow {
	q := s.server.queryStorage
	if !q.StoreResults {
		return nil
	}

	cursor, ok := body.Lookup("cursor").DocumentOK()
	if !ok {
		return nil
	}

	batch, ok := cursor.Lookup("firstBatch").ArrayOK()
	if !ok {
		batch, ok = cursor.Lookup("nextBatch").ArrayOK()
		if !ok {
			return nil
		}
	}

	values, err := batch.Values()
	if err != nil {
		return nil
	}

	rows := make([]store.QueryRow, 0, len(values))

	var totalBytes int64

	for i, v := range values {
		if q.MaxResultRows > 0 && i >= q.MaxResultRows {
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
			break
		}

		rows = append(rows, store.QueryRow{
			RowNumber:    i + 1,
			RowData:      json.RawMessage(extJSON),
			RowSizeBytes: size,
		})
		totalBytes += size
	}

	return rows
}

// recordQuery inserts a single query log row (asynchronously) with completion
// fields, stores captured rows, and bumps connection + grant counters. Mirrors
// the MySQL proxy's recordQuery.
func (s *Session) recordQuery(pq *pendingQuery, rows []store.QueryRow, rowsAffected *int64, queryError *string) {
	if s.connection == nil {
		return
	}

	total := s.cumulativeClientBytes()
	bytesTransferred := total - s.lastBytesSnapshot
	s.lastBytesSnapshot = total

	durationMs := float64(time.Since(pq.start).Microseconds()) / 1000.0

	record := &store.Query{
		ConnectionID: s.connection.UID,
		SQLText:      pq.sqlText,
		Parameters:   pq.params,
		ExecutedAt:   pq.start,
		DurationMs:   &durationMs,
		RowsAffected: rowsAffected,
		Error:        queryError,
	}

	go func() {
		created, err := s.server.store.CreateQuery(s.ctx, record)
		if err != nil {
			s.logger.ErrorContext(s.ctx, "create query log failed", slog.Any("error", err))

			return
		}

		if len(rows) > 0 {
			if err := s.server.store.StoreQueryRows(s.ctx, created.UID, rows); err != nil {
				s.logger.ErrorContext(s.ctx, "store query rows failed", slog.Any("error", err))
			}
		}

		if bytesTransferred > 0 {
			if err := s.server.store.IncrementConnectionStats(s.ctx, s.connection.UID, bytesTransferred); err != nil {
				s.logger.DebugContext(s.ctx, "increment connection stats failed", slog.Any("error", err))
			}
		}
	}()

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
