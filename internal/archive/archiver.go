package archive

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	neturl "net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/parquet-go/parquet-go"
	"github.com/parquet-go/parquet-go/compress/zstd"
	"gocloud.dev/blob"
	"gocloud.dev/gcerrors"

	"github.com/fclairamb/dbbat/internal/bucket"
	"github.com/fclairamb/dbbat/internal/store"
	"github.com/fclairamb/dbbat/internal/version"
)

// Bounds of one pass. The first pass after archival is switched on meets the
// whole history (tens of gigabytes on the measured instance) and must not hold
// the store for hours: it stops on whichever bound comes first and wakes the
// sweeper again.
const (
	// MaxConnectionsPerPass bounds the connections archived in one pass.
	MaxConnectionsPerPass = 200
	// MaxBytesPerPass bounds the object bytes written in one pass.
	MaxBytesPerPass int64 = 512 << 20
	// MaxDeletesPerPass bounds the archive objects deleted in one pass.
	MaxDeletesPerPass = 500
	// RowGroupRows caps a row group so the writer's memory is bounded for the 1%
	// of sessions that carry most of the bytes.
	RowGroupRows = 50_000
	// FailureNotifyAfter is how long consecutive failures last before the
	// notifier is called.
	FailureNotifyAfter = time.Hour
)

// ErrNotConfigured is returned when reading an archive while no bucket is
// configured.
var ErrNotConfigured = errors.New("archive is not configured")

// Archiver writes and deletes archive objects.
type Archiver struct {
	store  *store.Store
	bucket *blob.Bucket
	// dir is the directory behind a file:// bucket, empty otherwise.
	dir    string
	logger *slog.Logger

	// OnPersistentFailure, when set, is called (at most once per outage) after an
	// hour of consecutive archive failures. Wired to the default notify channel.
	OnPersistentFailure func(err error)

	failMu     sync.Mutex
	failingFor time.Time
	notified   bool
}

// Open opens the archive bucket. An empty URL returns (nil, nil): archival does
// not exist, which is the default.
func Open(ctx context.Context, url string, dataStore *store.Store, logger *slog.Logger) (*Archiver, error) {
	if url == "" {
		return nil, nil //nolint:nilnil // nil archiver is the documented "disabled" value
	}

	b, err := bucket.Open(ctx, withFileOptions(url))
	if err != nil {
		return nil, fmt.Errorf("open archive bucket: %w", err)
	}

	if logger == nil {
		logger = slog.Default()
	}

	a := &Archiver{store: dataStore, bucket: b, logger: logger}

	if u, err := neturl.Parse(url); err == nil && u.Scheme == "file" {
		a.dir = u.Path
	}

	return a, nil
}

// withFileOptions adds the options an archive directory needs so it holds
// nothing but the .parquet files: no sidecar .attrs file (the hash that matters
// is sealed in the audit chain) and atomic writes on the same file system
// (written as <name>.<hex>.tmp in its final directory, then renamed).
func withFileOptions(raw string) string {
	if !strings.HasPrefix(raw, "file://") {
		return raw
	}

	sep := "?"
	if strings.Contains(raw, "?") {
		sep = "&"
	}

	if !strings.Contains(raw, "metadata=") {
		raw += sep + "metadata=skip"
		sep = "&"
	}

	if !strings.Contains(raw, "no_tmp_dir=") {
		raw += sep + "no_tmp_dir=true"
	}

	return raw
}

// Bucket exposes the bucket for readers.
func (a *Archiver) Bucket() *blob.Bucket {
	if a == nil {
		return nil
	}

	return a.bucket
}

// Close closes the bucket.
func (a *Archiver) Close() error {
	if a == nil {
		return nil
	}

	return a.bucket.Close()
}

// PassResult counts what one pass did.
type PassResult struct {
	Archived int
	Deleted  int
	// Bounded is true when the pass stopped on its bound: more is due.
	Bounded bool
}

// Pass runs the archive phases of a sweeper pass: archive due connections, then
// delete due archives.
func (a *Archiver) Pass(ctx context.Context) (PassResult, error) {
	var res PassResult

	instance, err := a.store.InstanceRetention(ctx)
	if err != nil {
		return res, err
	}

	var firstErr error

	archived, bounded, err := a.archiveDue(ctx, instance)
	res.Archived, res.Bounded = archived, bounded

	if err != nil {
		firstErr = err
	}

	deleted, dbounded, err := a.deleteDue(ctx, instance)
	res.Deleted = deleted
	res.Bounded = res.Bounded || dbounded

	if err != nil && firstErr == nil {
		firstErr = err
	}

	a.trackFailure(firstErr)

	return res, firstErr
}

// trackFailure notifies after an hour of consecutive failures.
func (a *Archiver) trackFailure(err error) {
	a.failMu.Lock()
	defer a.failMu.Unlock()

	if err == nil {
		a.failingFor = time.Time{}
		a.notified = false

		return
	}

	if a.failingFor.IsZero() {
		a.failingFor = time.Now()
	}

	if !a.notified && time.Since(a.failingFor) >= FailureNotifyAfter && a.OnPersistentFailure != nil {
		a.notified = true

		go a.OnPersistentFailure(err)
	}
}

func (a *Archiver) archiveDue(ctx context.Context, instance store.InstanceRetention) (int, bool, error) {
	candidates, err := a.store.ClaimArchiveCandidates(ctx, instance, MaxConnectionsPerPass, time.Now())
	if err != nil {
		return 0, false, err
	}

	var (
		done    int
		written int64
		firstEr error
	)

	for i, cand := range candidates {
		if ctx.Err() != nil {
			return done, false, ctx.Err()
		}

		if written >= MaxBytesPerPass {
			// Hand the rest back: their lease would otherwise hold them for 15
			// minutes.
			for _, rest := range candidates[i:] {
				_ = a.store.ReleaseArchiveClaim(ctx, rest.UID)
			}

			return done, true, firstEr
		}

		seal, err := a.ArchiveConnection(ctx, cand)
		if err != nil {
			a.logger.ErrorContext(ctx, "archive failed, rows stay in PostgreSQL",
				slog.String("connection", cand.UID.String()), slog.Any("error", err))

			_ = a.store.ReleaseArchiveClaim(ctx, cand.UID)

			if firstEr == nil {
				firstEr = err
			}

			continue
		}

		done++
		written += seal.Bytes
	}

	return done, len(candidates) >= MaxConnectionsPerPass, firstEr
}

// ArchiveConnection writes the rows of one claimed connection to the bucket,
// seals the object in the audit chain and, in one transaction, marks the
// connection archived and deletes its rows from PostgreSQL. A crash anywhere
// leaves the rows in PostgreSQL: the lease expires, the next pass redoes the
// connection and overwrites the same key.
func (a *Archiver) ArchiveConnection(ctx context.Context, cand store.ArchiveCandidate) (store.ArchiveSeal, error) {
	key := RowsKey(cand.DisconnectedAt, cand.ServerUID, cand.UID)
	seal := store.ArchiveSeal{
		ConnectionUID: cand.UID,
		ServerUID:     cand.ServerUID,
		Key:           key,
		PolicySeconds: cand.ArchiveWindow.Seconds,
		PolicySource:  cand.ArchiveWindow.Source,
		PolicyGroups:  cand.ArchiveWindow.GroupUIDs,
	}

	// Canceling this context aborts the blob write without committing a
	// partial object.
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()

	bw, err := a.bucket.NewWriter(wctx, key, &blob.WriterOptions{ContentType: "application/vnd.apache.parquet"})
	if err != nil {
		return seal, fmt.Errorf("open archive object: %w", err)
	}

	hw := &hashingWriter{w: bw, h: sha256.New()}

	pw := parquet.NewGenericWriter[RowRecord](hw,
		parquet.Compression(&zstd.Codec{}),
		parquet.MaxRowsPerRowGroup(RowGroupRows),
		parquet.KeyValueMetadata(MetaSchemaVersion, SchemaVersion),
		parquet.KeyValueMetadata(MetaVersion, version.Version),
		parquet.KeyValueMetadata(MetaConnection, mustJSON(map[string]any{
			"uid": cand.UID, "server_uid": cand.ServerUID, "disconnected_at": cand.DisconnectedAt.UTC(),
		})),
	)

	offsets := map[string][2]int64{}
	buf := make([]RowRecord, 0, 1024)

	flush := func() error {
		if len(buf) == 0 {
			return nil
		}

		if _, err := pw.Write(buf); err != nil {
			return err
		}

		buf = buf[:0]

		return nil
	}

	var position int64

	err = a.store.StreamConnectionRows(ctx, cand.UID, func(r store.ArchivedRow) error {
		qk := r.QueryUID.String()
		o, ok := offsets[qk]

		if !ok {
			o = [2]int64{position, 0}
		}

		o[1]++
		offsets[qk] = o
		position++

		buf = append(buf, RowRecord{
			ConnectionUID: uidBytes(cand.UID),
			QueryUID:      uidBytes(r.QueryUID),
			RowUID:        uidBytes(r.RowUID),
			RowNumber:     int32(r.RowNumber),
			RowData:       string(r.RowData),
			RowSizeBytes:  r.RowSizeBytes,
		})

		if len(buf) == cap(buf) {
			return flush()
		}

		return nil
	})
	if err == nil {
		err = flush()
	}

	if err == nil {
		pw.SetKeyValueMetadata(MetaQueryOffsets, mustJSON(offsets))
		err = pw.Close()
	}

	if err != nil {
		cancel()
		_ = bw.Close()

		return seal, fmt.Errorf("write archive object: %w", err)
	}

	if err := bw.Close(); err != nil {
		return seal, fmt.Errorf("commit archive object: %w", err)
	}

	seal.Bytes = hw.n
	seal.SHA256 = hex.EncodeToString(hw.h.Sum(nil))
	seal.Rows = position
	seal.Statements = int64(len(offsets))

	if err := a.store.SealAndCommitArchive(ctx, seal); err != nil {
		return seal, err
	}

	return seal, nil
}

func (a *Archiver) deleteDue(ctx context.Context, instance store.InstanceRetention) (int, bool, error) {
	due, err := a.store.ListArchivesDueForDeletion(ctx, instance, MaxDeletesPerPass, time.Now())
	if err != nil {
		return 0, false, err
	}

	var (
		done    int
		firstEr error
	)

	for _, ref := range due {
		if ctx.Err() != nil {
			return done, false, ctx.Err()
		}

		// The list was read earlier in the pass: an edit since then may have
		// lengthened the window, and deleting an object cannot be undone.
		still, err := a.store.ArchiveStillDue(ctx, ref, time.Now())
		if err != nil {
			if firstEr == nil {
				firstEr = err
			}

			continue
		}

		if !still {
			continue
		}

		if err := a.deleteObject(ctx, ref.Key); err != nil {
			a.logger.ErrorContext(ctx, "archive deletion failed",
				slog.String("connection", ref.ConnectionUID.String()), slog.Any("error", err))

			if firstEr == nil {
				firstEr = err
			}

			continue
		}

		if err := a.store.MarkArchiveDeleted(ctx, ref); err != nil {
			if firstEr == nil {
				firstEr = err
			}

			continue
		}

		done++
	}

	return done, len(due) >= MaxDeletesPerPass, firstEr
}

// deleteObject deletes one object; an object already gone is a success. On a
// file:// bucket it then removes the server= and date= directories when empty
// (os.Remove refuses a directory that still has entries). A bucket has no
// directories, so this only runs on file://.
func (a *Archiver) deleteObject(ctx context.Context, key string) error {
	err := a.bucket.Delete(ctx, key)
	if err != nil && gcerrors.Code(err) != gcerrors.NotFound {
		return fmt.Errorf("delete archive object: %w", err)
	}

	a.removeEmptyDirs(key)

	return nil
}

// removeEmptyDirs removes the server= and date= directories of a deleted key
// when empty (os.Remove refuses a directory that still has entries).
func (a *Archiver) removeEmptyDirs(key string) {
	dir := a.fileDir()
	if dir == "" {
		return
	}

	for p := path.Dir(key); p != "." && p != "/" && p != "v1/rows" && p != "v1"; p = path.Dir(p) {
		if os.Remove(filepath.Join(dir, filepath.FromSlash(p))) != nil {
			return
		}
	}
}

// fileDir is the directory behind a file:// archive, "" for any other bucket.
func (a *Archiver) fileDir() string {
	return a.dir
}

type hashingWriter struct {
	w io.Writer
	h hash.Hash
	n int64
}

func (h *hashingWriter) Write(p []byte) (int, error) {
	n, err := h.w.Write(p)
	h.h.Write(p[:n])
	h.n += int64(n)

	return n, err
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}

	return string(b)
}
