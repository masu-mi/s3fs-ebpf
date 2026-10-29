package main

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/bbsakura/s3fs-ebpf/daemon/internal/objstore"
	"golang.org/x/sys/unix"
)

// opRecorder records Delete / Rename calls; it is the whole Backend as far as
// these handlers are concerned.
type opRecorder struct {
	objstore.Backend
	deleted []string
	renamed [][2]string
	err     error
}

func (o *opRecorder) Delete(_ context.Context, key string) error {
	o.deleted = append(o.deleted, key)
	return o.err
}

func (o *opRecorder) Rename(_ context.Context, oldKey, newKey string) error {
	o.renamed = append(o.renamed, [2]string{oldKey, newKey})
	return o.err
}

func delEv(ino, parent uint64, name string) *deleteEvent {
	ev := &deleteEvent{Ino: ino, ParentIno: parent, NameLen: uint32(len(name))}
	copy(ev.Name[:], name)
	return ev
}

func renEv(ino, oldParent, newParent uint64, oldName, newName string) *renameEvent {
	ev := &renameEvent{Ino: ino, OldParentIno: oldParent, NewParentIno: newParent,
		OldNameLen: uint32(len(oldName)), NewNameLen: uint32(len(newName))}
	copy(ev.OldName[:], oldName)
	copy(ev.NewName[:], newName)
	return ev
}

func TestHandleDeleteUsesTheParentPath(t *testing.T) {
	dir := assignIno("dir:del-test")
	o := &opRecorder{}
	handleDelete(context.Background(), o, delEv(7001, dir, "gone.txt"))
	handleDelete(context.Background(), o, delEv(7002, kmodRootIno, "top.txt"))
	if want := []string{"del-test/gone.txt", "top.txt"}; !reflect.DeepEqual(o.deleted, want) {
		t.Errorf("deleted %v, want %v", o.deleted, want)
	}
}

func TestHandleDeleteWithoutBackendOrOnError(t *testing.T) {
	handleDelete(context.Background(), nil, delEv(7003, kmodRootIno, "x")) // must not panic
	o := &opRecorder{err: errors.New("s3 down")}
	handleDelete(context.Background(), o, delEv(7004, kmodRootIno, "x")) // logged, not fatal
	if len(o.deleted) != 1 {
		t.Errorf("Delete called %d times, want 1", len(o.deleted))
	}
}

// After a rename the ino must resolve to the new key, so a later flush or
// delete uses it.
func TestHandleRenameRewiresTheInoKey(t *testing.T) {
	from := assignIno("dir:ren-from")
	to := assignIno("dir:ren-to")
	o := &opRecorder{}
	handleRename(context.Background(), o, renEv(7101, from, to, "a.txt", "b.txt"))
	if want := [][2]string{{"ren-from/a.txt", "ren-to/b.txt"}}; !reflect.DeepEqual(o.renamed, want) {
		t.Fatalf("renamed %v, want %v", o.renamed, want)
	}
	if got := resolveKeyPath(to, "b.txt"); got != "ren-to/b.txt" {
		t.Errorf("resolveKeyPath = %q", got)
	}
	if got := assignIno("ren-to/b.txt"); got != 7101 {
		t.Errorf("the new key maps to ino %d, want 7101", got)
	}
}

func TestHandleRenameErrorKeepsTheOldMapping(t *testing.T) {
	o := &opRecorder{err: errors.New("copy failed")}
	handleRename(context.Background(), o, renEv(987654, kmodRootIno, kmodRootIno, "old.txt", "new-failed.txt"))
	if got := assignIno("new-failed.txt"); got == 987654 {
		t.Error("a failed rename still registered the new key")
	}
	handleRename(context.Background(), nil, renEv(7103, kmodRootIno, kmodRootIno, "a", "b")) // no backend: no panic
}

func TestDrainDeletesUntilEAGAIN(t *testing.T) {
	queue := []*deleteEvent{delEv(7201, kmodRootIno, "d1"), delEv(7202, kmodRootIno, "d2")}
	pop := func(out *deleteEvent) error {
		if len(queue) == 0 {
			return unix.EAGAIN
		}
		*out, queue = *queue[0], queue[1:]
		return nil
	}
	o := &opRecorder{}
	drainDeletesWith(context.Background(), pop, o)
	if want := []string{"d1", "d2"}; !reflect.DeepEqual(o.deleted, want) {
		t.Errorf("deleted %v, want %v", o.deleted, want)
	}
	// A pop error other than EAGAIN stops the drain.
	o = &opRecorder{}
	drainDeletesWith(context.Background(), func(*deleteEvent) error { return errors.New("ioctl broke") }, o)
	if len(o.deleted) != 0 {
		t.Error("deleted after a pop error")
	}
}

func TestDrainRenamesUntilEAGAIN(t *testing.T) {
	queue := []*renameEvent{renEv(7301, kmodRootIno, kmodRootIno, "r1", "r1n"), renEv(7302, kmodRootIno, kmodRootIno, "r2", "r2n")}
	pop := func(out *renameEvent) error {
		if len(queue) == 0 {
			return unix.EAGAIN
		}
		*out, queue = *queue[0], queue[1:]
		return nil
	}
	o := &opRecorder{}
	drainRenamesWith(context.Background(), pop, o)
	if want := [][2]string{{"r1", "r1n"}, {"r2", "r2n"}}; !reflect.DeepEqual(o.renamed, want) {
		t.Errorf("renamed %v, want %v", o.renamed, want)
	}
}
