# s3fs-ebpf

Linux 6.12 LTS の eBPF 機能 (**`bpf_arena`**、**KFunc**、**struct_ops**) で S3 ファイルシステムを組めるかを試す **PoC** です。FUSE を経由せず、カーネルモジュールが Linux VFS に直接ファイルシステムを登録し、キャッシュ判定やポリシーを eBPF に任せます。本番利用は想定していません。

- 実装済み: read / write path、ポリシーの hot swap (CEL + built-in eBPF + user eBPF + KFunc の 4 層)、中身を kmod の chunk と page cache に置く pcache モード、既存オブジェクトの途中書き換え、victim 選択の BPF 化 (`pick_victim`)
- 経緯と現状: [`docs/journal/history.md`](docs/journal/history.md)、残課題: [`docs/journal/backlog.md`](docs/journal/backlog.md)

## アーキテクチャ

```
  syscall (stat / open / read / write)
              │
              ▼
  ┌──────────────────────────────────────────────┐
  │ kmod: VFS (s3fs_ebpf) + KFunc                 │
  │  eBPF: fentry + struct_ops policy vtable     │
  │  bpf_arena / page cache にあれば即応答         │
  └──────────────┬──────────────────┬────────────┘
                 │ hit              │ miss
                 ▼                  ▼
          [カーネル内で返す]   Go daemon → S3 / MinIO / GCS
                              (CEL + eBPF policy の hot swap)
```

| 層 | 役割 | 実装 |
|---|---|---|
| Layer 0 | KFunc primitives (`admit_count` / `softcap` / `time_ns` など) | `kmod/tree_traversal.c` |
| Layer 1 | CEL: prewarm 判定と Layer 2 の選択 | `policies/*.cel` |
| Layer 2 | built-in eBPF policy (LRU / SIEVE / S3-FIFO / W-TinyLFU など) | `bpf/policy_*_ops.bpf.c` |
| Layer 3 | user eBPF policy (SHA256 allowlist で gate) | `--policy-bpf=file:<path>` |

`POST /policy/reload` で走行中に policy を差し替えられます (in-flight の IO は古い vtable で完走)。

詳しくは:
- 全体の通信路 (VFS / kmod / BPF / daemon / backend): [`docs/design/component-communication.md`](docs/design/component-communication.md)
- policy engine: [`docs/design/policy-engine-design.md`](docs/design/policy-engine-design.md)
- pcache モード (`--arena-metadata-only`): [`docs/design/pcache-mode.md`](docs/design/pcache-mode.md)
- KFunc が要る理由 (verifier 拒否デモ): [`docs/journal/early-walkthroughs.md`](docs/journal/early-walkthroughs.md#verifier-拒否デモ-verifier-demo)
- ExtFUSE / RFUSE との比較: [`docs/user/related-works.md`](docs/user/related-works.md)

## 使ってみる

Linux 6.12 LTS 以上、clang 19+、Go、docker compose が要ります。前提とセットアップの詳細は [`docs/user/getting-started.md`](docs/user/getting-started.md)。

```bash
make doctor             # 足りないものと直し方を表示 (make setup-kernel / setup-toolchain で導入)
make all                # kmod + bpf + daemon
make compose-up         # MinIO 互換ストレージ + Prometheus + Grafana
make mount              # insmod + mount -t s3fs_ebpf (既定は /mnt/s3demo)
make daemon-run         # daemon をフォアグラウンドで起動 (Ctrl+C で停止)

ls /mnt/s3demo          # 別の端末で
cat /mnt/s3demo/hello.txt

make unmount            # daemon 停止 + umount + rmmod
```

- 自作 policy を書く: [`docs/user/writing-policies.md`](docs/user/writing-policies.md)
- GCS backend で動かす: [`docs/user/gcs-backend.md`](docs/user/gcs-backend.md)
- CLI のフラグと Make ターゲット一覧: [`docs/user/cli-reference.md`](docs/user/cli-reference.md)
- Meetup のデモ台本: [`docs/user/demo-scenario.md`](docs/user/demo-scenario.md)

## 性能

ローカル MinIO で s3fs-fuse (fuse-raw / fuse-cache) と比べた主な結果です (pcache モード)。

| 場面 | FUSE | s3fs-ebpf | 差 |
|---|---:|---:|---:|
| open+read+close (キャッシュあり) | 524.6 µs | 3.05 µs | 172× (fuse-cache 比) |
| 小さいファイル 10,000 個を cat (warm) | 18.3 s | 0.20 s | 93× |
| 小さいファイル 10,000 個を `ls -l` (cold) | 3.11 s | 0.27 s | 11× |
| durable write 100 MiB (新規) | 360 ms | 262 ms | 1.37× |
| cold read 300 MiB | 309.6 ms | 385.1 ms | 0.80× (負け) |

open / fsync を繰り返す小さいファイルの使い方で勝ち、大きいファイルの cold read ではまだ負けます。表と測り方は [`docs/journal/benchmark-results.md`](docs/journal/benchmark-results.md) (§18.32、§18.37)、ベンチの実行方法は [`docs/user/getting-started.md`](docs/user/getting-started.md#ベンチマーク)。キャッシュ policy ごとの hit rate は [`docs/journal/hit-rate-bench.md`](docs/journal/hit-rate-bench.md) にあります。

## ドキュメント

トピック別の索引は [`docs/INDEX.md`](docs/INDEX.md) です。
