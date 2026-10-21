package s3

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"

	"github.com/minio/minio-go/v7"

	"github.com/robertjndw/gotero/storage"
)

// maxSingleCopy is the largest source a single CopyObject request may handle.
const maxSingleCopy = 5 << 30

var errAborted = errors.New("s3: upload aborted")

// upload buffers the first part in memory. Most attachments are smaller
// than one part, and for those Commit issues a single PutObject straight to
// the final key: one request instead of multipart init, upload, complete,
// copy and delete, and a single PUT is atomic so no staging object is needed.
// Only once the buffer would overflow does the upload switch to streaming a
// multipart upload into the staging area. Peak memory stays one part either
// way, which is what minio-go buffers for streaming uploads anyway.
type upload struct {
	b      *Backend
	ctx    context.Context // Create's context
	cancel context.CancelFunc
	key    string       // full staging key
	n      atomic.Int64 // bytes accepted by Write

	buf       []byte // pending bytes while not streaming
	streaming bool
	pw        *io.PipeWriter
	done      chan struct{} // closed when the PutObject goroutine exits
	putErr    error         // valid after done is closed

	mu        sync.Mutex
	committed bool
}

// Create implements storage.Backend. The upload runs on ctx: cancelling it
// aborts the transfer, so pass a context that outlives the whole upload.
func (b *Backend) Create(ctx context.Context) (storage.Upload, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	name, err := storage.NewStagingName()
	if err != nil {
		return nil, err
	}
	uctx, cancel := context.WithCancel(ctx)
	return &upload{b: b, ctx: uctx, cancel: cancel, key: b.staging + name}, nil
}

// startStreaming hands everything buffered so far to a multipart PutObject
// running in the background and routes later writes through a pipe.
func (u *upload) startStreaming() error {
	pr, pw := io.Pipe()
	u.pw, u.done, u.streaming = pw, make(chan struct{}), true
	go func() {
		defer close(u.done)
		_, err := u.b.cli.PutObject(u.ctx, u.b.bucket, u.key, pr, -1, minio.PutObjectOptions{
			PartSize:    u.b.partSize,
			ContentType: "application/octet-stream",
			// Parallel part uploads would multiply the buffer memory; the
			// streaming path is sequential anyway.
			NumThreads: 1,
		})
		u.putErr = err
		if err == nil {
			// PutObject returned without draining (it should not); make any
			// further Write fail instead of blocking forever.
			err = io.ErrClosedPipe
		}
		pr.CloseWithError(err)
	}()
	buf := u.buf
	u.buf = nil
	_, err := pw.Write(buf)
	return err
}

func (u *upload) Write(p []byte) (int, error) {
	if err := u.ctx.Err(); err != nil {
		return 0, err
	}
	u.mu.Lock()
	if u.committed {
		u.mu.Unlock()
		return 0, errors.New("s3: write after commit or abort")
	}
	if !u.streaming {
		if uint64(len(u.buf))+uint64(len(p)) <= u.b.partSize {
			u.buf = append(u.buf, p...)
			u.mu.Unlock()
			u.n.Add(int64(len(p)))
			return len(p), nil
		}
		if err := u.startStreaming(); err != nil {
			u.mu.Unlock()
			return 0, err
		}
	}
	pw := u.pw
	// Don't hold the lock while blocked on the pipe, or a concurrent Abort
	// could never cancel the stalled transfer.
	u.mu.Unlock()
	n, err := pw.Write(p)
	u.n.Add(int64(n))
	return n, err
}

func (u *upload) Commit(ctx context.Context, key string) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.committed {
		return errors.New("s3: upload already committed")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	dst, err := u.b.objectKey(key)
	if err != nil {
		return err
	}
	if !u.streaming {
		return u.commitSmall(ctx, dst)
	}
	_ = u.pw.Close() // EOF for PutObject; it finalizes the multipart upload
	select {
	case <-u.done:
	case <-ctx.Done():
		return ctx.Err()
	}
	if u.putErr != nil {
		return fmt.Errorf("s3: upload staging object: %w", u.putErr)
	}

	cli, bucket := u.b.cli, u.b.bucket
	// Content-addressed callers expect "already there" to be a success. The
	// Stat/Copy pair is not atomic, but two racing writers of the same key
	// carry identical bytes, so a doubled copy is harmless.
	if _, err := cli.StatObject(ctx, bucket, dst, minio.StatObjectOptions{}); err == nil {
		return u.finish(ctx)
	} else if !errors.Is(mapErr(err), storage.ErrNotFound) {
		return fmt.Errorf("s3: check destination: %w", err)
	}
	srcInfo, err := cli.StatObject(ctx, bucket, u.key, minio.StatObjectOptions{})
	if err != nil {
		return fmt.Errorf("s3: stat staging object: %w", err)
	}
	dstOpts := minio.CopyDestOptions{Bucket: bucket, Object: dst}
	srcOpts := minio.CopySrcOptions{Bucket: bucket, Object: u.key}
	if srcInfo.Size <= maxSingleCopy {
		// Plain CopyObject is the most widely implemented server-side copy
		// (some S3 clones lack UploadPartCopy, which ComposeObject needs).
		_, err = cli.CopyObject(ctx, dstOpts, srcOpts)
	} else {
		// CopyObject caps at 5 GiB; compose switches to multipart copy.
		_, err = cli.ComposeObject(ctx, dstOpts, srcOpts)
	}
	if err != nil {
		return fmt.Errorf("s3: copy staging object into place: %w", err)
	}
	return u.finish(ctx)
}

// commitSmall writes a fully buffered upload directly to its final key.
func (u *upload) commitSmall(ctx context.Context, dst string) error {
	cli, bucket := u.b.cli, u.b.bucket
	defer u.cancel()
	if _, err := cli.StatObject(ctx, bucket, dst, minio.StatObjectOptions{}); err == nil {
		u.committed, u.buf = true, nil
		return nil
	} else if !errors.Is(mapErr(err), storage.ErrNotFound) {
		return fmt.Errorf("s3: check destination: %w", err)
	}
	if _, err := cli.PutObject(ctx, bucket, dst, bytes.NewReader(u.buf), int64(len(u.buf)), minio.PutObjectOptions{
		ContentType: "application/octet-stream",
		// Over plain HTTP minio would otherwise use aws-chunked signing,
		// which some servers refuse, notably for an empty body.
		DisableContentSha256: true,
	}); err != nil {
		return fmt.Errorf("s3: put object: %w", err)
	}
	u.committed, u.buf = true, nil
	return nil
}

// finish removes the staging object after the data is safely at its key.
func (u *upload) finish(ctx context.Context) error {
	u.committed = true
	u.cancel()
	// The object is already visible, so a failed cleanup must not fail the
	// Commit; SweepStaging will collect the leftover.
	_ = u.b.cli.RemoveObject(context.WithoutCancel(ctx), u.b.bucket, u.key, minio.RemoveObjectOptions{})
	return nil
}

func (u *upload) Abort(ctx context.Context) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.committed {
		return nil
	}
	u.committed = true // no further Commit after Abort
	u.buf = nil
	// Cancel before waiting: PutObject may be stuck mid-request.
	u.cancel()
	if !u.streaming {
		return nil // nothing reached the server yet
	}
	_ = u.pw.CloseWithError(errAborted)
	<-u.done

	// The caller's ctx may be the very one that was cancelled, and cleanup
	// still has to happen.
	cctx := context.WithoutCancel(ctx)
	// minio aborts its own multipart upload on error, but with the (now
	// cancelled) upload context, so do it again explicitly. Best effort:
	// SweepStaging retries both.
	_ = u.b.cli.RemoveIncompleteUpload(cctx, u.b.bucket, u.key)
	err := u.b.cli.RemoveObject(cctx, u.b.bucket, u.key, minio.RemoveObjectOptions{})
	if err != nil && !errors.Is(mapErr(err), storage.ErrNotFound) {
		return err
	}
	return nil
}

var _ storage.Upload = (*upload)(nil)
