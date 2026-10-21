package server

import (
	"context"
	"fmt"

	"github.com/robertjndw/gotero/internal/config"
	"github.com/robertjndw/gotero/storage"
	"github.com/robertjndw/gotero/storage/local"
	"github.com/robertjndw/gotero/storage/s3"
	"github.com/robertjndw/gotero/storage/webdav"
)

// NewBackend builds the configured file storage backend.
func NewBackend(ctx context.Context, c config.Storage) (storage.Backend, error) {
	switch c.Backend {
	case "local":
		return local.New(c.LocalPath)
	case "s3":
		return s3.New(ctx, s3.Config{
			Endpoint:     c.S3Endpoint,
			Region:       c.S3Region,
			Bucket:       c.S3Bucket,
			AccessKey:    c.S3AccessKey,
			SecretKey:    c.S3SecretKey,
			Prefix:       c.S3Prefix,
			UseSSL:       c.S3UseSSL,
			CreateBucket: true,
		})
	case "webdav":
		return webdav.New(webdav.Config{URL: c.WebDAVURL, Username: c.WebDAVUsername, Password: c.WebDAVPassword})
	default:
		return nil, fmt.Errorf("unknown storage backend %q", c.Backend)
	}
}
