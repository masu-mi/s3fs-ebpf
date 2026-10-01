# GCS backend

The daemon speaks two object-store dialects behind a single interface
(`daemon/internal/objstore.Backend`):

- `s3` — MinIO / AWS S3 / any S3-compatible endpoint via `minio-go/v7`
  (default; unchanged from earlier weeks).
- `gcs` — native Google Cloud Storage via `cloud.google.com/go/storage`.

Every daemon path that talks to the store (`prewarm`, read-miss upcall,
flush coalescer, delete-drain, rename-drain, entries sync) goes through
the interface, so switching backends is a flag flip. Fast Path
(kmod + eBPF + arena) is unchanged.

## Selecting the backend

```
# S3-compatible (default) — behaves exactly as before
s3fs-ebpf fastpath --s3 --endpoint minio:9000 --bucket demo \
    --access-key … --secret-key … [--ssl]

# GCS
s3fs-ebpf fastpath --s3 --backend gcs --bucket my-gcs-bucket \
    [--gcs-credentials /path/to/sa.json] [--cache-dir /var/cache/s3fs]
```

`--s3` toggles "wire the object backend on" — the flag name is legacy
and now covers both dialects. `--backend gcs` is what actually picks
GCS.

## Auth

The GCS client uses **Application Default Credentials** by default:

1. `--gcs-credentials <path>` or `GOOGLE_APPLICATION_CREDENTIALS` env
   (service-account JSON) — explicit key file.
2. `gcloud auth application-default login` — the developer's ADC file
   under `~/.config/gcloud/`.
3. GCE / GKE metadata server, or Workload Identity, on Google
   infrastructure.

No HMAC keys, no S3-interop endpoint. If you want to point the S3
backend at GCS via its S3-interop URL instead, use `--backend s3
--endpoint storage.googleapis.com --ssl` with HMAC-derived keys —
supported but deprecated by Google.

## Mount root ⇄ key prefix

`--key-prefix` (or `S3FS_KEY_PREFIX`) maps the mount root to a subtree
of the bucket. Semantics match `s3fs-fuse -o url=s3://bucket:/prefix/`
and `mount-s3 bucket/prefix mnt`: only keys under the prefix are
visible, and the prefix is stripped from what the user sees.

```
# Bucket contains: reports/2026/q1.pdf, reports/2026/q2.pdf, misc/scratch
s3fs-ebpf fastpath --s3 --bucket demo --key-prefix reports/2026/
# → mount shows q1.pdf, q2.pdf; misc/scratch is invisible.
# Writes go to the same subtree: `echo … > /mnt/q3.pdf` PUTs
#   reports/2026/q3.pdf.
```

Trailing `/` is added automatically if omitted. Empty (default) means
the whole bucket is the mount root. Works with both `--backend s3` and
`--backend gcs`; the wrapper sits above the backend so all ops
(Stat/Get/GetRange/Put/Delete/Rename/List) get the same mapping.

## Subcommand coverage

`s3-list` and `s3-stat` accept `--backend gcs` too (name kept for
back-compat). Same output shape.

```
s3fs-ebpf s3-list --backend gcs --bucket my-gcs-bucket
s3fs-ebpf s3-stat --backend gcs --bucket my-gcs-bucket path/to/obj
```

## Feature parity vs S3

| Op                       | S3 backend                | GCS backend                  |
|--------------------------|---------------------------|------------------------------|
| Stat                     | HEAD                      | Object.Attrs                 |
| Get                      | GetObject                 | Object.NewReader             |
| GetRange                 | GetObject + Range         | Object.NewRangeReader        |
| Put                      | PutObject                 | Object.NewWriter (resumable) |
| Delete                   | DeleteObject              | Object.Delete                |
| Rename                   | CopyObject + Delete       | Copier + Delete              |
| List                     | ListObjectsV2, recursive  | Objects iterator, recursive  |
| Local disk staging cache | ✅ (whole-object; sha256) | ✅ (same shape)              |
| Atomic rename            | ❌ (partial-fail 両残り)   | ❌ (same)                    |
| ETag surface             | S3 ETag                   | Etag if present, else `gen:<n>` |

Delete-of-missing is idempotent on both backends (GCS's
`ErrObjectNotExist` is swallowed).

## What is not wired yet

- **Multi-part / streaming Put.** Both backends currently
  buffer-then-single-shot. Fine for the arena chunk sizes we use
  (≤ 4 MiB in D-01 partial-admit); large-object write paths need a
  streaming pass before either backend can carry them.
- **Conditional writes.** No `If-Generation-Match` (GCS) or
  `If-Match` (S3). A racing writer wins last-write-wins.
- **Region / project pinning for GCS.** The daemon relies on ADC
  scope; if the bound credential can see multiple projects, the
  bucket lookup uses the `Bucket` name as-is (GCS names are global,
  so this Just Works — but there is no `--project` guard).
