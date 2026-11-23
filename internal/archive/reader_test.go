package archive

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/require"
	"gocloud.dev/blob"
	_ "gocloud.dev/blob/fileblob" // file:// buckets for the test
)

// An object without a sealed hash is never served unchecked, whether it is
// downloaded or read whole for a page.
func TestDownloadRequiresAHash(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	bucket, err := blob.OpenBucket(ctx, "file://"+t.TempDir())
	require.NoError(t, err)

	t.Cleanup(func() { _ = bucket.Close() })

	data := []byte("not really parquet")
	require.NoError(t, bucket.WriteAll(ctx, "obj.parquet", data, nil))

	sum := sha256.Sum256(data)

	got, err := Download(ctx, bucket, Object{Key: "obj.parquet", Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:])})
	require.NoError(t, err)
	require.Equal(t, data, got)

	_, err = Download(ctx, bucket, Object{Key: "obj.parquet", Size: int64(len(data))})
	require.ErrorIs(t, err, ErrHashMismatch)

	_, err = Read[RowRecord](ctx, bucket, Object{Key: "obj.parquet", Size: int64(len(data))}, 0, 1)
	require.ErrorIs(t, err, ErrHashMismatch)
}
