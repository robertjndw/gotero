// Package s3 stores blobs in an S3 compatible bucket using minio-go.
//
// Uploads stream through an io.Pipe into a multipart PutObject of unknown
// size, aimed at <prefix>.staging/<random>. Commit then server-side copies the
// staged object to its final key, so the bytes are never held in memory and
// never re-downloaded. Peak memory per in-flight upload is about one part
// (Config.PartSize, 16 MiB by default).
//
//	b, err := s3.New(ctx, s3.Config{
//		Endpoint:  "s3.example.com",
//		Bucket:    "gotero",
//		AccessKey: "...",
//		SecretKey: "...",
//		UseSSL:    true,
//	})
package s3

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/robertjndw/gotero/storage"
)

const (
	// DefaultPartSize is the multipart part size used when Config.PartSize is 0.
	DefaultPartSize = 16 << 20
	// MinPartSize is the smallest part size S3 accepts (except for the last part).
	MinPartSize = 5 << 20
)

// Config describes the bucket to use.
type Config struct {
	// Endpoint is host[:port] without scheme.
	Endpoint string
	// Region is optional; most S3 clones ignore it.
	Region string
	// Bucket must exist unless CreateBucket is set.
	Bucket    string
	AccessKey string
	SecretKey string
	// Prefix namespaces every key (and the staging area) inside the bucket.
	Prefix string
	// UseSSL selects https.
	UseSSL bool
	// PartSize is the multipart part size in bytes and therefore the main
	// driver of per-upload memory. Defaults to DefaultPartSize; values below
	// MinPartSize are rejected. S3 allows at most 10000 parts, so the largest
	// object is PartSize*10000 (160 GB with the default).
	PartSize uint64
	// CreateBucket makes New create the bucket if it is missing.
	CreateBucket bool
}

// Backend is a storage.Backend on S3.
type Backend struct {
	cli      *minio.Client
	bucket   string
	prefix   string // "" or ends with "/"
	staging  string // always ends with "/"
	partSize uint64
}

var _ storage.Backend = (*Backend)(nil)

// New connects to the bucket described by cfg and verifies access to it.
func New(ctx context.Context, cfg Config) (*Backend, error) {
	if cfg.Endpoint == "" || cfg.Bucket == "" {
		return nil, errors.New("s3: endpoint and bucket are required")
	}
	if cfg.PartSize == 0 {
		cfg.PartSize = DefaultPartSize
	}
	if cfg.PartSize < MinPartSize {
		return nil, fmt.Errorf("s3: part size %d below the S3 minimum of %d", cfg.PartSize, MinPartSize)
	}
	cli, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: cfg.UseSSL,
		Region: cfg.Region,
	})
	if err != nil {
		return nil, fmt.Errorf("s3: %w", err)
	}
	prefix := strings.Trim(cfg.Prefix, "/")
	if prefix != "" {
		prefix += "/"
	}
	b := &Backend{
		cli:      cli,
		bucket:   cfg.Bucket,
		prefix:   prefix,
		staging:  prefix + storage.StagingDir + "/",
		partSize: cfg.PartSize,
	}
	exists, err := cli.BucketExists(ctx, cfg.Bucket)
	if err != nil {
		return nil, fmt.Errorf("s3: check bucket: %w", err)
	}
	if !exists {
		if !cfg.CreateBucket {
			return nil, fmt.Errorf("s3: bucket %q does not exist", cfg.Bucket)
		}
		if err := cli.MakeBucket(ctx, cfg.Bucket, minio.MakeBucketOptions{Region: cfg.Region}); err != nil {
			return nil, fmt.Errorf("s3: create bucket: %w", err)
		}
	}
	return b, nil
}

func (b *Backend) objectKey(key string) (string, error) {
	if err := storage.ValidateKey(key); err != nil {
		return "", err
	}
	return b.prefix + key, nil
}

// mapErr turns S3 "no such object" answers into storage.ErrNotFound.
func mapErr(err error) error {
	if err == nil {
		return nil
	}
	resp := minio.ToErrorResponse(err)
	switch {
	case resp.Code == "NoSuchKey" || resp.Code == "NotFound":
		return fmt.Errorf("%w: %w", storage.ErrNotFound, err)
	case resp.StatusCode == http.StatusNotFound && resp.Code != "NoSuchBucket":
		return fmt.Errorf("%w: %w", storage.ErrNotFound, err)
	}
	return err
}

func (b *Backend) info(key string, oi minio.ObjectInfo) storage.Info {
	return storage.Info{Key: key, Size: oi.Size, ModTime: oi.LastModified}
}

// Stat implements storage.Backend.
func (b *Backend) Stat(ctx context.Context, key string) (storage.Info, error) {
	ok, err := b.objectKey(key)
	if err != nil {
		return storage.Info{}, err
	}
	oi, err := b.cli.StatObject(ctx, b.bucket, ok, minio.StatObjectOptions{})
	if err != nil {
		return storage.Info{}, mapErr(err)
	}
	return b.info(key, oi), nil
}

// Open implements storage.Backend. The reader also implements io.Seeker and
// io.ReaderAt; seeking is served by ranged GETs.
func (b *Backend) Open(ctx context.Context, key string) (storage.Reader, storage.Info, error) {
	ok, err := b.objectKey(key)
	if err != nil {
		return nil, storage.Info{}, err
	}
	obj, err := b.cli.GetObject(ctx, b.bucket, ok, minio.GetObjectOptions{})
	if err != nil {
		return nil, storage.Info{}, mapErr(err)
	}
	// GetObject is lazy, so a missing key would only surface on the first
	// Read. Stat forces the request and gives us the size anyway.
	oi, err := obj.Stat()
	if err != nil {
		_ = obj.Close()
		return nil, storage.Info{}, mapErr(err)
	}
	return &reader{Object: obj}, b.info(key, oi), nil
}

type reader struct{ *minio.Object }

func (r *reader) Read(p []byte) (int, error) {
	n, err := r.Object.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		err = mapErr(err)
	}
	return n, err
}

// Delete implements storage.Backend. S3 already treats deleting a missing key
// as success.
func (b *Backend) Delete(ctx context.Context, key string) error {
	ok, err := b.objectKey(key)
	if err != nil {
		return err
	}
	if err := b.cli.RemoveObject(ctx, b.bucket, ok, minio.RemoveObjectOptions{}); err != nil {
		if errors.Is(mapErr(err), storage.ErrNotFound) {
			return nil
		}
		return err
	}
	return nil
}

// Walk implements storage.Backend. Keys come back in lexical order.
func (b *Backend) Walk(ctx context.Context, prefix string, fn func(storage.Info) error) error {
	if err := storage.ValidatePrefix(prefix); err != nil {
		return err
	}
	lctx, cancel := context.WithCancel(ctx)
	defer cancel() // stops the lister goroutine if fn bails out early
	for oi := range b.cli.ListObjects(lctx, b.bucket, minio.ListObjectsOptions{
		Prefix:    b.prefix + prefix,
		Recursive: true,
	}) {
		if oi.Err != nil {
			return oi.Err
		}
		if strings.HasPrefix(oi.Key, b.staging) || strings.HasSuffix(oi.Key, "/") {
			continue
		}
		if err := fn(b.info(strings.TrimPrefix(oi.Key, b.prefix), oi)); err != nil {
			return err
		}
	}
	return ctx.Err()
}

// SweepStaging implements storage.Backend. It removes stale staging objects
// and aborts incomplete multipart uploads under the staging prefix. Ages come
// from the server's clock; olderThan <= 0 removes everything.
func (b *Backend) SweepStaging(ctx context.Context, olderThan time.Duration) (int, error) {
	cutoff := time.Now().Add(-olderThan)
	stale := func(t time.Time) bool { return olderThan <= 0 || t.Before(cutoff) }
	removed := 0

	var keys []string
	for oi := range b.cli.ListObjects(ctx, b.bucket, minio.ListObjectsOptions{Prefix: b.staging, Recursive: true}) {
		if oi.Err != nil {
			return removed, oi.Err
		}
		if stale(oi.LastModified) {
			keys = append(keys, oi.Key)
		}
	}
	for _, k := range keys {
		if err := b.cli.RemoveObject(ctx, b.bucket, k, minio.RemoveObjectOptions{}); err != nil {
			return removed, err
		}
		removed++
	}

	// Core instead of ListIncompleteUploads: the high level helper also asks
	// for the size of every upload and aborts the whole listing when one of
	// them vanishes meanwhile, which is routine while uploads are finishing.
	core := minio.Core{Client: b.cli}
	var keyMarker, idMarker string
	for {
		res, err := core.ListMultipartUploads(ctx, b.bucket, b.staging, keyMarker, idMarker, "", 1000)
		if code := minio.ToErrorResponse(err).Code; code == "NoSuchUpload" || code == "NotImplemented" {
			// Servers without (working) multipart listing, rclone among them,
			// have nothing we could abort.
			break
		}
		if err != nil {
			return removed, err
		}
		for _, mi := range res.Uploads {
			// Some S3 clones (rclone serve s3) answer with junk entries that
			// have no upload ID; there is nothing to abort there.
			if mi.UploadID == "" || mi.Key == "" || strings.HasSuffix(mi.Key, "/") {
				continue
			}
			// An unknown start time is treated as fresh unless everything goes.
			if olderThan > 0 && (mi.Initiated.IsZero() || !stale(mi.Initiated)) {
				continue
			}
			err := core.AbortMultipartUpload(ctx, b.bucket, mi.Key, mi.UploadID)
			if minio.ToErrorResponse(err).Code == "NoSuchUpload" {
				continue // finished or aborted by its owner in the meantime
			}
			if err != nil {
				return removed, err
			}
			removed++
		}
		if !res.IsTruncated {
			break
		}
		keyMarker, idMarker = res.NextKeyMarker, res.NextUploadIDMarker
	}
	return removed, ctx.Err()
}
