// Package s3 is the S3-flavor object-store client behind
// [objstore.Backend]. Compatible with MinIO, AWS S3, or any
// S3-compatible endpoint via minio-go v7. Callers should depend on
// the interface, not this concrete type.
package s3

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bbsakura/s3fs-ebpf/daemon/internal/objstore"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// Config holds connection settings. Endpoint is a host:port with no
// scheme; UseSSL toggles https vs http.
type Config struct {
	Endpoint  string
	Bucket    string
	AccessKey string
	SecretKey string
	UseSSL    bool
	// CacheDir enables local disk staging for Get(). Empty disables
	// caching. Analogous to s3fs-fuse's -o use_cache=<dir> (bench
	// §0.5 caveat).
	CacheDir string
	// MaxCacheBytes caps the disk cache size (S-07). 0 = unlimited.
	MaxCacheBytes int64
}

type Client struct {
	cfg   Config
	mc    *minio.Client
	cache objstore.DiskCache
}

// New constructs a Client bound to `cfg`. Does not validate
// connectivity — the first Stat/Get touches the network.
func New(cfg Config) (*Client, error) {
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("s3: endpoint must not be empty")
	}
	if cfg.Bucket == "" {
		return nil, fmt.Errorf("s3: bucket must not be empty")
	}
	mc, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: cfg.UseSSL,
	})
	if err != nil {
		return nil, fmt.Errorf("s3: init minio client: %w", err)
	}
	cache, err := objstore.NewDiskCache(cfg.CacheDir, objstore.KindS3, cfg.Bucket, cfg.MaxCacheBytes)
	if err != nil {
		return nil, err
	}
	return &Client{cfg: cfg, mc: mc, cache: cache}, nil
}

func (c *Client) Bucket() string { return c.cfg.Bucket }

func (c *Client) Stat(ctx context.Context, key string) (objstore.ObjectMeta, error) {
	info, err := c.mc.StatObject(ctx, c.cfg.Bucket, key, minio.StatObjectOptions{})
	if err != nil {
		return objstore.ObjectMeta{}, fmt.Errorf("s3 stat %q: %w", key, err)
	}
	return toMeta(info), nil
}

// Put uploads `data` as a single-shot PUT. Wired to kmod's
// .release/.fsync flush events so writes through the mount become
// durable in the backing bucket.
func (c *Client) Put(ctx context.Context, key string, data []byte) error {
	r := bytes.NewReader(data)
	_, err := c.mc.PutObject(ctx, c.cfg.Bucket, key, r,
		int64(len(data)), minio.PutObjectOptions{
			ContentType: "application/octet-stream",
		})
	if err != nil {
		return fmt.Errorf("s3 put %q: %w", key, err)
	}
	c.cache.Invalidate(key)
	return nil
}

func (c *Client) Delete(ctx context.Context, key string) error {
	err := c.mc.RemoveObject(ctx, c.cfg.Bucket, key, minio.RemoveObjectOptions{})
	if err != nil {
		return fmt.Errorf("s3 delete %q: %w", key, err)
	}
	c.cache.Invalidate(key)
	return nil
}

// Rename is CopyObject + Delete-of-source. S3 has no atomic rename;
// a partial failure leaves both keys, which the caller logs.
func (c *Client) Rename(ctx context.Context, oldKey, newKey string) error {
	src := minio.CopySrcOptions{Bucket: c.cfg.Bucket, Object: oldKey}
	dst := minio.CopyDestOptions{Bucket: c.cfg.Bucket, Object: newKey}
	if _, err := c.mc.CopyObject(ctx, dst, src); err != nil {
		return fmt.Errorf("s3 copy %q → %q: %w", oldKey, newKey, err)
	}
	if err := c.mc.RemoveObject(ctx, c.cfg.Bucket, oldKey,
		minio.RemoveObjectOptions{}); err != nil {
		return fmt.Errorf("s3 delete src %q after copy: %w", oldKey, err)
	}
	c.cache.InvalidateRename(oldKey, newKey)
	return nil
}

// GetRange fetches a byte slice via S3 HTTP Range GET so only the
// requested chunk crosses the wire — critical for D-01's
// partial-admit path. Local disk cache is skipped for range GETs
// (cache is keyed on whole object).
func (c *Client) GetRange(ctx context.Context, key string, offset int64, size int64) ([]byte, error) {
	opts := minio.GetObjectOptions{}
	if err := opts.SetRange(offset, offset+size-1); err != nil {
		return nil, fmt.Errorf("s3 range set: %w", err)
	}
	obj, err := c.mc.GetObject(ctx, c.cfg.Bucket, key, opts)
	if err != nil {
		return nil, fmt.Errorf("s3 range get %q [%d,%d): %w",
			key, offset, offset+size, err)
	}
	defer obj.Close()
	return readRange(obj, size, func(err error) error {
		return fmt.Errorf("s3 range get %q read: %w", key, err)
	})
}

// readRange reads a range response into a buffer sized up front: io.ReadAll
// grows (and copies) its buffer, which cost ~40% of a 4 MiB chunk's time.  A
// body shorter than `size` (range past the end) returns what arrived.
func readRange(r io.Reader, size int64, wrap func(error) error) ([]byte, error) {
	buf := make([]byte, size)
	n, err := io.ReadFull(r, buf)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return nil, wrap(err)
	}
	return buf[:n], nil
}

func (c *Client) Get(ctx context.Context, key string) ([]byte, error) {
	if buf, ok := c.cache.Read(key); ok {
		return buf, nil
	}
	obj, err := c.mc.GetObject(ctx, c.cfg.Bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("s3 get %q: %w", key, err)
	}
	defer obj.Close()
	buf, err := io.ReadAll(obj)
	if err != nil {
		return nil, fmt.Errorf("s3 get %q read: %w", key, err)
	}
	c.cache.WriteAsync(key, buf)
	return buf, nil
}

// List enumerates objects under `prefix`. Recursive (across
// CommonPrefixes) so deep hierarchies show up in demos.
func (c *Client) List(ctx context.Context, prefix string) ([]objstore.ObjectMeta, error) {
	ch := c.mc.ListObjects(ctx, c.cfg.Bucket, minio.ListObjectsOptions{
		Prefix:    prefix,
		Recursive: true,
	})
	out := make([]objstore.ObjectMeta, 0, 256)
	for obj := range ch {
		if obj.Err != nil {
			return nil, fmt.Errorf("s3 list %q: %w", prefix, obj.Err)
		}
		out = append(out, toMeta(obj))
	}
	return out, nil
}

var _ objstore.Backend = (*Client)(nil)

func toMeta(o minio.ObjectInfo) objstore.ObjectMeta {
	return objstore.ObjectMeta{
		Key:          o.Key,
		Size:         o.Size,
		ETag:         o.ETag,
		LastModified: o.LastModified,
		ContentType:  o.ContentType,
	}
}

// -- AC-51: multipart upload with server-side copy --------------------------

func isPreconditionFailed(err error) bool {
	r := minio.ToErrorResponse(err)
	return r.StatusCode == http.StatusPreconditionFailed || r.Code == "PreconditionFailed"
}

// GetRangeMatch is GetRange with If-Match: a changed object is
// ErrSourceChanged, not silently different bytes.
func (c *Client) GetRangeMatch(ctx context.Context, key string, offset, size int64, etag string) ([]byte, error) {
	opts := minio.GetObjectOptions{}
	if err := opts.SetRange(offset, offset+size-1); err != nil {
		return nil, fmt.Errorf("s3 range set: %w", err)
	}
	if err := opts.SetMatchETag(etag); err != nil {
		return nil, fmt.Errorf("s3 match etag: %w", err)
	}
	obj, err := c.mc.GetObject(ctx, c.cfg.Bucket, key, opts)
	if err != nil {
		return nil, fmt.Errorf("s3 range get %q: %w", key, err)
	}
	defer obj.Close()
	return readRange(obj, size, func(err error) error {
		if isPreconditionFailed(err) {
			return objstore.ErrSourceChanged
		}
		return fmt.Errorf("s3 range get %q read: %w", key, err)
	})
}

// PutWithCopy builds `key` from parts: uploads carry their bytes, copies
// are server-side UploadPartCopy of the current object pinned to
// ifMatchETag.  Any failure aborts the multipart upload.
func (c *Client) PutWithCopy(ctx context.Context, key, ifMatchETag string, parts []objstore.Part) (string, error) {
	core := minio.Core{Client: c.mc}
	uploadID, err := core.NewMultipartUpload(ctx, c.cfg.Bucket, key, minio.PutObjectOptions{
		ContentType: "application/octet-stream",
	})
	if err != nil {
		return "", fmt.Errorf("s3 multipart init %q: %w", key, err)
	}
	fail := func(err error) (string, error) {
		// ctx may be what failed; abort on a fresh one.
		actx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = core.AbortMultipartUpload(actx, c.cfg.Bucket, key, uploadID)
		return "", err
	}
	// Parts are independent; MinIO's copy cost grows with the bytes copied,
	// so run a few at a time.
	done := make([]minio.CompletePart, len(parts))
	errs := make([]error, len(parts))
	var next atomic.Int64
	var wg sync.WaitGroup
	pctx, cancel := context.WithCancel(ctx)
	defer cancel()
	workers := min(4, len(parts))
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1)) - 1
				if i >= len(parts) || pctx.Err() != nil {
					return
				}
				p, num := parts[i], i+1
				switch p.Kind {
				case objstore.PartCopy:
					cp, err := core.CopyObjectPart(pctx, c.cfg.Bucket, key, c.cfg.Bucket, key, uploadID,
						num, p.Off, p.Len, map[string]string{"x-amz-copy-source-if-match": ifMatchETag})
					if err != nil {
						if isPreconditionFailed(err) {
							errs[i] = objstore.ErrSourceChanged
						} else {
							errs[i] = fmt.Errorf("s3 copy part %d of %q: %w", num, key, err)
						}
					} else {
						done[i] = minio.CompletePart{PartNumber: num, ETag: cp.ETag}
					}
				default:
					if int64(len(p.Data)) != p.Len {
						errs[i] = fmt.Errorf("s3 upload part %d of %q: have %d bytes, want %d", num, key, len(p.Data), p.Len)
					} else if up, err := core.PutObjectPart(pctx, c.cfg.Bucket, key, uploadID, num,
						bytes.NewReader(p.Data), p.Len, minio.PutObjectPartOptions{}); err != nil {
						errs[i] = fmt.Errorf("s3 upload part %d of %q: %w", num, key, err)
					} else {
						done[i] = minio.CompletePart{PartNumber: num, ETag: up.ETag}
					}
				}
				if errs[i] != nil {
					cancel()
				}
			}
		}()
	}
	wg.Wait()
	// ErrSourceChanged wins over the cancellations it caused.
	var firstErr error
	for _, e := range errs {
		if errors.Is(e, objstore.ErrSourceChanged) {
			return fail(e)
		}
		if firstErr == nil && e != nil {
			firstErr = e
		}
	}
	if firstErr != nil {
		return fail(firstErr)
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	info, err := core.CompleteMultipartUpload(ctx, c.cfg.Bucket, key, uploadID, done, minio.PutObjectOptions{})
	if err != nil {
		return fail(fmt.Errorf("s3 multipart complete %q: %w", key, err))
	}
	c.cache.Invalidate(key)
	return info.ETag, nil
}

var _ objstore.MultipartCopier = (*Client)(nil)
