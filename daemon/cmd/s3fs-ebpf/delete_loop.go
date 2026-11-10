package main

import (
	"context"
	"fmt"
	"os"
	"time"

	fpebpf "github.com/bbsakura/s3fs-ebpf/daemon/internal/ebpf"
	"github.com/bbsakura/s3fs-ebpf/daemon/internal/objstore"
)

func handleDelete(ctx context.Context, s3c objstore.Backend, ev *deleteEvent) {
	name := string(ev.Name[:ev.NameLen])
	s3key := resolveEventS3Key(ev.Ino, ev.ParentIno, name)
	stagingInvalidate(s3key) // object staging: before and after the DELETE
	defer stagingInvalidate(s3key)
	if s3c == nil {
		fmt.Printf("[delete] drop ino=%d name=%s (no S3 backend)\n",
			ev.Ino, name)
		return
	}
	putCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := s3c.Delete(putCtx, s3key); err != nil {
		fmt.Fprintf(os.Stderr, "[delete] ino=%d s3://%s delete: %v\n",
			ev.Ino, s3key, err)
		return
	}
	fpebpf.DropRecorded(ev.Ino) // the key RecordKey recorded for it is gone
	fmt.Printf("[delete] ino=%d s3://%s deleted\n", ev.Ino, s3key)
}

func handleRename(ctx context.Context, s3c objstore.Backend, ev *renameEvent) {
	oldName := string(ev.OldName[:ev.OldNameLen])
	newName := string(ev.NewName[:ev.NewNameLen])
	oldKey := resolveEventS3Key(ev.Ino, ev.OldParentIno, oldName)
	newKey := resolveKeyPath(ev.NewParentIno, newName)
	// object staging: both keys change (the old one goes, the new one is replaced)
	stagingInvalidate(oldKey)
	stagingInvalidate(newKey)
	defer stagingInvalidate(oldKey)
	defer stagingInvalidate(newKey)
	if s3c == nil {
		fmt.Printf("[rename] drop ino=%d %s→%s (no S3 backend)\n",
			ev.Ino, oldKey, newKey)
		return
	}
	putCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := s3c.Rename(putCtx, oldKey, newKey); err != nil {
		fmt.Fprintf(os.Stderr, "[rename] ino=%d %s→%s: %v\n",
			ev.Ino, oldKey, newKey, err)
		return
	}
	// Rewire the ino → S3Key map so subsequent flushes / deletes
	// use the new key.
	registerIno(newKey, ev.Ino)
	fpebpf.RecordKey(ev.Ino, ev.NewParentIno, newName, newKey, 0, ev.Seq) // AC-86
	fmt.Printf("[rename] ino=%d s3://%s → s3://%s\n", ev.Ino, oldKey, newKey)
}
