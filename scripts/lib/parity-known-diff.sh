# shellcheck shell=bash
# Known differences between s3fs-ebpf and s3fs-fuse found by test-parity.sh.
# op name -> reason (backlog id). An entry is a gap: fix it and delete the line.
KNOWN_DIFF[ext_delete_listdir]="a key deleted by another client stays listed until the next sync (--sync-every=0 here); fuse lists S3 each time — AC-80"
KNOWN_DIFF[ext_create_open]="a key created by another client is invisible until the next sync (--sync-every=0 here) — AC-80"
