package bucket

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizeBucketURL(t *testing.T) {
	t.Parallel()

	// The s3 opener reads the bucket from the host and ignores the path, so a
	// documented "s3://bucket/prefix" has to become a prefix query parameter or
	// every capture silently lands at the bucket root.
	got, err := Normalize("s3://my-bucket/captures/dbbat")
	require.NoError(t, err)
	assert.Equal(t, "s3://my-bucket?prefix=captures%2Fdbbat%2F", got)

	// No path: nothing to fold in.
	got, err = Normalize("s3://my-bucket")
	require.NoError(t, err)
	assert.Equal(t, "s3://my-bucket", got)

	// Other query parameters survive.
	got, err = Normalize("s3://my-bucket/captures?region=eu-west-1")
	require.NoError(t, err)
	assert.Contains(t, got, "region=eu-west-1")
	assert.Contains(t, got, "prefix=captures%2F")

	// An explicit prefix wins over the path.
	got, err = Normalize("s3://my-bucket/ignored?prefix=explicit/")
	require.NoError(t, err)
	assert.Contains(t, got, "prefix=explicit%2F")
	assert.NotContains(t, got, "ignored")

	// file:// paths are the location, not a prefix.
	got, err = Normalize("file:///var/lib/dbbat/captures")
	require.NoError(t, err)
	assert.Equal(t, "file:///var/lib/dbbat/captures", got)

	_, err = Normalize("my-bucket/prefix")
	require.Error(t, err)
}

// Every documented scheme opens; the two clouds dbbat does not link a driver
// for are refused with a message that names the S3 route. The list cannot
// drift: docs and code say the same two schemes.
func TestOpenDocumentedSchemes(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	for _, raw := range []string{
		"file://" + dir,
		"file://" + dir + "?metadata=skip&no_tmp_dir=true",
		"s3://bucket/prefix?endpoint=http://localhost:9000&region=auto&use_path_style=true&" +
			"request_checksum_calculation=when_required&response_checksum_validation=when_required",
	} {
		b, err := Open(context.Background(), raw)
		require.NoError(t, err, raw)
		require.NoError(t, b.Close())
	}
}

func TestRefusedSchemes(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{"gs://bucket/prefix", "azblob://container/prefix", "ftp://x/y"} {
		_, err := Open(context.Background(), raw)
		require.ErrorIs(t, err, ErrUnsupportedScheme, raw)
		assert.NotContains(t, err.Error(), "no driver registered")
	}

	_, err := Open(context.Background(), "gs://bucket")
	assert.Contains(t, err.Error(), "S3")
}
