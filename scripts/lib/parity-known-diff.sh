# shellcheck shell=bash
# Known differences between s3fs-ebpf and s3fs-fuse found by test-parity.sh.
# op name -> reason (backlog id). An entry is a gap: fix it and delete the line.
KNOWN_DIFF[rename_open_new]="open of a renamed, never-read file fails EIO (the daemon's entry mirror still holds the old key, so the read-miss GET hits the deleted key) — AC-82"
KNOWN_DIFF[rename_open_old]="open of the old name after a rename gives EIO, fuse gives ENOENT (same stale key) — AC-82"
KNOWN_DIFF[ext_delete_listdir]="a key deleted by another client stays listed until the next sync (--sync-every=0 here); fuse lists S3 each time — AC-80"
KNOWN_DIFF[ext_create_open]="a key created by another client is invisible until the next sync (--sync-every=0 here) — AC-80"
KNOWN_DIFF[create_rename_quick]="create + close + rename before the daemon flushes: the rename event is handled first (source key missing, dropped), then the flush PUTs the OLD name, so S3 holds q1.txt while the user sees q2.txt (model counterexample, specs/tla) — AC-84"
KNOWN_DIFF[rename_then_reuse_name]="creating the old name right after a rename fails EEXIST (kmod still holds the old entry until the next sync); fuse succeeds. It also masks the model's data-loss race (rename a->b, create a, flush overwrites a, rename moves it to b) — AC-85"
