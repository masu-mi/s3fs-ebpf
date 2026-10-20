# 初期の動作確認手順とロードマップ (Week 1〜3)

以前 README に載せていた、スプリント初期の building block ごとの動作確認手順。当時の出力例と制約をそのまま残している (いまの通しの使い方は [`../user/getting-started.md`](../user/getting-started.md)、Meetup のデモは [`../user/demo-scenario.md`](../user/demo-scenario.md))。後半の制約メモ (「Week 3 で〜」など) は当時の予定で、以降の経緯は [`history.md`](history.md) にある。

---

## 動作確認 (`test-arena`)

Linux 6.12+ ホスト上で以下を叩くと 3 つのサブテストが順に走ります:

```bash
make test-arena     # sudo insmod → daemon test-arena → sudo rmmod
```

サブテスト内容:

| ID | 内容 | 検証する仮説 |
|---|---|---|
| A | `BPF_MAP_TYPE_ARENA` を daemon 側で作成、mmap して往復 read/write | bpf_arena (6.9 導入 / 6.12 LTS) の standalone create/mmap が動く |
| B | BPF プログラムが宣言した `BPF_MAP_TYPE_ARENA` を daemon が mmap で populate → SEC(`"syscall"`) の BPF プログラムが `__arena` deref 経由で読み出す | daemon ↔ BPF が同一 arena 上でゼロコピー共有できる |
| C | 上記 BPF プログラムが kmod 内の `s3fs_kfunc_echo(u32) → 2v+1` を呼ぶ | KFunc の登録・ロード・呼び出しパスが Verifier を通過 |

期待出力 (抜粋):

```text
== subtest A: bpf_arena create + mmap ==
[A] arena map id=42 size=4096 bytes
[A] roundtrip OK: wrote/read "hello arena"
== subtest B+C: bpf_arena populate + KFunc call ==
[B] arena mmap base=0x7ab...000 size=4096 bytes
[B] wrote 32 dummy inodes into arena
[B/C] prog ret=0 out_ino=0x1003 out_kfunc=43
PASS
```

---

## Fast Path 動作確認 (`fastpath`)

`fentry/vfs_getattr_nosec` をアタッチし、`stat` 系システムコールごとに Hit/Miss を分別してリングバッファ+per-CPU カウンタに落とします。

```bash
# 1. kmod をロード (未ロードなら)
sudo insmod kmod/s3fs_kfunc.ko

# 2. s3fs_ebpf をマウント (getattr が s_magic フィルタを通るのに必要)
sudo mkdir -p /mnt/s3demo
sudo mount -t s3fs_ebpf none /mnt/s3demo

# 3. 別 shell で fastpath を起動 (Ctrl+C で終了)
sudo ./bin/s3fs-ebpf fastpath --sample-every=1s

# 4. さらに別 shell で stat を叩く
stat /mnt/s3demo      # 初回: [miss] ino=1 op=getattr が飛ぶ
stat /tmp             # 非 s3fs → unknown_fs カウンタが伸びる

# 5. Fast Path Hit を模倣: ino=1 → arena offset=0 を index に注入
BPFTOOL=/usr/lib/linux-tools/$(uname -r | awk -F- '{print $1"-"$2}')/bpftool
INDEX_ID=$(sudo $BPFTOOL map list | awk '/s3fs_inode_index/{print $1}' | tr -d ':')
sudo $BPFTOOL map update id "$INDEX_ID" \
    key 0x01 0 0 0 0 0 0 0 \
    value 0 0 0 0
stat /mnt/s3demo      # 以降: fast_hits がインクリメントされ ringbuf は静か
```

期待出力例 (`fastpath` 側):

```text
[fastpath] attached fentry/vfs_getattr_nosec; arena base=0x7ab...000 size=4096
[fastpath] stat any path to trigger events. Ctrl+C to stop.
[counters/sample] fast_hits=0 slow_misses=0 unknown_fs=25788
[miss] ino=1 op=getattr
[miss] ino=1 op=getattr
[counters/sample] fast_hits=0 slow_misses=2 unknown_fs=56068
[counters/sample] fast_hits=3 slow_misses=11 unknown_fs=130815   ← ino=1 を index に注入後
```

**設計上の注意** — fentry フックは kstat を「返さない」（fentry は observe 専用）。実際の getattr レスポンスは kernel module の `inode_operations.getattr` から arena を読んで埋める設計で、その配線は Week 3 (kmod 側の inode 生成) 以降。今の fastpath は Hit/Miss メトリクスと Slow Path キック (ringbuf → daemon → S3 fetch) の 2 役に絞っている。

### Week 3: 実 VFS + fastpath prewarm フルスタック

kmod が 3 つのデモオブジェクト (hello.txt / world.txt / models_weights.bin) を lookup / readdir / read / write できるようになったので、`stat`・`cat`・`echo >` が通常のファイルとして通ります。daemon の fastpath は起動時に entry table の全 ino を prewarm するため、**1 発目の `stat` から Fast Path Hit** としてカウントされます。

```bash
# 1. kmod と mount
sudo insmod kmod/s3fs_kfunc.ko
sudo mkdir -p /mnt/s3demo && sudo mount -t s3fs_ebpf none /mnt/s3demo

# 2. VFS 基本動作
ls -la /mnt/s3demo                          # 3 files
cat /mnt/s3demo/hello.txt                   # "hello, s3fs-ebpf"
sudo bash -c 'echo "new content" > /mnt/s3demo/world.txt'
cat /mnt/s3demo/world.txt                   # "new content"

# 3. Fast Path 完全形 (--s3 で real S3 metadata を反映)
make compose-up
sudo ./bin/s3fs-ebpf fastpath --s3 --sample-every=1s
#   [prewarm] ino=100 name=hello.txt size=16 src=s3://hello.txt → offset=0
#   [prewarm] ino=102 name=models_weights.bin size=22 src=s3://models/weights.bin → offset=32
#   [prewarm] ino=101 name=world.txt size=12 src=s3://world.txt → offset=64

# 4. 別 shell で連続 stat → 全部 fast_hits に計上される
for f in hello.txt world.txt models_weights.bin; do stat /mnt/s3demo/$f > /dev/null; done
#   [counters/sample] fast_hits=9 slow_misses=0 unknown_fs=...
```

**現状の制約と Week 4 に持ち越すこと**:

- kmod のファイル内容は per-inode `kmalloc` プール。Week 4 で arena バッキング (KFunc 経由で arena kernel-VM base を取得) に切り替え、BPF Fast Path が「実データを直接返す」形へ
- kmod の entry table (`s3fs_entries[]`) が compile-time 静的。dynamic bucket sync (ioctl / netlink) は未実装。今の運用は「S3 バケットに `hello.txt` / `world.txt` / `models/weights.bin` を入れておく」規約で成立している
- `write()` は成功するが S3 write-back は無し (memory-only)。Week 4 の dirty page 追跡 + async upload で対応

### 統合デモ: miss → S3 fetch → arena populate → 次回 hit

`--s3` を付けると fastpath は miss ringbuf event を消費して S3 metadata を取得し、arena + `s3fs_inode_index` を warmup します。次の `stat` は Fast Path Hit として counter が動きます。

```bash
# 1. MinIO を立てて bucket にオブジェクトを入れる (前節参照)
make compose-up
docker run --rm --network=deploy_default --entrypoint /bin/sh docker.io/pgsty/silo:latest -c '
  mc alias set local http://minio:9000 minioadmin minioadmin
  echo hello | mc pipe local/s3fs-test/hello.txt
  echo world | mc pipe local/s3fs-test/world.txt'

# 2. kmod と mount
sudo insmod kmod/s3fs_kfunc.ko
sudo mkdir -p /mnt/s3demo && sudo mount -t s3fs_ebpf none /mnt/s3demo

# 3. S3 統合版 fastpath (別 shell)
sudo ./bin/s3fs-ebpf fastpath --s3 --sample-every=1s

# 4. さらに別 shell で連続 stat
for i in 1 2 3 4 5; do stat /mnt/s3demo > /dev/null; done
```

期待出力 (`fastpath` 側):

```text
[fastpath] attached fentry/vfs_getattr_nosec; arena base=0x7a...000 size=4096
[fastpath] s3 backend: bucket=s3fs-test (will warm arena on miss)
[fastpath] stat any path to trigger events. Ctrl+C to stop.
[counters/sample] fast_hits=0 slow_misses=0 unknown_fs=2005
[miss] ino=1 op=getattr
[warm] ino=1 ← s3://s3fs-test (3 objects) → arena offset=0
[miss] ino=1 op=getattr        ← ringbuf にキューされた重複 miss、IsWarmed() で S3 は再叩きしない
[miss] ino=1 op=getattr
[counters/sample] fast_hits=4 slow_misses=4 unknown_fs=8236  ← ここで cache warm 完了
[counters/sample] fast_hits=8 slow_misses=4 unknown_fs=14051 ← 以降は全て hit
```

**制限事項**:

- 現状 `ino=1` (マウントポイントの root inode) だけを S3 の bucket listing に紐付けている。他 ino が来ると `[warm] ino=N skipped (no S3 key mapping yet — Week 3)` を出す。理由は kmod がまだ per-object inode を作らないため。Week 3 で kmod の `inode_operations` を拡張したら任意 ino → S3 key の解決を daemon 側で書く
- arena は 1 ページ (128 slot) しか確保していない。溢れると `WarmCache` が `ErrCacheFull` を返す。デモ規模では問題にならない
- ringbuf は BPF 側で 256KB。ドレインが遅れて溢れるとイベントロスするが、`slow_misses` counter は fentry 側で直接インクリメントするので数値は正しい

---

## Verifier 拒否デモ (`verifier-demo`)

「なぜ KFunc が必要か」を verifier のログで示すためのデモ。純 eBPF で arena 上の linked-list を `while (n) { n = n->next; }` で辿ろうとして、verifier の 1M 命令上限 (`BPF_COMPLEXITY_LIMIT_INSNS`) に落ちる様子を見せます。

```bash
sudo ./bin/s3fs-ebpf dev verifier-demo
```

期待出力 (末尾抜粋):

```text
== verifier-demo ==============================================
Attempting to load s3fs_deep_walk_reject (pure eBPF arena walk
with an unbounded pointer chase). ...
=== verifier log ===
...
	7: (bf) r2 = addr_space_cast(r2, 0, 1)   ; R2_w=arena
	8: (79) r4 = *(u64 *)(r2 +8)             ; R2_w=arena R4_w=scalar()
	...
	12: (55) if r2 != 0x0 goto pc-6          ; back-edge
	...
	BPF program is too large. Processed 1000001 insn
	processed 1000001 insns (limit 1000000) max_states_per_insn 4 total_states 9664 peak_states 9664 mark_read 1

== KFunc counterpart ==========================================
built a 100000-node linked list in a 391-page arena (head=0x...)
s3fs_deep_walk_kfunc loaded: the verifier accepted it (the loop lives in the KFunc).
walked 100000 nodes in ..., sum=5000050000 (want 5000050000)

== outcome ====================================================
The verifier refused the pure-eBPF walk. The same walk handed to
s3fs_kfunc_arena_list_walk (kmod/tree_traversal.c, called from
bpf/kfunc_walk.bpf.c) loads and runs: ...
```

後半 (KFunc counterpart) は `make bpf` 済みの `bpf/kfunc_walk.bpf.o` と kmod のロードが前提です。どちらかが無い場合は `SKIP:` を表示して前半 (verifier 拒否) だけを見せます。

対比:

| 実装 | Verifier | 効果 |
|---|---|---|
| pure eBPF `while (n) { n = n->next; }` | ❌ `BPF program is too large. Processed 1000001 insn` | 深さ不定のツリー走査は静的解析で 1M 命令の状態爆発を起こす |
| BPF → KFunc `s3fs_kfunc_arena_list_walk` (`bpf/kfunc_walk.bpf.c` → `kmod/tree_traversal.c`) | ✅ 通過 (同じ `verifier-demo` の後半で実行) | BPF 側は KFunc 呼出 1 回の直線コード。ループは kmod の C で、安全性は `copy_from_user` (不正 `next` は -EFAULT)、停止性は `max_nodes` (循環は -ELOOP) + signal check が担保 |

デモは exit 0 で「rejected as expected」(+ KFunc 走査の結果一致) を返します。KFunc 走査の結果が合わなければ exit 1。将来のカーネル改良で万一ロードが通ってしまった場合は exit 2 (「デモは obsolete」) を返します。

---

## MinIO Slow Path 疎通 (`s3-list` / `s3-stat`)

daemon が本物の S3-compatible endpoint と会話できることを確認するスケルトンです。`fentry` から見た miss → S3 fetch → arena populate → hit の完全ループは kmod の inode ops (Week 3) を待つため今週はまだ組みませんが、S3 API 側の骨格はここまで動きます。

```bash
# 1. 観測スタック (MinIO 含む) を起動
make compose-up
#   MinIO:  http://localhost:9000  (minioadmin / minioadmin)
#   Console: http://localhost:9001
#   compose 内で bucket "s3fs-test" が自動作成される

# 2. 何かオブジェクトを put (mc / aws / SDK いずれでも可)
docker run --rm --network=deploy_default --entrypoint /bin/sh docker.io/pgsty/silo:latest -c '
  mc alias set local http://minio:9000 minioadmin minioadmin
  echo "hello, s3fs-ebpf" | mc pipe local/s3fs-test/hello.txt
  echo "AI weights"       | mc pipe local/s3fs-test/models/weights.bin'

# 3. daemon から list / stat
./bin/s3fs-ebpf s3-list
./bin/s3fs-ebpf s3-list --prefix=models/
./bin/s3fs-ebpf s3-stat hello.txt
```

期待出力:

```text
$ ./bin/s3fs-ebpf s3-list
bucket=s3fs-test prefix="" count=3
  hello.txt            17  787e25ff430270dcf04ba82bba3032c1-1  2026-09-21T01:00:05Z
  hostname             13  68d9ef9c4bdd75fab935fc116df6111b     2026-09-21T01:00:05Z
  models/weights.bin   23  139f878b2b374df6ca2a2cc2199056a1-1  2026-09-21T01:00:05Z

$ ./bin/s3fs-ebpf s3-stat hello.txt
bucket=s3fs-test key=hello.txt
  size          17
  etag          787e25ff430270dcf04ba82bba3032c1-1
  content-type  text/plain
  last-modified 2026-09-21T01:00:05Z
```

共通フラグ:

| フラグ | デフォルト | 説明 |
|---|---|---|
| `--endpoint` | `localhost:9000` | S3-compatible endpoint (`host:port`, scheme なし) |
| `--bucket` | `s3fs-test` | Bucket 名 |
| `--access-key` | `minioadmin` (`$S3FS_ACCESS_KEY`) | アクセスキー |
| `--secret-key` | `minioadmin` (`$S3FS_SECRET_KEY`) | シークレットキー |
| `--ssl` | `false` | HTTPS を使う |

AWS 本番 endpoint (`s3.us-west-2.amazonaws.com` 等) と IAM キーを指定すれば MinIO を経由せずに直接叩けます。

---

> **Week 2 の学習事項** — subtest B/C を「本物の arena deref」に上げるために踏んだ地雷:
>
> - `bpf_addr_space_cast` は `-mcpu=v3` を要求 (`__BPF_FEATURE_ADDR_SPACE_CAST`)。Ubuntu 24.04 標準の clang-18 では未サポート。Makefile が `clang-20` を優先検出します
> - verifier は arena map が `prog->aux->arena` に関連付いていないと `addr_space_cast` を拒否。単に `.maps` セクションに置くだけでは足りず、プログラム側で `LD_MAP_FD` を発行させる必要がある (`asm volatile("" :: "r"(&s3fs_arena));` で強制)
> - verifier は arena の `user_vm_start` が未設定だとロードを拒む。daemon が `Map.Memory()` で mmap してから BPF プログラムをロードする 2 段順序が必要 (`daemon/internal/ebpf/loader.go`)
> - cilium/ebpf v0.22.0 (main HEAD 含む) の `asm/instruction.go` `Marshal` が `bpf_addr_space_cast` のエンコーディングを壊すバグを踏んだ。`Instruction.Offset=1` (BPF_ADDR_SPACE_CAST マーカー) が plain Mov に対して常に 0 で上書きされてしまう。`daemon/third_party/ebpf/` にパッチ済みのソースを vendor して `go.mod` の `replace` で差し込んでいる。詳細は `daemon/third_party/ebpf/README-s3fs-patch.md`
> - `SEC("fentry/vfs_getattr")` は fires しない — 現行 kernel の `newfstatat` は `vfs_statx_path` → `vfs_getattr_nosec` を直接呼ぶため。ftrace で `set_ftrace_filter=vfs_getattr*` して確認したのち `SEC("fentry/vfs_getattr_nosec")` へ変更

---

## ロードマップ

`docs/user/motivation.md §1` の 1 ヶ月スプリント計画に沿って進める:

| Week | ゴール | ステータス |
|---|---|---|
| **0** | ビルドテンプレート（本リポジトリ） | ✅ done |
| **1** | `bpf_arena` 疎通、daemon↔BPF 共有メモリ、KFunc 呼び出し (`make test-arena`) | ✅ done |
| **2 (a)** | arena `__arena` deref パス (`bpf_addr_space_cast`) の疎通 | ✅ done (cilium/ebpf の Marshal バグを vendor patch で回避) |
| **2 (b)** | eBPF Fast Path (`fentry/vfs_getattr_nosec` → hit/miss counter + ringbuf) | ✅ done (`s3fs-ebpf fastpath`) |
| **2 (c)** | Verifier 拒否デモ | ✅ done (`s3fs-ebpf dev verifier-demo`) |
| **2 (d)** | MinIO Slow Path 骨 (minio-go / bucket list & stat) | ✅ done (`s3fs-ebpf s3-list` / `s3-stat`) |
| **3 (a)** | kmod が entry table のオブジェクトを VFS に materialise (lookup / readdir / read / write) | ✅ done (`ls` / `cat` / `echo >` が動作) |
| **3 (b)** | daemon の fastpath 起動時 prewarm (entry table の全 ino を arena に admit) | ✅ done (`--s3` で real S3 metadata を反映) |
| **3 (c)** | file 内容の arena バッキング (kmalloc → arena) | ✅ done (Path B GUP prewarm + Path C read-miss upcall、`docs/design/read-path-design.md` §3) |
| **3 (d)** / **4 (d)** | policy engine + hot reload (CEL + built-in eBPF + user eBPF + KFunc の 4 層) | ✅ done (`docs/design/policy-engine-design.md`) |
| **4 (b)** | dynamic bucket sync (basename 衝突解消、subdir 対応) | ✅ done (`docs/journal/week4b-implementation-notes.md`、`docs/design/g01-subdir-plan.md`) |
| **4 (c)** | write-back path (fsync blocking / unlink / rename / coalescer) | ✅ done (`docs/journal/week4c-implementation-notes.md`) |
| **4 (e)** | s3fs-fuse / Mountpoint との latency / throughput bench、Meetup 準備 | ✅ done (`docs/journal/benchmark-results.md`、`docs/journal/hit-rate-bench.md`) |
| **5+** | 分散 / multi-tenant / 大ファイル対応、残 backlog | pending (`docs/journal/backlog.md`) |

