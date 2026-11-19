// Package archive moves the captured result rows of closed connections from
// PostgreSQL to an object-storage bucket as Parquet files, and reads them back.
//
// Only result rows are archived (one object per connection that captured any).
// Statements stay in PostgreSQL until their delete window: they are about 4% of
// the store, query_rows is 96%.
package archive

import (
	"fmt"
	"regexp"
	"time"

	"github.com/google/uuid"
)

// SchemaVersion is the "v1" of the object key and the dbbat.schema_version
// file metadata.
const SchemaVersion = "v1"

// File key-value metadata.
const (
	MetaSchemaVersion = "dbbat.schema_version"
	MetaVersion       = "dbbat.version"
	MetaConnection    = "dbbat.connection"
	// MetaQueryOffsets maps each statement uid to [first row position, row
	// count] in the file: the rows of one statement are the positions
	// [first, first+count).
	MetaQueryOffsets = "dbbat.query_offsets"
)

// RowRecord is one captured result row in the Parquet file. The file is sorted
// by statement order (executed_at, uid) then row_number.
//
// There is no MAC column: a MAC is 32 bytes that do not compress and the median
// row carries 31 bytes of data. The row chain is whole in a file, so a verifier
// holding the key recomputes it from row_uid, row_number, row_data and
// row_size_bytes and compares the head with queries.row_chain_mac, which stays
// in PostgreSQL. The file's own integrity is its SHA-256, sealed in the audit
// chain.
type RowRecord struct {
	ConnectionUID [16]byte `parquet:"connection_uid,uuid"`
	QueryUID      [16]byte `parquet:"query_uid,uuid"`
	// RowUID is the uid the row chain MAC covers; it is needed to recompute
	// the chain.
	RowUID    [16]byte `parquet:"row_uid,uuid"`
	RowNumber int32    `parquet:"row_number"`
	// RowData is the canonical JSON text as PostgreSQL's jsonb stores it. Every
	// statement has its own columns, so this stays one JSON column.
	RowData      string `parquet:"row_data,json"`
	RowSizeBytes int64  `parquet:"row_size_bytes"`
}

// keyPattern is the charset of every key: a-z, 0-9, '-', '=', '.', '_' and '/'.
// No ':' and no space, no upper case (macOS and Windows file systems ignore
// case), so a key is a valid path on a plain file system and an archive can be
// copied out of a bucket with `aws s3 sync` and read in place.
var keyPattern = regexp.MustCompile(`^[a-z0-9=._/-]+$`)

// RowsKey is the object key of a connection's rows:
// v1/rows/date=<UTC date of disconnected_at>/server=<server uid>/<connection uid>.parquet.
//
// A pure function of the connection row, built from a date and uids only (never
// a server name or a username): a retry overwrites the same object.
func RowsKey(disconnectedAt time.Time, serverUID, connectionUID uuid.UUID) string {
	return fmt.Sprintf("%s/rows/date=%s/server=%s/%s.parquet",
		SchemaVersion, disconnectedAt.UTC().Format("2006-01-02"), serverUID, connectionUID)
}

// ValidKey reports whether a key only uses the allowed charset.
func ValidKey(key string) bool { return keyPattern.MatchString(key) }

func uidBytes(u uuid.UUID) [16]byte { return u }
