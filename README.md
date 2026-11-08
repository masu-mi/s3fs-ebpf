# s3fs-ebpf

Linux 6.12 LTS の eBPF 機能 (**`struct_ops`**、**KFunc**) で S3 ファイルシステムを組めるかを試す **PoC** です。(`bpf_arena` は最初の版でファイルの中身を置いていましたが、今は検証用のデモ `make test-arena` だけが使います。)FUSE を経由せず、カーネルモジュールが Linux VFS に直接ファイルシステムを登録し、キャッシュ判定やポリシーを eBPF に任せます。本番利用は想定していません。

- 実装済み: read / write path、ポリシーの hot swap (CEL + built-in eBPF + user eBPF + KFunc の 4 層)、中身を kmod の chunk と page cache に置く pcache モード、既存オブジェクトの途中書き換え、victim 選択の BPF 化 (`pick_victim`)、open で S3 の存在を確認するモード (`open_check`)、flush / rename / delete を kmod が積んだ順に S3 へ反映する処理、rename・unlink した S3 由来の名前を隠す tombstone
- 経緯と現状: [`docs/journal/history.md`](docs/journal/history.md)、残課題: [`docs/journal/backlog.md`](docs/journal/backlog.md)

## アーキテクチャ

```
  syscall (stat / open / read / write)
              │
              ▼
  ┌──────────────────────────────────────────────┐
  │ kmod: VFS (s3fs_ebpf) + KFunc                │
  │  eBPF: struct_ops policy vtable              │
  │  kmod の chunk 表 / page cache にあれば即応答 │
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

`POST /policy/reload` で走行中に policy を差し替えられます。差し替えは struct_ops の `BPF_LINK_UPDATE` 1 回 (kmod の `update()` が RCU のポインタを 1 本替える) だけで、読み手は旧か新の一式だけを見ます (in-flight の IO は古い vtable で完走)。struct_ops でない `.bpf.o` (fentry / fmod_ret) は reload も起動時の読み込みも拒否され (reload は HTTP 400)、`BPF_LINK_UPDATE` が失敗したときは古い policy が残ったままエラーを返します。

既存の大きいオブジェクトの途中への書き込みは、既定で変更に触れる chunk だけを読み、残りを multipart の `UploadPartCopy` で運びます (`--partial-modify`、既定で有効、S3 のみ。16 MiB 未満は全体を読み書き)。`--partial-modify=false` で全体 GET + PUT (AC-50) に戻せます。

**open の存在確認 (`open_check`)**: 既定では、O_CREAT なしの open は kmod の entry 表 (daemon の sync が更新する) だけで答え、S3 に聞きません。sync の後に S3 から消えたファイルも open は成功し、エラーは最初の read まで出ません。s3fs-fuse は open のたびに HEAD するので、kmod の module param `open_check=1` (`echo 1 > /sys/module/s3fs_kfunc/parameters/open_check`) にすると同じように HEAD し、404 なら `ENOENT` で失敗します。cache hit の open にも S3 の往復 (ローカル MinIO で約 420 µs) が乗るので、既定は 0 です。速度への影響は下の「性能」を参照。

詳しくは:
- 技術要素 (bpf_arena / struct_ops / KFunc / KUnit / lockdep / kCFI / 閉じた MinIO など) を入門記事と一次資料から学ぶ: [`docs/user/learning-guide.md`](docs/user/learning-guide.md)。doc 全体の地図は [`docs/INDEX.md`](docs/INDEX.md)
- 全体の通信路 (VFS / kmod / BPF / daemon / backend): [`docs/design/component-communication.md`](docs/design/component-communication.md)
- policy engine: [`docs/design/policy-engine-design.md`](docs/design/policy-engine-design.md)
- pcache モード (ファイルの中身を kmod の chunk 表と page cache に置く。唯一のモード): [`docs/design/pcache-mode.md`](docs/design/pcache-mode.md)
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

ローカル MinIO で s3fs-fuse と比べた主な結果です (pcache モード、n=5〜10 の中央値、2026-11-03)。比は「s3fs-fuse の時間 ÷ s3fs-ebpf の時間」で、1 より大きいほど s3fs-ebpf が速いことを表します。

| 場面 | s3fs-fuse | s3fs-ebpf | 比 | 比べた相手 |
|---|---:|---:|---:|---|
| open+read+close (キャッシュあり、open で S3 を確認しない既定) | 520.1 µs | 2.9 µs | 180× | fuse-cache |
| open+read+close (キャッシュあり、open でも S3 を確認: `open_check=1`) | 517 µs | 425 µs | 1.2× | fuse-cache |
| 小さいファイル 10,000 個を cat (warm) | 6.42 s | 0.16 s | 39× | fuse-cache |
| 小さいファイル 10,000 個を `ls -l` (cold) | 3.40 s | 68 ms | 50× | fuse-raw |
| durable write 100 MiB (新規) | 346 ms | 337 ms | 1.03× (差なし) | fuse-raw |
| 既存 100 MiB の途中に 4 KiB + fsync | 171.0 ms | 170.5 ms | 1.00× (`--partial-modify` が効く) | fuse-raw |
| 既存 10 MiB の途中に 4 KiB + fsync | 47.9 ms | 70.2 ms | 0.68× (負け) | fuse-raw |
| cold read 300 MiB | 314.1 ms | 367.3 ms | 0.86× (負け) | fuse-raw |

**180× の行の読み方**: s3fs-fuse は open のたびに S3 へ HEAD を 1 回投げます (キャッシュありでも、1000 回で HEAD 1001)。既定の s3fs-ebpf は投げないので、180× は「open で S3 を確認しない設計の値」です。確認を揃える (`open_check=1`) と、時間のほぼ全部が HEAD 1 往復になり、差は約 1.2× に縮みます (§18.52)。キャッシュなし同士の差も、3.2× と書いていたものは HEAD の分を含み、揃えると約 1.7× です (§18.51)。確認の頻度を下げて速くする案 (TTL、更新通知) は [`docs/journal/backlog.md`](docs/journal/backlog.md) の AC-78〜81 にあります。

open / `ls -l` / cat を何度も繰り返す小さいファイルの使い方で勝ち、大きいファイルの cold read と、`--partial-modify` の対象 (16 MiB) 未満の途中書き換えではまだ負けます。page cache のヒットで終わる warm の SQLite / ベクトル検索は fuse-cache と ±3% で差がつきません。新規ファイルの durable write は、fsync が PUT の完了前に返る不具合を直した後は 1 MiB / 10 MiB が 1.3〜1.5×、100 MiB は差がなくなりました。

**v2 再設計後の再計測 (2026-11-07、§18.54)**: 再設計の前 (`03bb636`) と後 (#57 の先端) を同じセッションで交互に測りました。getattr の fentry・ringbuf・arena の経路を外した結果、**速くなったもの**は open+read+close の p50 (キャッシュあり 3.1 → 2.9 µs)、64 スレッドの並列 read (`ebpf-pcache ÷ fuse-raw` が 0.92〜0.94 から 0.98〜1.00)、小さいファイルの warm cat (143.7 → 109.8 ms) です。書き込み (新規・途中の書き換え) と cold read の 10・100 MiB は変わりません (±10%)。**遅くなったもの**は、小さいファイル 10,000 個の **cold cat** で、6.8 秒 → 8.3 秒 (+22%) です。S3 へのリクエスト (HEAD / GET) と upcall の数は同じで、1 回の miss が約 200 µs 遅くなっています。daemon が ringbuf の event を読まなくなった副作用で、機構は確かめていません (仮想環境のアイドルからの復帰が疑わしい)。上の表の「小さいファイル 10,000 個を cat (warm)」の s3fs-ebpf は 0.16 s、再計測では 0.11 s です。cold cat の s3fs-fuse との比 (21.5 s ÷ 8.3 s = 2.6×) は、この表にはまだ載せていません。

表と測り方は [`docs/journal/benchmark-results.md`](docs/journal/benchmark-results.md) の §18.48 (6 構成 × 8 項目の測り直し) と §18.49 (page cache / kmod の chunk 表 / daemon の disk cache / S3 の層ごとのレイテンシ)、試験の概要と測っている側面は [`docs/dev-notes/bench-suite-overview.md`](docs/dev-notes/bench-suite-overview.md)、ベンチの実行方法は [`docs/user/getting-started.md`](docs/user/getting-started.md#ベンチマーク) です。キャッシュ policy ごとの hit rate は [`docs/journal/hit-rate-bench.md`](docs/journal/hit-rate-bench.md) と §18.48 (open のたびに page cache を捨てる構成では、policy なしの 27.8% に対して W-TinyLFU が 50.9%) にあります。

## 検証

```bash
make test-regression    # 実機の回帰試験 (約 25 分、30 本)
make test-parity        # s3fs-fuse と同じ操作を流して、結果 (内容か errno) を比べる差分テスト
make test-spec          # TLA+ のモデルを TLC で検証 (使い捨てコンテナ、ホストに Java は不要)
```

- 速度比は、双方が同じ仕事をしたときだけ意味があります。そのため、比を書く前に S3 への HEAD / GET と upcall の数を双方で数え (`.claude/skills/perf-bench-verify`)、振る舞いの差は `test-parity` の既知の差 ([`scripts/lib/parity-known-diff.sh`](scripts/lib/parity-known-diff.sh)) に理由つきで並べます。
- 非同期・並行の状態 (kmod / daemon / S3 の flush・rename・sync) は、実装の前に状態遷移図と TLA+ のモデル ([`specs/tla/`](specs/tla/)) で検証します。入門: [`docs/dev-notes/tlaplus-introduction.md`](docs/dev-notes/tlaplus-introduction.md)、手順: [`docs/dev-notes/design-verification.md`](docs/dev-notes/design-verification.md)。適用例は [ファイルの状態遷移](docs/design/file-lifecycle-state-machine.md) (反例 3 件) と [kmod の entry 表](docs/design/kmod-entry-table-state-machine.md) (設計案 3 つを棄却)。

## ドキュメント

トピック別の索引は [`docs/INDEX.md`](docs/INDEX.md) です。
