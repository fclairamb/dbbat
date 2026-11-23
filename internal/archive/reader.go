package archive

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/google/uuid"
	"github.com/parquet-go/parquet-go"
	"gocloud.dev/blob"
	"gocloud.dev/gcerrors"
)

// WholeObjectLimit is the size up to which an object is fetched whole, in one
// GET, checked against its SHA-256 and read from memory. That is nearly every
// object (a median connection captures 2 rows). A larger one is read through
// range requests, chunk by chunk, each chunk checked against the per-chunk
// SHA-256 list sealed next to the whole-object hash (see ChunkBytes).
const WholeObjectLimit = 8 << 20

// LegacyWholeObjectLimit is the size up to which an object sealed without a
// per-chunk digest list (archived before the list existed) is still fetched
// whole and checked against its SHA-256. A larger such object can only be read
// unchecked: its rows are served with Verified false, never as verified.
const LegacyWholeObjectLimit = 64 << 20

// Read errors. The API turns each into a 502 that says which, never an empty
// list.
var (
	ErrObjectMissing = errors.New("archive object is missing from the bucket")
	ErrHashMismatch  = errors.New("archive object does not match the SHA-256 sealed in the audit chain")
	ErrBadMetadata   = errors.New("archive object has bad metadata")
)

// Object identifies one archive object, as sealed in its connection.archived
// audit entry (see store.GetConnectionArchive).
type Object struct {
	Key    string
	Size   int64
	SHA256 string
	// ChunkBytes and ChunkSHA256 are the per-chunk digests of an object larger
	// than WholeObjectLimit. Empty for an object sealed before they existed.
	ChunkBytes  int64
	ChunkSHA256 []string
	// ConnectionUID is the connection the object holds the rows of: every
	// record ReadQueryRows returns must carry it.
	ConnectionUID uuid.UUID
}

// file is an opened archive object.
type file struct {
	f *parquet.File
	// verified is false only for a legacy object read unchecked.
	verified bool
	// ra is the checked range reader, nil when the object was fetched whole.
	ra *verifiedReaderAt
}

// failure returns the integrity error a checked range read hit, if any.
func (f *file) failure() error {
	if f.ra == nil {
		return nil
	}

	return f.ra.failure()
}

// open fetches the object whole when small (checking its hash), otherwise opens
// it over range requests checked chunk by chunk. A legacy object without chunk
// digests is fetched whole up to LegacyWholeObjectLimit and read unchecked
// above it. legacyLimit is LegacyWholeObjectLimit outside tests.
func open(ctx context.Context, b *blob.Bucket, o Object, legacyLimit int64) (*file, error) {
	switch {
	case o.Size <= WholeObjectLimit:
		return openWhole(ctx, b, o)
	case len(o.ChunkSHA256) > 0:
		ra, err := newVerifiedReaderAt(ctx, b, o)
		if err != nil {
			return nil, err
		}

		pf, err := parquet.OpenFile(ra, o.Size)
		if ferr := ra.failure(); ferr != nil {
			return nil, ferr
		}

		if err != nil {
			return nil, fmt.Errorf("open archive object: %w", err)
		}

		return &file{f: pf, verified: true, ra: ra}, nil
	case o.Size <= legacyLimit:
		return openWhole(ctx, b, o)
	}

	pf, err := parquet.OpenFile(&rangeReaderAt{ctx: ctx, b: b, key: o.Key}, o.Size)
	if err != nil {
		if gcerrors.Code(err) == gcerrors.NotFound {
			return nil, fmt.Errorf("%w: %s", ErrObjectMissing, o.Key)
		}

		return nil, fmt.Errorf("open archive object: %w", err)
	}

	return &file{f: pf}, nil
}

// openWhole fetches the whole object in one GET, checks its SHA-256 and reads
// it from memory.
func openWhole(ctx context.Context, b *blob.Bucket, o Object) (*file, error) {
	data, err := Download(ctx, b, o)
	if err != nil {
		return nil, err
	}

	pf, err := parquet.OpenFile(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("open archive object: %w", err)
	}

	return &file{f: pf, verified: true}, nil
}

// rangeReaderAt is an io.ReaderAt over blob range requests.
type rangeReaderAt struct {
	ctx context.Context //nolint:containedctx // an io.ReaderAt has no context parameter
	b   *blob.Bucket
	key string
}

func (r *rangeReaderAt) ReadAt(p []byte, off int64) (int, error) {
	rr, err := r.b.NewRangeReader(r.ctx, r.key, off, int64(len(p)), nil)
	if err != nil {
		return 0, err
	}

	defer func() { _ = rr.Close() }()

	n, err := io.ReadFull(rr, p)
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return n, io.EOF
	}

	return n, err
}

func (f *file) metadata(key string) (string, bool) {
	return f.f.Lookup(key)
}

func (f *file) numRows() int64 { return f.f.NumRows() }

// rows returns up to limit records starting at position offset.
func readRecords[T any](f *file, offset int64, limit int) ([]T, error) {
	if offset >= f.numRows() || limit <= 0 {
		return []T{}, nil
	}

	reader := parquet.NewGenericReader[T](f.f)
	defer func() { _ = reader.Close() }()

	if err := reader.SeekToRow(offset); err != nil {
		if ferr := f.failure(); ferr != nil {
			return nil, ferr
		}

		return nil, fmt.Errorf("seek archive object: %w", err)
	}

	if remaining := f.numRows() - offset; int64(limit) > remaining {
		limit = int(remaining)
	}

	out := make([]T, limit)

	n := 0

	for n < limit {
		m, err := reader.Read(out[n:])
		n += m

		if ferr := f.failure(); ferr != nil {
			return nil, ferr
		}

		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}

			return nil, fmt.Errorf("read archive object: %w", err)
		}
	}

	return out[:n], nil
}

// Read returns rows [offset, offset+limit) of one archive object.
func Read[T any](ctx context.Context, b *blob.Bucket, o Object, offset int64, limit int) ([]T, error) {
	f, err := open(ctx, b, o, LegacyWholeObjectLimit)
	if err != nil {
		return nil, err
	}

	return readRecords[T](f, offset, limit)
}

// QueryRows is a page of one statement's rows.
type QueryRows struct {
	Rows []RowRecord
	// Total is how many rows the statement has in the archive.
	Total int64
	// Verified is false when the object could not be checked against its seal
	// (a legacy object above LegacyWholeObjectLimit, sealed without chunk
	// digests). The rows must then never be presented as verified.
	Verified bool
}

// ReadQueryRows returns rows [offset, offset+limit) of one statement of an
// archived connection. The statement's position in the file comes from the
// file's own metadata. A statement with no rows in the file returns none. Every
// returned record must belong to queryUID and o.ConnectionUID: metadata that
// points elsewhere is ErrBadMetadata.
func ReadQueryRows(
	ctx context.Context, b *blob.Bucket, o Object, queryUID uuid.UUID, offset int64, limit int,
) (QueryRows, error) {
	return readQueryRows(ctx, b, o, queryUID, offset, limit, LegacyWholeObjectLimit)
}

func readQueryRows(
	ctx context.Context, b *blob.Bucket, o Object, queryUID uuid.UUID, offset int64, limit int, legacyLimit int64,
) (QueryRows, error) {
	if o.ConnectionUID == uuid.Nil {
		return QueryRows{}, fmt.Errorf("%w: no connection uid to check %s against", ErrBadMetadata, o.Key)
	}

	f, err := open(ctx, b, o, legacyLimit)
	if err != nil {
		return QueryRows{}, err
	}

	raw, ok := f.metadata(MetaQueryOffsets)
	if !ok {
		return QueryRows{}, fmt.Errorf("%w: %s has no %s", ErrBadMetadata, o.Key, MetaQueryOffsets)
	}

	var offsets map[string][2]int64
	if err := json.Unmarshal([]byte(raw), &offsets); err != nil {
		return QueryRows{}, fmt.Errorf("archive object %s: bad %s metadata: %w", o.Key, MetaQueryOffsets, err)
	}

	pos, ok := offsets[queryUID.String()]
	if !ok {
		return QueryRows{Rows: []RowRecord{}, Verified: f.verified}, nil
	}

	if pos[0] < 0 || pos[1] < 0 || pos[0]+pos[1] > f.numRows() {
		return QueryRows{}, fmt.Errorf("%w: %s places statement %s at rows [%d, %d+%d) of %d",
			ErrBadMetadata, o.Key, queryUID, pos[0], pos[0], pos[1], f.numRows())
	}

	if offset >= pos[1] {
		return QueryRows{Rows: []RowRecord{}, Total: pos[1], Verified: f.verified}, nil
	}

	if remaining := pos[1] - offset; int64(limit) > remaining {
		limit = int(remaining)
	}

	rows, err := readRecords[RowRecord](f, pos[0]+offset, limit)
	if err != nil {
		return QueryRows{}, err
	}

	for i, r := range rows {
		if uuid.UUID(r.QueryUID) != queryUID || uuid.UUID(r.ConnectionUID) != o.ConnectionUID {
			return QueryRows{}, fmt.Errorf("%w: %s, row %d of statement %s belongs to statement %s of connection %s",
				ErrBadMetadata, o.Key, offset+int64(i), queryUID, uuid.UUID(r.QueryUID), uuid.UUID(r.ConnectionUID))
		}
	}

	return QueryRows{Rows: rows, Total: pos[1], Verified: f.verified}, nil
}

// Download returns the whole object, hash checked. An object without a hash
// is never served unchecked: it is ErrHashMismatch.
func Download(ctx context.Context, b *blob.Bucket, o Object) ([]byte, error) {
	if o.SHA256 == "" {
		return nil, fmt.Errorf("%w: %s has no sealed SHA-256", ErrHashMismatch, o.Key)
	}

	data, err := b.ReadAll(ctx, o.Key)
	if err != nil {
		if gcerrors.Code(err) == gcerrors.NotFound {
			return nil, fmt.Errorf("%w: %s", ErrObjectMissing, o.Key)
		}

		return nil, fmt.Errorf("read archive object: %w", err)
	}

	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != o.SHA256 {
		return nil, fmt.Errorf("%w: %s", ErrHashMismatch, o.Key)
	}

	return data, nil
}
