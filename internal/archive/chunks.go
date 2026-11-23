package archive

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"sync"

	"gocloud.dev/blob"
	"gocloud.dev/gcerrors"
)

// ChunkBytes is the size of the chunks an object larger than WholeObjectLimit
// is cut into for its per-chunk SHA-256 list, sealed next to the whole-object
// hash. A range read fetches whole chunks and checks each one against the list,
// so every byte the reader parses (footer, page headers, pages) is checked
// without fetching the whole object. 1 MiB keeps the list at 64 hex characters
// per MiB of object in audit_log, and a page read at one or two GETs.
const ChunkBytes = 1 << 20

// chunkCacheSize is how many verified chunks a reader keeps in memory.
const chunkCacheSize = 8

// chunkHasher computes the SHA-256 of every ChunkBytes-long chunk of a stream.
type chunkHasher struct {
	size int64
	h    hash.Hash
	n    int64
	sums []string
}

func newChunkHasher(size int64) *chunkHasher {
	return &chunkHasher{size: size, h: sha256.New()}
}

func (c *chunkHasher) Write(p []byte) {
	for len(p) > 0 {
		room := c.size - c.n
		take := min(int64(len(p)), room)

		c.h.Write(p[:take])
		c.n += take
		p = p[take:]

		if c.n == c.size {
			c.sums = append(c.sums, hex.EncodeToString(c.h.Sum(nil)))
			c.h.Reset()
			c.n = 0
		}
	}
}

// Sums returns the digests of every chunk, the last partial one included.
func (c *chunkHasher) Sums() []string {
	out := append([]string{}, c.sums...)
	if c.n > 0 {
		out = append(out, hex.EncodeToString(c.h.Sum(nil)))
	}

	return out
}

// ChunkCount is the number of chunks of chunkBytes an object of size bytes
// has.
func ChunkCount(size, chunkBytes int64) int64 {
	if chunkBytes <= 0 {
		return 0
	}

	return (size + chunkBytes - 1) / chunkBytes
}

// verifiedReaderAt is an io.ReaderAt over blob range requests that fetches
// whole chunks and checks each against its sealed SHA-256 before handing any
// byte of it out. The first integrity failure is kept: the parquet library may
// wrap or drop the error, so callers check failure() after every operation.
type verifiedReaderAt struct {
	ctx        context.Context //nolint:containedctx // an io.ReaderAt has no context parameter
	b          *blob.Bucket
	key        string
	size       int64
	chunkBytes int64
	digests    []string

	mu    sync.Mutex
	cache map[int64][]byte
	order []int64
	err   error
}

func newVerifiedReaderAt(ctx context.Context, b *blob.Bucket, o Object) (*verifiedReaderAt, error) {
	if o.ChunkBytes <= 0 || int64(len(o.ChunkSHA256)) != ChunkCount(o.Size, o.ChunkBytes) {
		return nil, fmt.Errorf("%w: %s has %d sealed chunk digests of %d bytes for %d bytes",
			ErrHashMismatch, o.Key, len(o.ChunkSHA256), o.ChunkBytes, o.Size)
	}

	return &verifiedReaderAt{
		ctx: ctx, b: b, key: o.Key, size: o.Size, chunkBytes: o.ChunkBytes, digests: o.ChunkSHA256,
		cache: map[int64][]byte{},
	}, nil
}

func (r *verifiedReaderAt) failure() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.err
}

func (r *verifiedReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("archive object: negative offset")
	}

	n := 0

	for n < len(p) {
		pos := off + int64(n)
		if pos >= r.size {
			return n, io.EOF
		}

		idx := pos / r.chunkBytes

		chunk, err := r.chunk(idx)
		if err != nil {
			return n, err
		}

		n += copy(p[n:], chunk[pos-idx*r.chunkBytes:])
	}

	return n, nil
}

// chunk returns chunk idx, fetched and checked once, then kept in a small
// cache.
func (r *verifiedReaderAt) chunk(idx int64) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.err != nil {
		return nil, r.err
	}

	if c, ok := r.cache[idx]; ok {
		return c, nil
	}

	start := idx * r.chunkBytes
	length := min(r.chunkBytes, r.size-start)

	data, err := r.fetch(start, length)
	if err != nil {
		r.err = err

		return nil, err
	}

	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != r.digests[idx] {
		r.err = fmt.Errorf("%w: %s, chunk %d", ErrHashMismatch, r.key, idx)

		return nil, r.err
	}

	if len(r.order) >= chunkCacheSize {
		delete(r.cache, r.order[0])
		r.order = r.order[1:]
	}

	r.cache[idx] = data
	r.order = append(r.order, idx)

	return data, nil
}

func (r *verifiedReaderAt) fetch(start, length int64) ([]byte, error) {
	rr, err := r.b.NewRangeReader(r.ctx, r.key, start, length, nil)
	if err != nil {
		if gcerrors.Code(err) == gcerrors.NotFound {
			return nil, fmt.Errorf("%w: %s", ErrObjectMissing, r.key)
		}

		return nil, fmt.Errorf("read archive object: %w", err)
	}

	defer func() { _ = rr.Close() }()

	data := make([]byte, length)

	if _, err := io.ReadFull(rr, data); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
			// Shorter than sealed: it was truncated.
			return nil, fmt.Errorf("%w: %s is shorter than sealed", ErrHashMismatch, r.key)
		}

		return nil, fmt.Errorf("read archive object: %w", err)
	}

	return data, nil
}
