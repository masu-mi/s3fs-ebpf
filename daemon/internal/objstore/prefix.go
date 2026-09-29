package objstore

import (
	"context"
	"strings"
)

// Prefixed wraps a Backend so a fixed key prefix maps to the mount
// root. Semantically matches `s3fs-fuse -o url=s3://bucket:/prefix/`:
// keys under <prefix> appear as top-level names in the mount, and
// callers using this wrapper see and write keys relative to <prefix>.
//
// All key-taking methods prepend the prefix; List and Stat strip it
// from returned metadata. Callers (sync_entries, flush, delete,
// rename, prewarm) stay backend-agnostic and prefix-agnostic — the
// entire mapping is confined to this file.
type Prefixed struct {
	inner  Backend
	prefix string
}

// NewPrefixed returns b unchanged when prefix is empty. Otherwise
// returns a wrapper that normalizes prefix to end in "/" so users
// can pass either "foo" or "foo/" and get directory semantics.
func NewPrefixed(b Backend, prefix string) Backend {
	if prefix == "" {
		return b
	}
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	return &Prefixed{inner: b, prefix: prefix}
}

// Prefix returns the normalized prefix ("" if the wrapper is a
// no-op). Useful for log lines that want to show what the mount root
// resolves to.
func (p *Prefixed) Prefix() string { return p.prefix }

func (p *Prefixed) Bucket() string { return p.inner.Bucket() }

func (p *Prefixed) Stat(ctx context.Context, key string) (ObjectMeta, error) {
	m, err := p.inner.Stat(ctx, p.prefix+key)
	if err != nil {
		return m, err
	}
	m.Key = strings.TrimPrefix(m.Key, p.prefix)
	return m, nil
}

func (p *Prefixed) Get(ctx context.Context, key string) ([]byte, error) {
	return p.inner.Get(ctx, p.prefix+key)
}

func (p *Prefixed) GetRange(ctx context.Context, key string, offset, size int64) ([]byte, error) {
	return p.inner.GetRange(ctx, p.prefix+key, offset, size)
}

func (p *Prefixed) Put(ctx context.Context, key string, data []byte) error {
	return p.inner.Put(ctx, p.prefix+key, data)
}

func (p *Prefixed) Delete(ctx context.Context, key string) error {
	return p.inner.Delete(ctx, p.prefix+key)
}

func (p *Prefixed) Rename(ctx context.Context, oldKey, newKey string) error {
	return p.inner.Rename(ctx, p.prefix+oldKey, p.prefix+newKey)
}

// List forwards subprefix through the wrapper prefix so callers can
// still narrow the enumeration ("list under mount-root/subdir/").
// The returned metadata carries relative keys — the "<prefix>"
// placeholder object itself (a directory marker) is dropped so it
// doesn't clutter the mount-root listing.
func (p *Prefixed) List(ctx context.Context, subprefix string) ([]ObjectMeta, error) {
	metas, err := p.inner.List(ctx, p.prefix+subprefix)
	if err != nil {
		return nil, err
	}
	out := make([]ObjectMeta, 0, len(metas))
	for _, m := range metas {
		// Backends only return keys matching the prefix we asked for,
		// so HasPrefix should always be true. Defensive skip guards
		// against a hypothetical backend that ignores the filter.
		if !strings.HasPrefix(m.Key, p.prefix) {
			continue
		}
		m.Key = strings.TrimPrefix(m.Key, p.prefix)
		if m.Key == "" {
			continue
		}
		out = append(out, m)
	}
	return out, nil
}

var _ Backend = (*Prefixed)(nil)
