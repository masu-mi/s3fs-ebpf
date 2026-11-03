<!-- Generated from docs/user/cli-reference.md.tmpl by `make cli-reference`. Do not edit cli-reference.md directly. -->
# CLI / Make リファレンス

`s3fs-ebpf --help` と `make help` の出力。`make cli-reference` で daemon をビルドしてから再生成する。CLI や Make ターゲットを変えたら、再生成してから一緒にコミットする (差分をレビューで見るため)。使い方の流れは [`getting-started.md`](getting-started.md) を見る。

## CLI リファレンス

`s3fs-ebpf --help` の出力:

```text
Usage: s3fs-ebpf <command> [options]

Commands:
  mount        Mount an S3 bucket via the s3fs_ebpf VFS
  unmount      Unmount a previously mounted target
  fastpath     Run the fastpath daemon: attach fentry/vfs_getattr, stream
               ringbuf miss events, sync S3, serve arena content
  stats        Print fast/slow path counters
  policy       Hot-reload the cache policy blob
  hash-policy  Print SHA256 for a user policy .bpf.o (for allowlist entries).
               See --policy-allowlist and policies/allowlist.example.txt.
  s3-list      List objects in a bucket. Backend: --backend s3|gcs.
  s3-stat      Stat a single object.     Backend: --backend s3|gcs.
  dev          Development / probe / demo commands.
               Run "s3fs-ebpf dev" for the full list.

Run "s3fs-ebpf <command> --help" for command-specific flags.
```

サブコマンドの詳細フラグは `s3fs-ebpf <command> --help` で確認できます。例:

```text
Usage of mount:
  -bucket string
    	S3 bucket name
  -cache-dir string
    	Local NVMe tier cache dir (default "/var/cache/s3fs")
  -endpoint string
    	S3-compatible endpoint (default "http://localhost:9000")
  -mount-point string
    	Local mount point (default "/mnt/s3data")
  -policy string
    	Optional Lua/WASM policy path
```

---

## Make ターゲット一覧

`make help` の出力:

```text

Usage:
  make <target>

Targets:
  Help:
    help                          Show this help
  Build:
    all                           Build all components (kmod + bpf + daemon)
    build                         Alias of 'all'
    kmod                          Build kernel module -> kmod/s3fs_kfunc.ko
    kmod-clean                    Clean kernel module build artefacts
    bpf                           Build eBPF program -> bpf/s3fs_fastpath.bpf.o
    bpf-clean                     Clean eBPF build artefacts
    vmlinux                       Regenerate bpf/vmlinux.h from running kernel BTF
    vmlinux-clean                 Drop bpf/vmlinux.h
    daemon                        Build Go user process -> bin/s3fs-ebpf
    daemon-gen                    Run bpf2go codegen then build daemon (needs network)
    daemon-clean                  Clean Go build cache and bin/s3fs-ebpf
  Observability stack (docker compose):
    compose-up                    Start MinIO + Prometheus + Grafana + node_exporter
    compose-down                  Stop the observability stack
    compose-logs                  Tail compose logs
  Runtime lifecycle:
    mount                         insmod kmod + mount s3fs_ebpf on $(MOUNT_POINT)
    unmount                       umount + rmmod (also stops any running daemon)
    daemon-run                    Run daemon in foreground (needs mount + compose-up)
    daemon-stop                   Send SIGINT to any running daemon
  Benchmarks:
    bench                         Latency bench (hyperfine + microbench + fio) vs s3fs-fuse
    bench-write                   Write-back throughput bench (dd 1M/10M/100M, ebpf vs fuse)
    bench-scale                   Parallel scaling bench (numjobs=1..64, stat + read)
    bench-3way                    ebpf vs fuse-raw vs fuse-cache 3-way comparison
    bench-hitrate                 Hit-rate bench across Layer 2 policies (Zipf workload, capacity mode)
    bench-hitrate-multi           Multi-seed / Zipf-α sweep hit-rate bench (set ALPHAS= for sweep)
    bench-hitrate-mixed           D-01 mixed large+small file hit-rate bench (Y-05 acceptance)
    setup-bench-env               Install bench dependencies (hyperfine, fio, s3fs, /etc/passwd-s3fs)
  Host bootstrap:
    doctor                        Check kernel / headers / compilers / tools and print fixes
    doctor-build                  Same as doctor, limited to what 'make all' needs
    toolchain                     Print the resolved CLANG / KMOD_CC / KMOD_PAHOLE / BPFTOOL / KDIR
    setup-kernel-lockdep          Build + install a 6.12 kernel with CONFIG_PROVE_LOCKING (docs/dev-notes/lockdep-kernel.md)
    test-lockdep                  Run the regression suite on a lockdep kernel, fail on lock-order reports (AC-42)
    setup-kernel                  Install mainline Linux 6.12 LTS kernel (Ubuntu; reboot after)
    setup-toolchain               Install everything (clang, bpftool, headers, gcc/pahole, go, bench)
    setup-kbuild                  Re-run kernel-specific fixes (headers, gcc, pahole, vmlinux) after a kernel change
    lint-scripts                  shellcheck scripts/ and s3fs-ebpf.sh
  Documentation:
    cli-reference                 Regenerate docs/user/cli-reference.md (CLI / make help)
  Week 1 verification:
    test-arena                    insmod + run bpf_arena/KFunc round-trip probe + rmmod
  Unit tests (no daemon, no root):
    test-kunit                    KUnit suites for kmod in UML (KSRC=<linux source tree>)
    test-kunit-cov                KUnit in UML with clang coverage -> coverage/kmod.lcov (KSRC=...)
    test-daemon                   go test for the daemon
    test-daemon-cov               go test with coverage -> coverage/daemon.out
  Regression tests (T1–T5, AC-01, R1/S1/T3 coverage):
    check-invariants              Static checks on kmod/vfs_ops.c (+ kmod/vfs/) and the bench seeds (no root)
    test-policy-conformance       Live pick_victim policies vs their simulators, per access (issue #22)
    test-sync-keeps-cache         An unchanged bucket sync must not drop the cache (issue #22)
    test-boot-kernel              scripts/boot-kernel.sh picks the right grub entry (fixture + dry-run, no root)
    test-splice-hits              sendfile / splice reads are counted as hits (hit_accum, issue #22)
    test-regression               Run all regression tests
    test-write-readback           T1: write → read content match (new file, 4 MiB+, truncate-extend, flush+drop_caches)
    test-large-file-read          T2/Q3: 300 MiB pcache read, sha256 must match
    test-evict-readback           Q1/S8: evict then re-read — size and content unchanged
    test-populate-boundary        P2/Q3: POPULATE ioctl boundary values — live ioctl calls (size=0, overflow, >64 MiB, valid)
    test-pcache-multiread         AC-01: pcache split-chunk multi-chunk read sha256 match
    test-pcache-evict-race        R1/S1/T3: evict-race concurrent stress (default DURATION=120s)
    test-pcache-evict-race-short  R1/S1/T3: evict-race stress (DURATION=30s, for regression suite)
    test-pcache-capacity-tail     AC-41: capacity_bytes bounds fd->buf memory (tail-chunk reads)
    test-pcache-prewarm-none      AC-43: reads work in pcache mode when prewarm admits nothing
    test-pcache-prewarm           AC-67: --pcache-prewarm makes the first read of a selected file a page-cache hit
    test-prefetch-next            AC-68: prefetch_next reads the next sibling files ahead after a miss
    test-arena-partial-readback   AC-48: arena chunk admits keep i_size (random-first + parallel reads, sha256)
    test-pcache-capacity-noeval   AC-45: capacity_bytes bounds fd->buf without any policy eval (full reads, 4 x 9 MiB)
    test-pcache-capacity-seq      AC-45: a file larger than capacity_bytes read front to back stays within capacity (40 MiB vs 12 MiB)
    test-policy-reload            issue #35: reload = one BPF_LINK_UPDATE; reads never fail; non-struct_ops refused (400)
    test-fsync-large-new-file     issue #35: fsync of a new 100 MiB file returns only after the PUT landed; no late 0-byte PUT empties it
    test-policy-data-members      struct_ops data members (flags / name) + validate: accepted, refused (EINVAL), carried by a hot swap
    test-pcache-drop              pcache_drop=1: open drops only the page cache; chunk table hit needs no upcall
    test-write-existing-nopm      AC-50: same as test-write-existing with --partial-modify=false (whole-object GET + PUT path)
    test-write-existing           AC-50: write/truncate/append on an unread existing object keeps the rest (pcache + arena)
    test-write-partial            AC-51: partial_modify end to end (multipart + server-side copy, 412 fallback)
    test-write-existing-capacity  AC-50: write/truncate on an existing object bigger than capacity, under read pressure (shed/evict race)
    test-fsync-new-file           a new file written + fsync'ed is not truncated by the create placeholder flush
    test-pattern-aware-admit      R-01/AC-47: the fentry's access pattern still drives full-file promotion (seq, random, parallel, off)
    test-pick-victim-stale        AC-64: an ino that left the admit set does not stay in the policy's own list (pick_victim -> on_remove)
    test-wtinylfu-harness         AC-66: policy_wtinylfu_pick_ops through the verifier, checked against the Go model (no kmod / mount)
  Cleanup:
    clean                         Clean all build artefacts
```
