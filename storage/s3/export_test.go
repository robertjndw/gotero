package s3

import (
	"bytes"
	"context"

	"github.com/minio/minio-go/v7"
)

// PutOrphan writes a finished object into the staging area, as a crashed
// process would leave it.
func PutOrphan(ctx context.Context, b *Backend, name string, data []byte) error {
	_, err := b.cli.PutObject(ctx, b.bucket, b.staging+name, bytes.NewReader(data), int64(len(data)),
		minio.PutObjectOptions{DisableContentSha256: true})
	return err
}
