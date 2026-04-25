package mysql

import (
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"unicode/utf8"

	gomysql "github.com/go-mysql-org/go-mysql/mysql"

	"github.com/fclairamb/dbbat/internal/store"
)

// captureRows walks an upstream MySQL result and serializes its rows as JSON
// arrays for storage in query_rows. Returns the captured rows, the cumulative
// JSON byte size, and a `truncated` flag if any limit was hit.
//
// Both COM_QUERY (text protocol) and COM_STMT_EXECUTE (binary protocol) flow
// through here — go-mysql's *mysql.Result presents both as a Resultset with
// already-decoded FieldValue entries, so the same code captures rows from
// either path. (The Phase 3 spec assumed text-only; the high-level Handler
// API gives us binary-protocol capture for free.)
func (h *handler) captureRows(result *gomysql.Result) ([]store.QueryRow, int64, bool) {
	if result == nil || result.Resultset == nil || len(result.Values) == 0 {
		return nil, 0, false
	}

	q := h.session.server.queryStorage
	if !q.StoreResults {
		return nil, 0, false
	}

	rs := result.Resultset
	rows := make([]store.QueryRow, 0, len(rs.Values))

	var totalBytes int64

	var truncated bool

	for rowIdx, row := range rs.Values {
		if q.MaxResultRows > 0 && rowIdx >= q.MaxResultRows {
			truncated = true

			break
		}

		rowJSON, err := encodeRow(rs.Fields, row)
		if err != nil {
			h.session.logger.WarnContext(h.session.ctx, "row encode failed; skipping",
				slog.Int("row", rowIdx), slog.Any("error", err))

			continue
		}

		size := int64(len(rowJSON))
		if q.MaxResultBytes > 0 && totalBytes+size > q.MaxResultBytes {
			truncated = true

			break
		}

		rows = append(rows, store.QueryRow{
			RowNumber:    rowIdx + 1,
			RowData:      rowJSON,
			RowSizeBytes: size,
		})
		totalBytes += size
	}

	return rows, totalBytes, truncated
}

// encodeRow serializes a single MySQL result row to a JSON array, applying
// type-aware coercions:
//   - NULL → null
//   - uint/int/float → number
//   - BLOB-typed []byte → base64 string when not valid UTF-8 (otherwise string)
//   - everything else (varchar/text/enum/json/dates) → string
//
// JSON column type is detected by Field.Type and parsed if valid; invalid
// JSON falls through to a plain string so the row is never lost.
func encodeRow(fields []*gomysql.Field, row []gomysql.FieldValue) (json.RawMessage, error) {
	values := make([]any, len(row))

	for i, fv := range row {
		var fieldType uint8
		if i < len(fields) && fields[i] != nil {
			fieldType = fields[i].Type
		}

		values[i] = jsonValue(&fv, fieldType)
	}

	return json.Marshal(values)
}

// jsonValue converts one FieldValue to a JSON-serializable Go value.
func jsonValue(fv *gomysql.FieldValue, fieldType uint8) any {
	v := fv.Value()
	if v == nil {
		return nil
	}

	switch typed := v.(type) {
	case []byte:
		return jsonBytesValue(typed, fieldType)
	default:
		// uint64, int64, float64 — JSON-encodes as number directly
		return v
	}
}

// jsonBytesValue interprets a []byte FieldValue per the column's MySQL type.
//
// Binary blob types are base64-encoded with a JSON marker so the stored row
// is unambiguously decoded later. JSON columns are parsed if valid (so the
// audit UI can pretty-print them). Everything else becomes a UTF-8 string.
func jsonBytesValue(b []byte, fieldType uint8) any {
	switch fieldType {
	case gomysql.MYSQL_TYPE_TINY_BLOB,
		gomysql.MYSQL_TYPE_MEDIUM_BLOB,
		gomysql.MYSQL_TYPE_LONG_BLOB,
		gomysql.MYSQL_TYPE_BLOB:
		if !utf8.Valid(b) {
			return map[string]string{
				"$bytes": base64.StdEncoding.EncodeToString(b),
				"$type":  "blob",
			}
		}

		return string(b)

	case gomysql.MYSQL_TYPE_JSON:
		var parsed any
		if err := json.Unmarshal(b, &parsed); err == nil {
			return parsed
		}

		return string(b)

	default:
		// Numeric text-protocol values arrive here as []byte; we leave them as
		// strings rather than attempting type-aware parsing. Audit display
		// pays a small cosmetic cost; correctness is preserved (lossless
		// round-trip of the upstream's wire representation).
		return string(b)
	}
}
