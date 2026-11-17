package gcs

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/storage"
	"github.com/bbsakura/s3fs-ebpf/daemon/internal/objstore"
)

func TestParseGenTag(t *testing.T) {
	for _, tc := range []struct {
		in string
		n  int64
		ok bool
	}{
		{"gen:1699999999", 1699999999, true},
		{"gen:0", 0, false},
		{"gen:", 0, false},
		{"\"abc\"", 0, false},
	} {
		n, ok := parseGenTag(tc.in)
		if n != tc.n || ok != tc.ok {
			t.Errorf("parseGenTag(%q) = %d,%v want %d,%v", tc.in, n, ok, tc.n, tc.ok)
		}
	}
}

func TestNewRequiresBucket(t *testing.T) {
	if _, err := New(context.Background(), Config{}); err == nil {
		t.Fatal("New with an empty bucket must fail before touching the network")
	}
}

func TestNewBadCredentialsFile(t *testing.T) {
	_, err := New(context.Background(), Config{
		Bucket:          "b",
		CredentialsFile: filepath.Join(t.TempDir(), "missing.json"),
	})
	if err == nil {
		t.Fatal("New with an unreadable credentials file must fail")
	}
}

func TestCacheTagRoundTrip(t *testing.T) {
	// the tag the cache stores must be what parseGenTag reads back
	tag := cacheTag(&storage.ObjectAttrs{Generation: 1699999999})
	if n, ok := parseGenTag(tag); !ok || n != 1699999999 {
		t.Fatalf("parseGenTag(cacheTag) = %d,%v", n, ok)
	}
}

func TestAttrsToMeta(t *testing.T) {
	up := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	got := attrsToMeta(&storage.ObjectAttrs{
		Name: "a/b", Size: 7, Generation: 42, Updated: up, ContentType: "text/plain",
	})
	want := objstore.ObjectMeta{Key: "a/b", Size: 7, ETag: "gen:42", LastModified: up, ContentType: "text/plain"}
	if got != want {
		t.Fatalf("no Etag: got %+v want %+v", got, want)
	}
	// an Etag from the server wins over the generation fallback
	if m := attrsToMeta(&storage.ObjectAttrs{Name: "k", Generation: 42, Etag: "CKi"}); m.ETag != "CKi" {
		t.Fatalf("ETag = %q, want the server's", m.ETag)
	}
}

// fakeGCS serves the two JSON-API object reads Stat and List use.
func fakeGCS(t *testing.T) *Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/storage/v1/b/bkt/o/present", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"name":"present","bucket":"bkt","size":"11","generation":"5","contentType":"text/plain"}`)
	})
	mux.HandleFunc("/storage/v1/b/bkt/o/missing", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"code":404,"message":"No such object"}}`, http.StatusNotFound)
	})
	mux.HandleFunc("/storage/v1/b/bkt/o", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("prefix"); got != "d/" {
			t.Errorf("List prefix = %q, want d/", got)
		}
		if r.URL.Query().Get("delimiter") != "" {
			t.Errorf("List must be recursive (no delimiter)")
		}
		fmt.Fprint(w, `{"items":[{"name":"d/a","size":"1","generation":"1"},{"name":"d/b","size":"2","generation":"2"}]}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	t.Setenv("STORAGE_EMULATOR_HOST", strings.TrimPrefix(srv.URL, "http://"))
	c, err := New(context.Background(), Config{Bucket: "bkt"})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestStatAgainstFake(t *testing.T) {
	c := fakeGCS(t)
	m, err := c.Stat(context.Background(), "present")
	if err != nil || m.Size != 11 || m.ETag != "gen:5" || m.ContentType != "text/plain" {
		t.Fatalf("Stat(present) = %+v, %v", m, err)
	}
	// a missing key must be distinguishable from a failed request (open_check relies on it)
	if _, err := c.Stat(context.Background(), "missing"); !errors.Is(err, objstore.ErrNotFound) {
		t.Fatalf("Stat(missing) err = %v, want ErrNotFound", err)
	}
}

func TestListAgainstFake(t *testing.T) {
	c := fakeGCS(t)
	got, err := c.List(context.Background(), "d/")
	if err != nil || len(got) != 2 || got[0].Key != "d/a" || got[1].Size != 2 {
		t.Fatalf("List = %+v, %v", got, err)
	}
}
