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
// range requests, which are not checked against the hash: the download is.
const WholeObjectLimit = 8 << 20

// Read errors. The API turns each into a 502 that says which, never an empty
// list.
var (
	ErrObjectMissing = errors.New("archive object is missing from the bucket")
	ErrHashMismatch  = errors.New("archive object does not match the SHA-256 sealed in the audit chain")
	ErrBadMetadata   = errors.New("archive object has bad metadata")
)

// Object identifies one archive object, as recorded on the connection row.
type Object struct {
	Key    string
	Size   int64
	SHA256 string
}

// file is an opened archive object.
type file struct {
	f *parquet.File
}

// open fetches the object whole when small (checking its hash), otherwise opens
// it over range requests.
func open(ctx context.Context, b *blob.Bucket, o Object) (*file, error) {
	if o.Size <= WholeObjectLimit {
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

	return &file{f: pf}, nil
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
	f, err := open(ctx, b, o)
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
}

// ReadQueryRows returns rows [offset, offset+limit) of one statement of an
// archived connection. The statement's position in the file comes from the
// file's own metadata. A statement with no rows in the file returns none.
func ReadQueryRows(
	ctx context.Context, b *blob.Bucket, o Object, queryUID uuid.UUID, offset int64, limit int,
) (QueryRows, error) {
	f, err := open(ctx, b, o)
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
		return QueryRows{Rows: []RowRecord{}}, nil
	}

	if offset >= pos[1] {
		return QueryRows{Rows: []RowRecord{}, Total: pos[1]}, nil
	}

	if remaining := pos[1] - offset; int64(limit) > remaining {
		limit = int(remaining)
	}

	rows, err := readRecords[RowRecord](f, pos[0]+offset, limit)
	if err != nil {
		return QueryRows{}, err
	}

	return QueryRows{Rows: rows, Total: pos[1]}, nil
}

// Download returns the whole object, hash checked.
func Download(ctx context.Context, b *blob.Bucket, o Object) ([]byte, error) {
	data, err := b.ReadAll(ctx, o.Key)
	if err != nil {
		if gcerrors.Code(err) == gcerrors.NotFound {
			return nil, fmt.Errorf("%w: %s", ErrObjectMissing, o.Key)
		}

		return nil, fmt.Errorf("read archive object: %w", err)
	}

	sum := sha256.Sum256(data)
	if o.SHA256 != "" && hex.EncodeToString(sum[:]) != o.SHA256 {
		return nil, fmt.Errorf("%w: %s", ErrHashMismatch, o.Key)
	}

	return data, nil
}
