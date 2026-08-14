package dump

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"gocloud.dev/blob"
	"gocloud.dev/gcerrors"

	// Blank imports register the blob drivers dbbat supports. "file" is not
	// only for tests: it is also how a capture archive on a mounted volume is
	// configured, and it is what makes the whole upload path exercisable
	// without any cloud dependency.
	_ "gocloud.dev/blob/fileblob"
	_ "gocloud.dev/blob/s3blob"

	"github.com/fclairamb/dbbat/internal/safe"
)

// Recorder persists the blob key of an uploaded capture on its connection row.
//
// The key has to be stored: the API looks captures up by connection UID alone
// and cannot know which instance wrote the file or on which day, so without it
// every download would have to LIST the bucket. Implemented by *store.Store.
type Recorder interface {
	SetConnectionDumpKey(ctx context.Context, connectionUID uuid.UUID, key string) error
}

// Uploader configuration errors.
var (
	// ErrNoSpoolDir is returned when uploading is configured without a local
	// spool. Captures are written to disk first and uploaded once complete —
	// S3 objects cannot be appended to — so there is nothing to upload
	// without one.
	ErrNoSpoolDir = errors.New("dump upload requires a local spool directory")

	// ErrNoRecorder is returned when no Recorder is supplied: an uploaded
	// capture whose key is stored nowhere can never be found again.
	ErrNoRecorder = errors.New("dump upload requires a key recorder")

	// ErrNoScheme is returned for an upload URL with no scheme. The scheme is
	// what selects the driver.
	ErrNoScheme = errors.New("dump upload URL has no scheme (expected e.g. s3://bucket/prefix)")
)

// Upload queue and retry tuning. The queue is deliberately generous and the
// retries deliberately few: a capture that cannot be uploaded is not lost, it
// simply stays in the spool and is picked up by the next startup sweep.
const (
	uploadQueueSize  = 1024
	uploadWorkers    = 2
	uploadAttempts   = 3
	uploadRetryDelay = 2 * time.Second
	uploadTimeout    = 5 * time.Minute
)

// goroutineNameUpload is what a panic in one queued upload is logged under.
const goroutineNameUpload = "capture upload"

// UploaderOptions configures OpenUploader.
type UploaderOptions struct {
	// URL is the destination bucket, e.g. "s3://bucket/prefix" or
	// "file:///var/lib/dbbat/captures". Empty disables uploading entirely.
	URL string

	// SpoolDir is the local capture directory (DBB_DUMP_DIR). Finished
	// captures are read from — and, once uploaded, removed from — here.
	SpoolDir string

	// InstanceID names the process in the object key, so replicas sharing a
	// bucket stay distinguishable.
	InstanceID string

	// Recorder stores the resulting key on the connection row. Required:
	// an uploaded capture nobody can address again is a lost capture.
	Recorder Recorder

	Logger *slog.Logger
}

// Uploader moves finished captures from the local spool to a blob bucket.
//
// Captures are never streamed to the bucket while the session is live: S3
// objects cannot be appended to, so live streaming would mean multipart uploads
// with 5 MiB parts, would lose the flush-per-packet behavior the writer has
// today, and would strand an invisible incomplete upload on a crash. Spooling
// to disk and uploading on close keeps the "a crash still leaves a valid
// partial pcapng" property.
//
// Every method is safe on a nil *Uploader, which is what "no upload configured"
// is represented by — the four proxies and the API can then call through
// unconditionally.
type Uploader struct {
	bucket     *blob.Bucket
	spoolDir   string
	instanceID string
	recorder   Recorder
	logger     *slog.Logger

	jobs chan uuid.UUID
	wg   sync.WaitGroup

	// closed is what the retry loop watches so a shutdown does not have to
	// wait out a backoff. closeMu makes "still open?" and "enqueue" one step:
	// checking the channel and then sending on it would be a race whose loser
	// sends on a closed channel.
	closeMu sync.RWMutex
	closed  chan struct{}
	done    bool
}

// OpenUploader opens the destination bucket and starts the upload workers.
// An empty opts.URL returns (nil, nil): local-only capture storage, the
// default, is expressed by a nil *Uploader rather than by a flag.
func OpenUploader(ctx context.Context, opts UploaderOptions) (*Uploader, error) {
	if opts.URL == "" {
		return nil, nil //nolint:nilnil // nil uploader is the documented "disabled" value
	}

	if opts.SpoolDir == "" {
		return nil, ErrNoSpoolDir
	}

	if opts.Recorder == nil {
		return nil, ErrNoRecorder
	}

	bucketURL, err := normalizeBucketURL(opts.URL)
	if err != nil {
		return nil, err
	}

	bucket, err := blob.OpenBucket(ctx, bucketURL)
	if err != nil {
		return nil, fmt.Errorf("open capture bucket: %w", err)
	}

	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}

	u := &Uploader{
		bucket:     bucket,
		spoolDir:   opts.SpoolDir,
		instanceID: sanitizeKeySegment(opts.InstanceID),
		recorder:   opts.Recorder,
		logger:     logger,
		jobs:       make(chan uuid.UUID, uploadQueueSize),
		closed:     make(chan struct{}),
	}

	// Deliberately not ctx: the workers must outlive whatever context started
	// the process wiring, and an upload triggered by a session teardown must
	// not be canceled by that session's context going away. Shutdown is
	// signaled by Close, not by cancellation.
	workerCtx := context.WithoutCancel(ctx)

	for range uploadWorkers {
		u.wg.Add(1)

		go u.work(workerCtx)
	}

	return u, nil
}

// normalizeBucketURL turns the documented "s3://bucket/prefix" form into what
// gocloud.dev actually understands.
//
// The s3 opener reads the bucket from the URL host and ignores the path
// entirely, so a path-style prefix would be silently dropped — every capture
// would land at the bucket root. gocloud does support prefixes, but only as a
// "prefix" query parameter, so the path is folded into it here. Schemes whose
// path *is* the location (file, mem) are passed through untouched.
func normalizeBucketURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parse dump upload URL %q: %w", raw, err)
	}

	if u.Scheme == "" {
		return "", fmt.Errorf("%q: %w", raw, ErrNoScheme)
	}

	if u.Scheme == "file" || u.Scheme == "mem" {
		return raw, nil
	}

	prefix := strings.Trim(u.Path, "/")
	if prefix == "" {
		return raw, nil
	}

	q := u.Query()
	// An explicit ?prefix= wins: it is the driver-native spelling, and
	// combining the two would produce a prefix nobody wrote.
	if q.Get("prefix") == "" {
		q.Set("prefix", prefix+"/")
	}

	u.Path = ""
	u.RawQuery = q.Encode()

	return u.String(), nil
}

// sanitizeKeySegment keeps an instance id from introducing extra key segments
// or an empty one. Instance ids are hostnames in practice, but DBB_INSTANCE_ID
// is free-form.
func sanitizeKeySegment(s string) string {
	s = strings.ReplaceAll(s, "/", "_")
	s = strings.TrimSpace(s)

	if s == "" {
		return "unknown"
	}

	return s
}

// Key returns the object key a capture taken at t by this instance gets:
// YYYY/MM/DD/<instance>/<connection uid>.pcapng.
//
// The date segments exist for human browsing only. They are derived from the
// spool file's modification time rather than from the clock, so recomputing the
// key on a retry — or on the next process's startup sweep — yields the same
// key and overwrites rather than orphaning the previous attempt.
func (u *Uploader) Key(connectionUID uuid.UUID, t time.Time) string {
	instance := "unknown"
	if u != nil {
		instance = u.instanceID
	}

	return path.Join(
		t.UTC().Format("2006/01/02"),
		instance,
		connectionUID.String()+FileExt,
	)
}

// Finish schedules the finished capture of connectionUID for upload. Call it
// after the dump writer has been closed — the file must be complete.
//
// It never blocks on the network: the job is queued and a worker does the
// upload. A full queue is not an error either, the capture simply stays in the
// spool for the next startup sweep. No-op on a nil Uploader.
func (u *Uploader) Finish(ctx context.Context, connectionUID uuid.UUID) {
	if u == nil || connectionUID == uuid.Nil {
		return
	}

	u.closeMu.RLock()
	defer u.closeMu.RUnlock()

	if u.done {
		return
	}

	select {
	case u.jobs <- connectionUID:
	default:
		u.logger.WarnContext(ctx, "capture upload queue full, leaving capture in the spool",
			slog.String("connection_uid", connectionUID.String()))
	}
}

// SweepSpool queues every capture left in the spool directory for upload and
// returns how many it queued.
//
// This is the crash-recovery half: a session whose process died never reached
// Finish, so its (valid, partial) capture is sitting in the spool with nobody
// to upload it. Call it once at startup, before any proxy accepts — at that
// moment every file in the spool is by definition finished, which is what makes
// a blind sweep safe.
func (u *Uploader) SweepSpool(ctx context.Context) (int, error) {
	if u == nil {
		return 0, nil
	}

	entries, err := os.ReadDir(u.spoolDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}

		return 0, fmt.Errorf("read spool dir: %w", err)
	}

	queued := 0

	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != FileExt {
			continue
		}

		uid, err := uuid.Parse(strings.TrimSuffix(e.Name(), FileExt))
		if err != nil {
			// Not a capture we named; leave it to the retention sweep.
			continue
		}

		u.Finish(ctx, uid)

		queued++
	}

	return queued, nil
}

// Open streams a previously uploaded capture back. The caller closes the
// reader. Returns fs.ErrNotExist (wrapped) when the object is gone.
func (u *Uploader) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	if u == nil || key == "" {
		return nil, os.ErrNotExist
	}

	r, err := u.bucket.NewReader(ctx, key, nil)
	if err != nil {
		if gcerrors.Code(err) == gcerrors.NotFound {
			return nil, fmt.Errorf("capture %q: %w", key, os.ErrNotExist)
		}

		return nil, fmt.Errorf("open capture %q: %w", key, err)
	}

	return r, nil
}

// Stat returns the size in bytes of a previously uploaded capture, without
// reading its body. Returns fs.ErrNotExist (wrapped) when the object is gone.
func (u *Uploader) Stat(ctx context.Context, key string) (int64, error) {
	if u == nil || key == "" {
		return 0, os.ErrNotExist
	}

	attrs, err := u.bucket.Attributes(ctx, key)
	if err != nil {
		if gcerrors.Code(err) == gcerrors.NotFound {
			return 0, fmt.Errorf("capture %q: %w", key, os.ErrNotExist)
		}

		return 0, fmt.Errorf("stat capture %q: %w", key, err)
	}

	return attrs.Size, nil
}

// Delete removes an uploaded capture. A missing object is not an error: the
// caller's intent (the capture should be gone) is already satisfied.
func (u *Uploader) Delete(ctx context.Context, key string) error {
	if u == nil || key == "" {
		return nil
	}

	if err := u.bucket.Delete(ctx, key); err != nil {
		if gcerrors.Code(err) == gcerrors.NotFound {
			return nil
		}

		return fmt.Errorf("delete capture %q: %w", key, err)
	}

	return nil
}

// Close stops accepting new work, drains what is queued and releases the
// bucket. Safe on a nil Uploader and safe to call twice.
func (u *Uploader) Close() error {
	if u == nil {
		return nil
	}

	u.closeMu.Lock()

	if u.done {
		u.closeMu.Unlock()

		return nil
	}

	u.done = true
	close(u.closed)
	close(u.jobs)
	u.closeMu.Unlock()

	u.wg.Wait()

	if err := u.bucket.Close(); err != nil {
		return fmt.Errorf("close capture bucket: %w", err)
	}

	return nil
}

// work drains the upload queue.
func (u *Uploader) work(ctx context.Context) {
	defer u.wg.Done()

	for uid := range u.jobs {
		// Per job rather than around the loop: a panic must not end the process,
		// and must not retire this worker either — a proxy that silently stopped
		// shipping captures is worse than one that loses a single upload. See
		// safe.RunMaintenance.
		safe.RunMaintenance(ctx, u.logger, goroutineNameUpload, func() {
			u.uploadWithRetry(ctx, uid)
		})
	}
}

// uploadWithRetry runs one upload, retrying a few times on transient failure.
// Giving up is not data loss: the spool file is only removed after both the
// object and the recorded key are in place, so a capture that never uploads is
// still served locally and still picked up by the next startup sweep.
func (u *Uploader) uploadWithRetry(ctx context.Context, uid uuid.UUID) {
	for attempt := 1; attempt <= uploadAttempts; attempt++ {
		attemptCtx, cancel := context.WithTimeout(ctx, uploadTimeout)
		key, err := u.upload(attemptCtx, uid)

		cancel()

		if err == nil {
			u.logger.DebugContext(ctx, "capture uploaded",
				slog.String("connection_uid", uid.String()),
				slog.String("key", key))

			return
		}

		if errors.Is(err, os.ErrNotExist) {
			// Already uploaded (a duplicate sweep), or never captured.
			return
		}

		if attempt == uploadAttempts {
			u.logger.ErrorContext(ctx, "capture upload failed, leaving capture in the spool",
				slog.String("connection_uid", uid.String()),
				slog.Int("attempts", attempt),
				slog.Any("error", err))

			return
		}

		select {
		case <-time.After(uploadRetryDelay * time.Duration(attempt)):
		case <-u.closed:
			// Shutting down: the spool file stays, the next start sweeps it.
			return
		}
	}
}

// upload copies one finished capture to the bucket, records its key on the
// connection row and only then removes the local copy.
//
// The order is the whole correctness argument. Uploading first means a failure
// leaves the capture readable where it already was. Recording the key before
// deleting means the API can always find the capture in exactly one place: the
// spool until the key is stored, the bucket afterwards. Deleting first, or
// recording last, would open a window where a capture exists but nothing knows
// where.
func (u *Uploader) upload(ctx context.Context, uid uuid.UUID) (string, error) {
	spoolPath := filepath.Join(u.spoolDir, uid.String()+FileExt)

	f, err := os.Open(spoolPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("capture %s: %w", uid, os.ErrNotExist)
		}

		return "", fmt.Errorf("open spooled capture: %w", err)
	}

	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("stat spooled capture: %w", err)
	}

	key := u.Key(uid, info.ModTime())

	w, err := u.bucket.NewWriter(ctx, key, &blob.WriterOptions{ContentType: ContentType})
	if err != nil {
		return "", fmt.Errorf("open capture object %q: %w", key, err)
	}

	if _, err := io.Copy(w, f); err != nil {
		_ = w.Close()

		return "", fmt.Errorf("upload capture %q: %w", key, err)
	}

	if err := w.Close(); err != nil {
		return "", fmt.Errorf("finalize capture %q: %w", key, err)
	}

	if err := u.recorder.SetConnectionDumpKey(ctx, uid, key); err != nil {
		return "", fmt.Errorf("record capture key %q: %w", key, err)
	}

	if err := os.Remove(spoolPath); err != nil && !os.IsNotExist(err) {
		// The capture is safely in the bucket and addressable; a stuck spool
		// file is the retention sweep's problem, not a failed upload.
		u.logger.WarnContext(ctx, "failed to remove spooled capture after upload",
			slog.String("path", spoolPath),
			slog.Any("error", err))
	}

	return key, nil
}
