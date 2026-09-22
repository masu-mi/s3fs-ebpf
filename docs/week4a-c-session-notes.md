# Week 4 (a) C 実装セッション記録

Path B (GUP prewarm) が動いた状態から Path C (read-miss upcall) を積み上げた 1 セッションの記録。何を作り、何を発見し、どう判断したかを短くまとめる。**踏んだ地雷の一次情報**は `docs/week4a-implementation-notes.md §10`、**設計比較**は `docs/read-path-design.md §3` を参照。

対応 commit: master ブランチの Week 4 (a) C サブトラック (session end 時点で未 commit、9 files modified)。

---

## 1. セッション start / end の diff

### Start (前回セッション終了時)

- Path B (GUP prewarm) 完成: prewarm 済 ino に `cat` すると arena 経由で S3 バイトを返す
- 未 admit ino に `cat` すると kmod の compile-time `initial_content` (静的テキスト) が返る
- daemon の役割: 起動時に 3 file 全部 prewarm、その後は fentry のリング読むだけ
- 実用 FS からの距離: 「daemon が起動時に知らない object は静的初期値のまま」= 実運用不可

### End (このセッション終了時)

- Path B + Path C 両方動作
- 未 admit ino (`lazy.txt`) に `cat` すると:
  1. kmod の `.read` が admit miss を検知
  2. `/dev/s3fs_ebpf` の read チャネル経由で miss event を daemon に流す
  3. daemon が S3 Get → `AdmitContent` → `addAdmit` (ioctl)
  4. kmod の waiter を `SIGNAL_ADMIT` ioctl で叩き起こす
  5. kmod は再度 admit lookup → hit → GUP + kmap で返す
- `cat` から見れば 1 発で S3 バイトが返る (透過的、EIO も EAGAIN も出さない)
- daemon 落とせば kmod initial fallback に自動切り替え、pending waiter は `-ESHUTDOWN` で解放

### コード diff の shape

```
 9 files changed, 871 insertions(+), 52 deletions(-)

 include/s3fs_ebpf_uapi.h          +35  (S3FS_IOC_SIGNAL_ADMIT + s3fs_read_miss_event)
 kmod/vfs_ops.c                    +329 (pending table + miss FIFO + upcall branch + shutdown)
 kmod/vfs_ops.h                    +14  (upcall API expose)
 kmod/s3fs_ebpf_dev.c              +48  (.read/.poll + SIGNAL_ADMIT ioctl handler)
 daemon/cmd/s3fs-ebpf/admit_arena.go +172 (readMissEvent + runReadMissLoop + handleReadMiss + admit snapshot mgr)
 daemon/cmd/s3fs-ebpf/main.go       +32  (setAdmits + goroutine start)
 daemon/internal/ebpf/entries.go    +24  (Prewarm bool + LookupByIno + lazy.txt entry)
 docs/read-path-design.md          +142 (§3 を "implemented" に、実装 API + smoke 追加)
 docs/week4a-implementation-notes.md +127 (§10 に C 実装時の地雷 6 subsection)
```

見積 (`read-path-design.md §3` の当初見積) は ~210 行、実測は ~480 行 (+ docs ~270 行)。2 倍強の要因は §3 で整理。

---

## 2. 設計判断とその理由

セッション中に迫られた「A か B か」の選択と、なぜそちらを取ったかを記録する。**後から掘り返したときに「なぜこうなってる?」を尋ねられる場所**として。

### 2.1 miss event チャネル: BPF ringbuf ではなく char dev の .read

**選択肢:**
- (a) 既存の `s3fs_events` ringbuf を再利用、kmod から BPF helper `__bpf_ringbuf_reserve/submit` を呼ぶ
- (b) `/dev/s3fs_ebpf` に `.read` / `.poll` を配線、内部 FIFO + wait queue で drain

**採用: (b)**

**理由:**
- (a) は `__bpf_ringbuf_reserve` が export されているか未確認 (namespace 依存の可能性)。仮に呼べても、arena namespace / verifier context の関係が複雑になり本題からずれる
- (b) は完全に BPF から独立、標準 Linux miscdevice API (`register_chrdev` の類) だけで完結
- daemon 側は `os.File.Read` の loop で済み、`select(2)` / `poll(2)` 対応 (Go runtime が epoll で拾ってくれる)
- 既に `/dev/s3fs_ebpf` (miscdevice) はある → 追加チャネルを張るコストゼロ

**後で B に切り替える動機があれば** ringbuf は zero-copy + lock-free で高頻度 event に強い。read-miss 頻度が上がった時点で再検討。現状は 1 event / cat 1 回なので char dev で十分。

### 2.2 admit ioctl の semantics 変更 vs. daemon-side snapshot 管理

**発見:** C の upcall handler で 1 件だけ `admitArena(dev, []admitEntry{...})` すると、既に prewarm 済の 3 件が **消える**。`s3fs_ebpf_admit_replace` が table 全置換 semantics だったため。

**選択肢:**
- (a) `S3FS_IOC_APPEND_ARENA` を新設して kmod 側で merge する
- (b) daemon 側で `admits []admitEntry` を source of truth にし、常に全 snapshot を送る

**採用: (b)**

**理由:**
- (a) は kmod の hash table を「差分適用可能」に拡張する必要がある。同 ino の重複 handling、削除 API、race window の設計が増える
- (b) は kmod の ioctl semantics を触らないので diff 小。daemon 側の 30 行 (`setAdmits/addAdmit/clearAdmits` + `sync.Mutex` + slice)
- daemon が state を持つ方が「daemon = control plane, kmod = data plane」の分業と整合する
- パフォーマンス懸念: admit 数が大きくなると全 snapshot を毎回 ioctl で送るのは無駄。ただし本 PoC の想定 (bucket 内 object 数 数百〜数千) では ioctl 1 回の cost は無視できる

**将来的に (a) が必要になる条件:** admit 数が数万を超えるか、admit の per-second rate が高い場合。その時点で kmod 側に merge API を追加。

### 2.3 pending completion: 個別作成 vs. refcount 共有

**問題:** 同じ未 admit ino に対して並行して `cat` が 2 発来たら、pending entry を 2 個作る? それとも共有?

**採用: refcount で共有**

**理由:**
- 個別だと miss event queue に同 ino が 2 件積まれ、daemon が S3 Get を 2 回打つ (無駄 + race)
- 共有すると `complete_all` で 2 waiter が同時に起きる、S3 Get は 1 回
- `refcount_t` は kernel 標準で API 単純

**実装ポイント:** `pending_get_or_create` の中で「hash lookup → 見つかったら refcount_inc、無ければ allocate して hash_add + refcount=2 (hash 用 + caller 用)」。allocate 時に spinlock 二度取り (`unlock → kmalloc(GFP_KERNEL) → relock → 再 lookup`) するので race check を再実行する。

### 2.4 timeout 時の挙動: EIO vs. kmalloc fallback

**選択肢:**
- (a) daemon が 2 秒以内に SIGNAL_ADMIT を返さなければ `-EIO` / `-ETIMEDOUT` を .read から返す
- (b) 同じ状況で kmod の kmalloc fallback (compile-time initial_content) に flow-through

**採用: (b)**

**理由:**
- 透過性優先。daemon が一時的に反応しなくても `cat` が失敗しない
- Meetup 訴求として「daemon 落ちても cat が (縮退した内容だが) 動く」の方がやわらかい
- 「daemon 死んでる時に古い bytes 返す」の consistency 懸念はあるが、PoC の範囲外
- 実運用 FS では (a) が正解 (静かにデータが差し替わる方が事故) — その時点でフラグ化する

### 2.5 lazy.txt を kmod と daemon 両方に追加した理由

demo entry `lazy.txt` (ino=103) を仕込むにあたって:

- kmod の `s3fs_entries[]` に追加 (`ls` で表示させ、initial_content として `(not fetched yet)\n` を持たせる)
- daemon の `fpebpf.Entries` にも追加、`Prewarm: false` フラグを立てる (起動時 skip、upcall handler が `LookupByIno` で S3Key 解決)

**なぜ両方に:**
- kmod にないと `ls` で見えない、`stat` で ENOENT
- daemon にないと upcall handler が ino → S3Key を解決できない

**「二重定義」への懸念:** これは Week 4 (b) dynamic bucket sync で解消予定 (daemon → kmod へ entry を push できるようにする)。C 単体では現状の shape で妥協。

---

## 3. 技術的発見 (要旨、詳細は §10)

C 実装中に見つかった、上流 doc に書いてない or 分かりにくい kernel API 挙動。

| # | API / 動作 | 実態 | doc の記述 | 対処 |
|---|---|---|---|---|
| 3.1 | `pin_user_pages_remote(..., locked=NULL)` | mmap_read_lock を **caller が持っていることを前提** に assert する | "handles locking internally" と書いてある | `mmap_read_lock` を明示取得 + `locked` out-param で lifetime tracking |
| 3.2 | `admit_arena` ioctl の semantics | table 全置換 (差分 API 無し) | UAPI header の comment に明記あり | daemon 側で snapshot 管理、全量 push |
| 3.3 | `complete_all` vs `complete` | 前者は待機中の**全 waiter** を起こす、後者は 1 waiter だけ | doc に書いてある | 共有 completion なので `complete_all` |
| 3.4 | BPF ringbuf を kmod から書く | `__bpf_ringbuf_reserve` は BPF helper で、kmod からの直接呼び出しは未サポート (と推定) | doc に明示なし | 使わない (char dev .read で代替) |

これらは Linux kernel bpf/arena 系のドキュメント品質が「上流 dev には自明だが outsider には見つけにくい」層に属する。同種のプロジェクトで同じ地雷を踏まないように記録。

---

## 4. 数字

### コード規模

| 分類 | 見積 (`read-path-design.md §3` 当初) | 実測 |
|---|---|---|
| UAPI | +15 | +35 |
| kmod side | +105 (pending + ioctl + ringbuf) | +391 (pending + FIFO + ioctl + shutdown) |
| daemon side | +60 (consumer + signalAdmit) | +204 (reader + handler + snapshot mgr) |
| その他 (Entry.Prewarm 等) | 0 | +32 |
| docs | 0 | +269 |
| **合計 (docs 除く)** | **~180** | **~662** |

差の主因: (a) BPF ringbuf 依存を捨てた分 kmod の queue 管理コードが増、(b) admit の replace-all を daemon 側で吸収した分 daemon が増、(c) initial 見積が実装細部を過小評価。

### 実測 timing (cat 1 回の end-to-end)

| ケース | 経路 | 実測 time |
|---|---|---|
| prewarm 済 hello.txt (2 回目以降) | B: admit hit → GUP | ~0.001s |
| unadmitted lazy.txt (初回) | C: miss → daemon Get → admit → retry → GUP | ~30ms |
| unadmitted lazy.txt (2 回目) | B: admit hit → GUP | ~0.001s |

C 初回の 30ms のほぼ全部は S3 (MinIO on localhost) の RTT。upcall + admit + signal の kmod/daemon 往復オーバーヘッド自体は timing に見えない (< 1ms 想定)。

### dmesg (WARN ゼロ)

lockdep fix 後、C を含む一連の `cat` sequence で dmesg に WARN / BUG / call trace が出ないことを確認。B 単体時代と同じ品質を維持。

---

## 5. 残った follow-up

このセッションでは扱わなかったが、C 実装から派生して見えた課題:

1. **`s3fs_active_sb` 経由の `i_size_write` が admit タイミングと合わない**
   - `ls -la` を先に打つと inode cache が kmod initial size (17/23/13/18) で埋まる
   - その後 admit されても cached inode の i_size は残る (ilookup で更新するはずだが動作を要確認)
   - `wc -c` が kmod initial size を報告する現象があった (cat の内容は正しい)
   - **対処案**: admit 時に inode を `mark_inode_dirty` + `invalidate_inode_pages2` で page cache も flush
   - 影響範囲: `stat` の size 表示のみ。read 内容は arena admit の size に clamp されているので safe

2. **既存の `warmFromS3` (fentry stat miss handler) が古いまま**
   - daemon の `main.go` に「no S3 key mapping yet — Week 3」というログ出しコードが残る
   - C の `LookupByIno` を使えば ino → S3Key を解決できるので、`warmFromS3` も同じロジックで置き換え可能
   - 影響: log noise のみ。動作には影響なし

3. **admit の snapshot re-push が atomic でない**
   - `addAdmit` → mutex 内で slice append → snapshot copy → mutex release → ioctl
   - ioctl 中に別 goroutine が addAdmit すると、後者が古い snapshot に 1 件追加した状態で ioctl を打つ
   - 現状は upcall goroutine が単一で走るのでレースしないが、複数並行時の race window あり
   - **対処案**: ioctl 中も mutex を保持する (single-writer 制約を作る)

4. **daemon の miss reader が SIGINT で clean に終わらないケース**
   - `f.Close()` は defer + goroutine で ctx.Done を待って発火する
   - `os.File.Read` が blocking 中に fd close されると Go は EPIPE / EBADF を Read から返すはずだが、シナリオによっては hangup する可能性
   - PoC 上は問題観測されず。長時間稼働 test で挙動確認したい

5. **pending の shutdown race**
   - `s3fs_ebpf_upcall_shutdown` は `-ESHUTDOWN` で `complete_all` するが、`hash_del` と complete のタイミングで waiter が起きる前に entry が消える可能性
   - 現在は `hash_del` → `pending_put` の 2 step で hash 削除、waiter は各自 `refcount_dec` で最後の ref を落とす
   - refcount 設計はテスト範囲では問題ないが、極限並行下では検証必要

---

## 6. Meetup narrative への貢献

C 実装で「実用 FS っぽさ」が demo できるようになった。

Before (B のみ):
> daemon が起動時に S3 metadata を bpf_arena に注ぐ → 以後 stat/read が zero-copy で返る

After (B + C):
> daemon が prewarm した object は zero-copy で return、prewarm されていない object は最初の cat で自動的に daemon が S3 fetch → arena に admit → 透過的に返す。daemon 落とせば kmod の内蔵 fallback。3 段構成の Fast / Slow / Failsafe が揃っている。

「eBPF + BPF arena で FUSE を置き換える」主張の第一形が揃った。残るは write-back (Week 4 (c)) と benchmark (Week 4 (e))。
