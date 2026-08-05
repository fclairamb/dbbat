package dump

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recorder is an in-memory Recorder. It doubles as the assertion surface for
// "the key was stored before the local copy was dropped".
type recorder struct {
	mu   sync.Mutex
	keys map[uuid.UUID]string
	fail error
}

func newRecorder() *recorder {
	return &recorder{keys: map[uuid.UUID]string{}}
}

func (r *recorder) SetConnectionDumpKey(_ context.Context, uid uuid.UUID, key string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.fail != nil {
		return r.fail
	}

	r.keys[uid] = key

	return nil
}

func (r *recorder) key(uid uuid.UUID) string {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.keys[uid]
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return len(r.keys)
}

// newTestUploader builds an uploader writing to a file:// bucket in a temp dir.
// The file driver is a real gocloud blob bucket, so the whole upload path — URL
// opening, object writes, reads, deletes — is exercised without any cloud.
func newTestUploader(t *testing.T) (*Uploader, string, *recorder) {
	t.Helper()

	spool := t.TempDir()
	bucketDir := t.TempDir()
	rec := newRecorder()

	u, err := OpenUploader(t.Context(), UploaderOptions{
		URL:        "file://" + bucketDir,
		SpoolDir:   spool,
		InstanceID: "test-instance",
		Recorder:   rec,
	})
	require.NoError(t, err)
	require.NotNil(t, u)

	t.Cleanup(func() { _ = u.Close() })

	return u, spool, rec
}

// spoolCapture writes a plausible finished capture into the spool.
func spoolCapture(t *testing.T, spool string, uid uuid.UUID, body string) string {
	t.Helper()

	path := filepath.Join(spool, uid.String()+FileExt)
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))

	return path
}

// waitForKey waits for the asynchronous upload worker to record a key.
func waitForKey(t *testing.T, rec *recorder, uid uuid.UUID) string {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if k := rec.key(uid); k != "" {
			return k
		}

		time.Sleep(5 * time.Millisecond)
	}

	t.Fatalf("no key recorded for %s", uid)

	return ""
}

func TestOpenUploaderDisabledByDefault(t *testing.T) {
	t.Parallel()

	// An empty URL is how "local-only captures" is expressed, and it must stay
	// the default: a nil uploader that every call site can use unconditionally.
	u, err := OpenUploader(t.Context(), UploaderOptions{SpoolDir: t.TempDir()})
	require.NoError(t, err)
	assert.Nil(t, u)

	// Every method is nil-safe, which is what lets the proxies skip branching.
	u.Finish(t.Context(), uuid.New())

	queued, err := u.SweepSpool(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 0, queued)

	_, err = u.Open(t.Context(), "whatever")
	require.ErrorIs(t, err, os.ErrNotExist)

	_, err = u.Stat(t.Context(), "whatever")
	require.ErrorIs(t, err, os.ErrNotExist)

	require.NoError(t, u.Delete(t.Context(), "whatever"))
	require.NoError(t, u.Close())
}

func TestOpenUploaderRequiresSpoolAndRecorder(t *testing.T) {
	t.Parallel()

	_, err := OpenUploader(t.Context(), UploaderOptions{URL: "file://" + t.TempDir()})
	require.Error(t, err)

	_, err = OpenUploader(t.Context(), UploaderOptions{
		URL:      "file://" + t.TempDir(),
		SpoolDir: t.TempDir(),
	})
	require.Error(t, err)
}

func TestNormalizeBucketURL(t *testing.T) {
	t.Parallel()

	// The s3 opener reads the bucket from the host and ignores the path, so a
	// documented "s3://bucket/prefix" has to become a prefix query parameter or
	// every capture silently lands at the bucket root.
	got, err := normalizeBucketURL("s3://my-bucket/captures/dbbat")
	require.NoError(t, err)
	assert.Equal(t, "s3://my-bucket?prefix=captures%2Fdbbat%2F", got)

	// No path: nothing to fold in.
	got, err = normalizeBucketURL("s3://my-bucket")
	require.NoError(t, err)
	assert.Equal(t, "s3://my-bucket", got)

	// Other query parameters survive.
	got, err = normalizeBucketURL("s3://my-bucket/captures?region=eu-west-1")
	require.NoError(t, err)
	assert.Contains(t, got, "region=eu-west-1")
	assert.Contains(t, got, "prefix=captures%2F")

	// An explicit prefix wins over the path.
	got, err = normalizeBucketURL("s3://my-bucket/ignored?prefix=explicit/")
	require.NoError(t, err)
	assert.Contains(t, got, "prefix=explicit%2F")
	assert.NotContains(t, got, "ignored")

	// file:// paths are the location, not a prefix.
	got, err = normalizeBucketURL("file:///var/lib/dbbat/captures")
	require.NoError(t, err)
	assert.Equal(t, "file:///var/lib/dbbat/captures", got)

	_, err = normalizeBucketURL("my-bucket/prefix")
	require.Error(t, err)
}

func TestKeyLayout(t *testing.T) {
	t.Parallel()

	u, _, _ := newTestUploader(t)

	uid := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	at := time.Date(2026, 8, 5, 13, 0, 0, 0, time.UTC)

	assert.Equal(t,
		"2026/08/05/test-instance/11111111-2222-3333-4444-555555555555.pcapng",
		u.Key(uid, at))

	// An instance id with a slash must not invent key segments.
	assert.Equal(t, "unknown", sanitizeKeySegment(""))
	assert.Equal(t, "a_b", sanitizeKeySegment("a/b"))
}

func TestFinishUploadsRecordsAndClearsSpool(t *testing.T) {
	t.Parallel()

	u, spool, rec := newTestUploader(t)

	uid := uuid.New()
	path := spoolCapture(t, spool, uid, "pcapng-bytes")

	u.Finish(t.Context(), uid)

	key := waitForKey(t, rec, uid)
	assert.Contains(t, key, uid.String()+FileExt)
	assert.Contains(t, key, "test-instance")

	// Read-through: the object serves the exact bytes that were spooled.
	body, err := u.Open(t.Context(), key)
	require.NoError(t, err)

	data, err := io.ReadAll(body)
	require.NoError(t, err)
	require.NoError(t, body.Close())
	assert.Equal(t, "pcapng-bytes", string(data))

	// The local copy is dropped only once the key is stored, so a capture is
	// always addressable in exactly one place.
	assert.NoFileExists(t, path)
}

func TestFinishUsesSpoolModTimeForTheDateSegments(t *testing.T) {
	t.Parallel()

	u, spool, rec := newTestUploader(t)

	uid := uuid.New()
	path := spoolCapture(t, spool, uid, "x")

	// Deriving the date from the file rather than the clock is what makes the
	// key stable across a retry or a later startup sweep.
	when := time.Date(2024, 3, 9, 4, 5, 6, 0, time.UTC)
	require.NoError(t, os.Chtimes(path, when, when))

	u.Finish(t.Context(), uid)

	key := waitForKey(t, rec, uid)
	assert.Equal(t, u.Key(uid, when), key)
	assert.Contains(t, key, "2024/03/09/")
}

func TestFinishOnMissingCaptureIsNotAnError(t *testing.T) {
	t.Parallel()

	u, _, rec := newTestUploader(t)

	u.Finish(t.Context(), uuid.New())

	// Nothing to upload, nothing recorded, and no retry storm.
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, 0, rec.count())
}

func TestFinishKeepsTheCaptureWhenTheKeyCannotBeRecorded(t *testing.T) {
	t.Parallel()

	spool := t.TempDir()
	bucketDir := t.TempDir()
	rec := newRecorder()
	rec.fail = assert.AnError

	u, err := OpenUploader(t.Context(), UploaderOptions{
		URL:        "file://" + bucketDir,
		SpoolDir:   spool,
		InstanceID: "test-instance",
		Recorder:   rec,
	})
	require.NoError(t, err)

	uid := uuid.New()
	path := spoolCapture(t, spool, uid, "x")

	u.Finish(t.Context(), uid)
	require.NoError(t, u.Close()) // drains, retries, gives up

	// An unrecordable key means nobody could find the object again, so the
	// local copy must survive — it is still servable, and still sweepable.
	assert.FileExists(t, path)
}

func TestSweepSpoolRecoversCapturesLeftByACrash(t *testing.T) {
	t.Parallel()

	u, spool, rec := newTestUploader(t)

	first, second := uuid.New(), uuid.New()
	spoolCapture(t, spool, first, "one")
	spoolCapture(t, spool, second, "two")

	// Files that are not ours are left strictly alone.
	require.NoError(t, os.WriteFile(filepath.Join(spool, "notes.txt"), []byte("x"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(spool, "not-a-uuid"+FileExt), []byte("x"), 0o600))

	queued, err := u.SweepSpool(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 2, queued)

	assert.NotEmpty(t, waitForKey(t, rec, first))
	assert.NotEmpty(t, waitForKey(t, rec, second))

	assert.FileExists(t, filepath.Join(spool, "notes.txt"))
	assert.FileExists(t, filepath.Join(spool, "not-a-uuid"+FileExt))
}

func TestSweepSpoolOnAMissingDirIsNotAnError(t *testing.T) {
	t.Parallel()

	rec := newRecorder()

	u, err := OpenUploader(t.Context(), UploaderOptions{
		URL:      "file://" + t.TempDir(),
		SpoolDir: filepath.Join(t.TempDir(), "does-not-exist"),
		Recorder: rec,
	})
	require.NoError(t, err)

	t.Cleanup(func() { _ = u.Close() })

	queued, err := u.SweepSpool(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 0, queued)
}

func TestDeleteRemovesTheObjectAndToleratesAMissingOne(t *testing.T) {
	t.Parallel()

	u, spool, rec := newTestUploader(t)

	uid := uuid.New()
	spoolCapture(t, spool, uid, "x")
	u.Finish(t.Context(), uid)

	key := waitForKey(t, rec, uid)

	require.NoError(t, u.Delete(t.Context(), key))

	_, err := u.Open(t.Context(), key)
	require.ErrorIs(t, err, os.ErrNotExist)

	// Deleting again is fine: the caller's intent is already satisfied.
	require.NoError(t, u.Delete(t.Context(), key))
	require.NoError(t, u.Delete(t.Context(), ""))
}

func TestStatReturnsTheUploadedObjectSizeAndToleratesAMissingOne(t *testing.T) {
	t.Parallel()

	u, spool, rec := newTestUploader(t)

	uid := uuid.New()
	spoolCapture(t, spool, uid, "twelve-bytes")
	u.Finish(t.Context(), uid)

	key := waitForKey(t, rec, uid)

	size, err := u.Stat(t.Context(), key)
	require.NoError(t, err)
	assert.EqualValues(t, len("twelve-bytes"), size)

	require.NoError(t, u.Delete(t.Context(), key))

	_, err = u.Stat(t.Context(), key)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestCloseIsIdempotentAndStopsAcceptingWork(t *testing.T) {
	t.Parallel()

	u, spool, rec := newTestUploader(t)

	require.NoError(t, u.Close())
	require.NoError(t, u.Close())

	uid := uuid.New()
	spoolCapture(t, spool, uid, "x")

	// Must not panic on a closed queue, and must not upload after shutdown.
	u.Finish(t.Context(), uid)
	assert.Equal(t, 0, rec.count())
}
