// Package bucket opens the object storage dbbat talks to.
//
// There are exactly two drivers: s3:// (AWS S3 and anything that speaks its
// API: MinIO, Ceph, R2, Scaleway, Google Cloud Storage through its XML API)
// and file:// (a directory). No Google or Azure SDK is linked into the binary.
package bucket

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"gocloud.dev/blob"

	// The only two drivers dbbat ships.
	_ "gocloud.dev/blob/fileblob"
	_ "gocloud.dev/blob/s3blob"
)

// Errors returned by Normalize and Open.
var (
	// ErrNoScheme is returned for a URL with no scheme.
	ErrNoScheme = errors.New("bucket URL has no scheme (expected e.g. s3://bucket/prefix)")

	// ErrUnsupportedScheme is returned for a scheme dbbat has no driver for.
	ErrUnsupportedScheme = errors.New("bucket URL scheme is not supported")
)

// Normalize validates the scheme of a bucket URL and turns the documented
// "s3://bucket/prefix" form into what gocloud.dev understands.
//
// The s3 opener reads the bucket from the URL host and ignores the path, so
// the path is folded into the "prefix" query parameter. file:// and mem://
// URLs are passed through untouched (their path is the location).
func Normalize(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parse bucket URL %q: %w", raw, err)
	}

	switch u.Scheme {
	case "":
		return "", fmt.Errorf("%q: %w", raw, ErrNoScheme)
	case "file", "mem":
		return raw, nil
	case "s3":
	case "gs", "azblob":
		return "", fmt.Errorf("%q: %w: dbbat has no %s:// driver. Reach the store "+
			"through its S3 API instead (s3://bucket/prefix?endpoint=...), see "+
			"the \"Other clouds\" section of the session dumps documentation",
			raw, ErrUnsupportedScheme, u.Scheme)
	default:
		return "", fmt.Errorf("%q: %w (use s3:// or file://)", raw, ErrUnsupportedScheme)
	}

	prefix := strings.Trim(u.Path, "/")
	if prefix == "" {
		return raw, nil
	}

	q := u.Query()
	// An explicit ?prefix= wins: it is the driver-native spelling.
	if q.Get("prefix") == "" {
		q.Set("prefix", prefix+"/")
	}

	u.Path = ""
	u.RawQuery = q.Encode()

	return u.String(), nil
}

// Open opens the bucket behind a dbbat bucket URL.
func Open(ctx context.Context, raw string) (*blob.Bucket, error) {
	normalized, err := Normalize(raw)
	if err != nil {
		return nil, err
	}

	b, err := blob.OpenBucket(ctx, normalized)
	if err != nil {
		return nil, fmt.Errorf("open bucket: %w", err)
	}

	return b, nil
}
