package syncplan

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	fpebpf "github.com/bbsakura/s3fs-ebpf/daemon/internal/ebpf"
	"github.com/bbsakura/s3fs-ebpf/daemon/internal/inostate"
	"github.com/bbsakura/s3fs-ebpf/daemon/internal/kmodio"
	"github.com/bbsakura/s3fs-ebpf/daemon/internal/objstore"
)

// lister is a Backend that only lists; any other call panics on the nil
// embedded interface, which would mean BuildEntries strayed from List.
type lister struct {
	objstore.Backend
	objs []objstore.ObjectMeta
	err  error
}

func (l lister) List(context.Context, string) ([]objstore.ObjectMeta, error) {
	return l.objs, l.err
}

func objs(keys ...string) lister {
	var l lister
	for i, k := range keys {
		l.objs = append(l.objs, objstore.ObjectMeta{Key: k, Size: int64(100 + i)})
	}
	return l
}

func TestBuildEntriesNestedKeys(t *testing.T) {
	alloc := inostate.New()
	es, err := BuildEntries(context.Background(), objs("top.txt", "a/b/c.txt", "a/d.txt"), alloc)
	if err != nil {
		t.Fatal(err)
	}
	// 3 files + synthetic dirs a and a/b.
	if len(es) != 5 {
		t.Fatalf("got %d entries, want 5: %+v", len(es), es)
	}
	var dirA, dirB, c, d, top fpebpf.Entry
	for _, e := range es {
		switch {
		case e.Name == "a" && e.Mode&FileTypeMask == 0o040000:
			dirA = e
		case e.Name == "b":
			dirB = e
		case e.Name == "c.txt":
			c = e
		case e.Name == "d.txt":
			d = e
		case e.Name == "top.txt":
			top = e
		}
	}
	if dirA.ParentIno != RootIno || top.ParentIno != RootIno {
		t.Errorf("top-level entries should hang off the root: a=%d top=%d", dirA.ParentIno, top.ParentIno)
	}
	if dirB.ParentIno != dirA.Ino || c.ParentIno != dirB.Ino || d.ParentIno != dirA.Ino {
		t.Errorf("parents wrong: b->%d (want %d), c->%d (want %d), d->%d (want %d)",
			dirB.ParentIno, dirA.Ino, c.ParentIno, dirB.Ino, d.ParentIno, dirA.Ino)
	}
	if dirA.S3Key != "" || dirA.Prewarm || dirA.Mode != 0o040755 {
		t.Errorf("a synthetic dir should have no key, no prewarm and mode 040755: %+v", dirA)
	}
	if c.S3Key != "a/b/c.txt" || !c.Prewarm || c.Mode != 0o100644 || c.Fallback != 101 {
		t.Errorf("file entry wrong: %+v", c)
	}
	for i := 1; i < len(es); i++ {
		if es[i-1].Ino >= es[i].Ino {
			t.Errorf("entries not sorted by ino: %d then %d", es[i-1].Ino, es[i].Ino)
		}
	}
}

// F-02: the same basename under different prefixes are distinct (parent, name)
// pairs.
func TestBuildEntriesSameBasenameInTwoDirs(t *testing.T) {
	es, err := BuildEntries(context.Background(), objs("x/a.txt", "y/a.txt"), inostate.New())
	if err != nil {
		t.Fatal(err)
	}
	parents := map[uint64]bool{}
	for _, e := range es {
		if e.Name == "a.txt" {
			parents[e.ParentIno] = true
		}
	}
	if len(parents) != 2 {
		t.Errorf("a.txt entries share a parent: %+v", es)
	}
}

func TestBuildEntriesSkipsBadKeys(t *testing.T) {
	long := strings.Repeat("n", kmodio.SyncNameMax+1)
	es, err := BuildEntries(context.Background(),
		objs("", "dir/", "ok.txt", long, "d/"+long+"/f.txt", "has//double.txt"), inostate.New())
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range es {
		if e.Mode&FileTypeMask != 0o040000 {
			names = append(names, e.S3Key)
		}
	}
	// "" and the trailing-slash marker are dropped; a too-long leaf or
	// directory component is skipped; repeated separators are absorbed.
	want := []string{"ok.txt", "has//double.txt"}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("files = %v, want %v", names, want)
	}
}

func TestBuildEntriesInosAreStable(t *testing.T) {
	alloc := inostate.New()
	l := objs("a/x.txt", "b.txt")
	first, _ := BuildEntries(context.Background(), l, alloc)
	second, _ := BuildEntries(context.Background(), l, alloc)
	if !reflect.DeepEqual(first, second) {
		t.Errorf("a second sync with the same allocator changed the entries:\n%+v\n%+v", first, second)
	}
}

func TestBuildEntriesListError(t *testing.T) {
	boom := errors.New("boom")
	if _, err := BuildEntries(context.Background(), lister{err: boom}, inostate.New()); !errors.Is(err, boom) {
		t.Errorf("err = %v, want the List error", err)
	}
}

func TestSplitPath(t *testing.T) {
	for in, want := range map[string][]string{
		"a/b/c":  {"a", "b", "c"},
		"/a//b/": {"a", "b"},
		"single": {"single"},
		"":       {},
		"///":    {},
	} {
		if got := SplitPath(in); !reflect.DeepEqual(got, want) {
			t.Errorf("SplitPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestToPayload(t *testing.T) {
	long := strings.Repeat("n", kmodio.SyncNameMax+1)
	got := ToPayload([]fpebpf.Entry{
		{Ino: 10, ParentIno: 1, Name: "f", Mode: 0o644, Fallback: 7}, // type bits blank -> regular
		{Ino: 11, ParentIno: 1, Name: "d", Mode: 0o040755},           // directory kept
		{Ino: 12, Name: ""},   // empty name skipped
		{Ino: 13, Name: long}, // too long skipped
		{Ino: 14, ParentIno: 11, Name: strings.Repeat("n", kmodio.SyncNameMax)}, // longest allowed
	})
	if len(got) != 3 {
		t.Fatalf("got %d payload entries, want 3", len(got))
	}
	if got[0].Mode != 0o100644 || got[0].Size != 7 || got[0].NameLen != 1 || got[0].Name[0] != 'f' {
		t.Errorf("file payload wrong: %+v", got[0])
	}
	if got[1].Mode != 0o040755 {
		t.Errorf("directory mode changed: %#o", got[1].Mode)
	}
	if got[2].NameLen != kmodio.SyncNameMax || got[2].ParentIno != 11 {
		t.Errorf("longest-name payload wrong: len=%d parent=%d", got[2].NameLen, got[2].ParentIno)
	}
}
