// Arena-mode prewarm: populate arena + inode_index from the kmod entry table.

package main

import (
	"context"
	"fmt"
	"os"
	"time"

	fpebpf "github.com/bbsakura/s3fs-ebpf/daemon/internal/ebpf"

	"github.com/bbsakura/s3fs-ebpf/daemon/internal/metrics"
	"github.com/bbsakura/s3fs-ebpf/daemon/internal/objstore"
	"github.com/bbsakura/s3fs-ebpf/daemon/internal/policy"
	"golang.org/x/sync/errgroup"
)

// prewarmWorkers is the concurrency limit for parallel S3 fetches during
// prewarm. 8 matches typical S3 client concurrency limits and avoids
// overwhelming small MinIO test instances (V-03).
const prewarmWorkers = 8

// prewarm populates the arena + inode_index from the kmod entry table
// so the first stat on each demo object hits fast_hits, and when S3
// is available copies each object's body into the arena and returns
// the (ino, arena_off, size) triples the caller ioctls into kmod so
// .read serves those bytes without a userspace bounce.
//
// Without S3 we still warm the stat cache but skip content admission —
// kmod's kmalloc scratch buffer covers `cat` at reduced fidelity.
//
// V-03: entries are prewarmed in parallel (up to prewarmWorkers) to
// overlap S3 round-trips. AdmitContent is serialized inside fpebpf so
// arena offset assignment is still safe under concurrency.
func prewarm(ctx context.Context, progs *fpebpf.Programs, s3c objstore.Backend, pol *policy.Engine) []admitEntry {
	timedCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	live := fpebpf.Snapshot()

	type result struct {
		ad admitEntry
		ok bool
	}
	results := make([]result, len(live))

	g, gCtx := errgroup.WithContext(timedCtx)
	g.SetLimit(prewarmWorkers)
	for i, entry := range live {
		i, entry := i, entry
		g.Go(func() error {
			ad, ok := prewarmEntry(gCtx, progs, s3c, pol, entry)
			results[i] = result{ad, ok}
			return nil
		})
	}
	_ = g.Wait()

	admits := make([]admitEntry, 0, len(live))
	for _, r := range results {
		if r.ok {
			admits = append(admits, r.ad)
		}
	}
	return admits
}

// prewarmEntry runs the per-entry prewarm work. Split into two
// distinct steps so F-03 can guarantee metadata warm even when
// content admit is skipped:
//
//  1. **Metadata warm** (always): S3 Stat → progs.WarmCache. Populates
//     the BPF inode_index so the fentry fast_hits stub recognizes
//     the ino on the first getattr. Runs regardless of policy — a
//     policy that suppresses content admit still wants stat to hit
//     Path A rather than Path C.
//
//  2. **Content admit** (policy-gated): S3 Get → progs.AdmitContent.
//     Only fires when `wantPrewarm` (Entry.Prewarm ∨ CEL override) is
//     true. Skip returns (zero, false) — the ino stays warm for stat,
//     the first cat takes Path C.
//
// Returns (entry, true) only when content admission completed. The
// warm-only case is (zero, false) — same signal shape as before F-03,
// so existing callers keep working; the meaningful visible change is
// that fentry recognizes policy-skipped inos immediately.
func prewarmEntry(ctx context.Context, progs *fpebpf.Programs, s3c objstore.Backend,
	pol *policy.Engine, entry fpebpf.Entry) (admitEntry, bool) {
	// G-01: synthetic directory entries have no arena content and
	// no S3 backing key. WarmCache would try to stat "", which S3
	// answers with a nonsense error. Short-circuit here.
	if entry.Mode&fileTypeMask == 0o040000 {
		return admitEntry{}, false
	}
	// Entry.Prewarm is the static bootstrap default; the policy
	// engine, when loaded, overrides per-entry (P-01 spike). Both
	// paths default to prewarm=true if the policy is broken /
	// missing, so a misconfigured file doesn't strand the mount.
	wantPrewarm := entry.Prewarm
	if pol != nil {
		wantPrewarm = pol.EvalPrewarm(policy.EntryInput{
			Ino: entry.Ino, Name: entry.Name, S3Key: entry.S3Key,
			Size: entry.Fallback, Mode: entry.Mode,
		})
	}
	// AC-44: --arena-metadata-only keeps object bodies out of the arena;
	// content reaches fd->buf on the first read via upcall + POPULATE_PAGES.
	// Staging the body here would make it an arena admit that consumes
	// capacity_bytes (and bypass the pcache path) for every listed object.
	// POPULATE_PAGES cannot be used instead: the inode does not exist yet.
	pcacheSkip := wantPrewarm && arenaMetadataOnlyEnabled
	if pcacheSkip {
		wantPrewarm = false
	}

	// Step 1 — metadata warm (unconditional). Prefer S3-authoritative
	// size/mtime when reachable; fall back to the bootstrap entry
	// otherwise. Both branches populate the BPF inode_index so the
	// fentry hook resolves getattr on Path A.
	stub := fpebpf.InodeStub{
		Ino:     entry.Ino,
		Size:    entry.Fallback,
		MtimeNs: uint64(time.Now().UnixNano()),
		Mode:    entry.Mode,
	}
	source := "fallback"
	var body []byte
	if s3c != nil {
		meta, err := s3c.Stat(ctx, entry.S3Key)
		if err != nil {
			fmt.Printf("[prewarm] ino=%d s3://%s stat: %v (using fallback)\n",
				entry.Ino, entry.S3Key, err)
		} else {
			stub.Size = uint64(meta.Size)
			stub.MtimeNs = uint64(meta.LastModified.UnixNano())
			source = fmt.Sprintf("s3://%s", entry.S3Key)
			if wantPrewarm {
				body, err = s3c.Get(ctx, entry.S3Key)
				if err != nil {
					fmt.Fprintf(os.Stderr, "[prewarm] ino=%d get: %v (skip admit)\n",
						entry.Ino, err)
					body = nil
				}
			}
		}
	}
	off, err := progs.WarmCache(stub)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[prewarm] ino=%d warm: %v\n", entry.Ino, err)
		return admitEntry{}, false
	}
	fmt.Printf("[prewarm] ino=%d name=%s size=%d src=%s → stub=%d\n",
		entry.Ino, entry.Name, stub.Size, source, off)

	// Step 2 — content admit (policy-gated).
	if !wantPrewarm {
		reason := "policy=false"
		if pcacheSkip {
			reason = "arena-metadata-only"
			if pcachePrewarmEnabled {
				// AC-67: read through the mount once the upcall loop runs.
				queuePcachePrewarm(entry.Ino, stub.Size)
				reason = "arena-metadata-only, queued for pcache prewarm"
			}
		}
		fmt.Printf("[prewarm] ino=%d name=%s content-skip (%s, stat=warm, cat=PathC)\n",
			entry.Ino, entry.Name, reason)
		return admitEntry{}, false
	}
	if body == nil {
		return admitEntry{}, false
	}
	cOff, err := progs.AdmitContent(body)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[prewarm] ino=%d content: %v (skip admit)\n",
			entry.Ino, err)
		return admitEntry{}, false
	}
	if hi := cOff + uint64(len(body)); hi > metrics.Global.ArenaUsedBytes.Load() {
		metrics.Global.ArenaUsedBytes.Store(hi)
	}
	fmt.Printf("[prewarm] ino=%d body=%d bytes → content=%d\n",
		entry.Ino, len(body), cOff)
	return admitEntry{
		Ino:   entry.Ino,
		Uaddr: progs.Arena().BaseAddr() + cOff,
		Size:  uint64(len(body)),
	}, true
}

// warmFromS3 resolves a miss event through the S3 client and populates
// the arena + inode index so the next matching stat hits Fast Path.
//
// Week 4 (a)+ per-object inos are handled by admit_arena / handleReadMiss.
// This function now only covers the root ino (=1) case where the "content"
// is the bucket-wide listing (a summary stub). Non-root inos are silently
// no-op'd — the dyn entry table + admit path owns them.
func warmFromS3(ctx context.Context, progs *fpebpf.Programs, c objstore.Backend, ev fpebpf.MissEvent) {
	if ev.Ino != 1 {
		// Handled by handleReadMiss for per-object read misses; the
		// fentry hook fires for stat too but the sync loop already
		// pushed the entry, so nothing to do here.
		return
	}
	// Drain-race: BPF can fire many misses before we admit the first;
	// they all queue on the ringbuf. Skip the S3 round-trip when the
	// ino is already cached — the fentry hook will start hitting
	// fast_hits as soon as its dentry-cache-warm getattrs finish.
	if progs.IsWarmed(ev.Ino) {
		return
	}
	timedCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	objs, err := c.List(timedCtx, "")
	if err != nil {
		fmt.Fprintf(os.Stderr, "[warm] ino=1 list bucket: %v\n", err)
		return
	}
	// The root dir stub summarises "how many objects live under this
	// mount point" and when the listing was last refreshed. It's a
	// pragmatic placeholder for Week 3 when per-object inodes exist.
	stub := fpebpf.InodeStub{
		Ino:     ev.Ino,
		Size:    uint64(len(objs)),
		MtimeNs: uint64(time.Now().UnixNano()),
		Mode:    0o40755, // S_IFDIR | 0755
	}
	off, err := progs.WarmCache(stub)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[warm] ino=%d populate: %v\n", ev.Ino, err)
		return
	}
	fmt.Printf("[warm] ino=%d ← s3://%s (%d objects) → arena offset=%d\n",
		ev.Ino, c.Bucket(), len(objs), off)
}
