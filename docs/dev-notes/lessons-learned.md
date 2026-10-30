# Engineering Lessons — s3fs-ebpf を作って踏んだ地雷と一般化された知見

Week 1〜4 (a)(b)(c)(e) を通して、週次ノート (`docs/weekN-*.md`) には残せない**横断的な**教訓をまとめる。「同じ地雷を次のプロジェクトでも避ける」ためのチェックリスト的なメモ。

個別の症状 → 原因 → 対処の一次情報は下記に既にある:

- `docs/journal/week1-build-runtime-notes.md` — ビルド / toolchain
- `docs/journal/week4a-implementation-notes.md` §10 — Path B/C 実装で踏んだ点
- `docs/journal/week4b-implementation-notes.md` §2 — sync 設計判断
- `docs/journal/week4c-implementation-notes.md` §2, §4 — write-back 設計 + follow-up
- `docs/journal/benchmark-results.md` §6.1 — bench 側の bug/盲点

この文書はそれらを **メタ化・パターン化** した資料。

> 実装の地雷ではなく、**設計・先行研究・キャッシュの使い方・計測** の知見 (2026-10-30、warm で fuse-cache に勝てなかった振り返り) は `docs/design/lessons-cache-fs-layer.md` にある。§8 の要旨にある倍率 (fuse-cache 比 14〜17× など) は、書き込みや毎回 upcall が走る経路での値で、warm の読みには当てはまらない。

---

## 0. TL;DR — 一言ずつの教訓

1. **kernel 同期プリミティブは sleep 可否で選ぶ** — dir_emit / copy_to_user 系を触るなら mutex / rwsem、非 sleep なら spinlock / rwlock / RCU
2. **`new_inode()` は inode を hash に登録しない** — ilookup 系 API を使う予定があるなら `iget_locked() + insert_inode_hash()` (unlock_new_inode で完結)
3. **buffer growth は最初から kvmalloc** — kmalloc/krealloc は ~4 MiB (`MAX_ORDER`) で `__alloc_pages_noprof` WARN。fallback を仕込むなら kvmalloc + memcpy
4. **upcall path で作った lock は upcall 相手に触らせない** — .read_folio が folio_lock を持ったまま待機中に、daemon 側から `invalidate_inode_pages2` を呼ぶと deadlock
5. **UAPI struct は「size を持たせるかどうか」を先に決める** — kmod 側で i_size を推定できないと read が短絡して cat が空になる (Week 4 (b) の再ビルド 1 発分の教訓)
6. **replace-all ioctl は semantics が simple、delta より PoC 向き** — ただし「local additions」のような後付けの視点も要件次第で足す必要がある
7. **bench は fork/exec と cache 種別を分離して数字を取る** — hyperfine wall clock は fork dominant、drop_caches=3 は inode まで飛ばして path lookup dominant
8. **FUSE 比較は公平ラインを引く** — `-o use_cache + -o kernel_cache` を入れた fuse-cache を必ず対抗馬に含める
9. **バグ検出のプライマリ計器は dmesg WARN と byte-for-byte diff** — この 2 つが通れば実装の 8 割は妥当
10. **「まず動く形」→「次にコーナー」の順で行けば 4 週間で回る PoC が成立する** — 完璧な設計を先に立てると arena kern_vm 直 deref のような早期に潰れる案で 2 週停滞していた

---

## 1. 具体的な難所トップ 8

### 1.1 arena kern_vm 直 deref が全 EFAULT (Week 4 (a))

**症状**: kmod から `bpf_arena_get_kern_vm_start()` で取得した kernel-VM アドレスを deref すると全部 EFAULT。

**原因**: 6.12 の arena kern_vm range には generic-kernel-context 用の fault handler が登録されていない。`arena_vm_fault` は user_vm VMA 用のみ。BPF JIT の `addr_space_cast` は独自経路で kern_vm を触るが、汎用 kernel access には反応しない。

**対処**: A′ を断念し、B (GUP over user_vm) にピボット。daemon の user_vm 側なら `arena_vm_fault` が発火するので `pin_user_pages_remote` で物理 page を掴んで `kmap_local_page` → `copy_to_user`。

**一般化**: **BPF 側の内部機構と generic kernel access は同じ VM range でも別レイヤ**。upstream の解説 (LWN 記事、kernel doc) が仄めかす API 表面と実際の実装は乖離することがある。1 コミットで済むと思った path が塞がっていたら次のパスを考える判断を早く。詳細 `docs/journal/week4a-implementation-notes.md §3〜§4`。

### 1.2 `new_inode()` が inode を hash に登録せず、admit の `ilookup` が dead code だった (Week 4 (a) fix)

**症状**: daemon が admit を PUT した後も `stat /mnt/s3demo/hello.txt` が古い initial size (17B) を返し続ける。`drop_caches=2` を打つと直る。

**原因**: `s3fs_ebpf_make_inode` で `new_inode(sb)` を使っていた。`new_inode` は inode を allocate + dentry 経由でアクセス可能にはするが、**inode_hashtable には登録しない**。`admit_replace` の `ilookup(sb, ino)` はハッシュにない inode を返せないので常に NULL → i_size 更新の `if (s3fs_active_sb)` ブロック丸ごと dead code。

**対処**: `iget_locked()` に置換。既存 inode があれば返す、無ければ I_NEW を立てて返すので init 完了後に `unlock_new_inode()`。エラー path は `iget_failed()` で cleanup。

**一般化**: **「inode を作る」API は 2 系統ある**:
- `new_inode()` — anonymous inode 相当。super block に紐付くが、ino ベースの lookup は不可能。他モジュールから ilookup で見つかる必要がなければこれで OK
- `iget_locked()` — inode_hashtable に登録され、ilookup で発見可能。ino が意味を持つ FS (通常の意味) はほぼ確実にこちら

「ilookup が動くこと」を仮定するコード (admit table の refresh、fsnotify 等) を書く前に、その inode がどう作られているか必ず確認する。詳細 `docs/journal/week4a-c-session-notes.md § invalidate_inode_pages2 の話`。

### 1.3 `invalidate_inode_pages2` が Path C の .read_folio と deadlock (Week 4 (a) fix、続き)

**症状**: iget_locked 修正後、Path C 初回の `cat /mnt/s3demo/lazy.txt` が 2.063 秒でタイムアウト (`S3FS_UPCALL_WAIT_JIFFIES = 2 * HZ` ちょうど) して initial content を返す。2 回目は正常。

**原因**: `admit_replace` の invalidate 経路と `.read_folio` のロック順序が競合する:
1. cat → .read_folio → folio_lock 取得 → pending_get_or_create → wait_for_completion 待機
2. daemon 側で S3 Get → addAdmit → `admit_replace` → **同 inode の invalidate_inode_pages2** → folio_lock を blocking で待つ (in-flight の folio が uptodate になるのを待つ)
3. daemon 側 admit_replace が folio_lock で block → signal_admit が発火せず → cat タイムアウト

**対処**: `invalidate_inode_pages2` (blocking folio_lock) を `invalidate_mapping_pages` (folio_trylock) に置換。in-flight folio はスキップされるが「まだ initial content で埋まる前の空 folio」なので、reader は目覚めた後に re-fetch して新 arena を叩く。

**一般化**: **daemon → kmod → daemon の 3 段 upcall path では「daemon 側が呼ぶ ioctl が、その ioctl の完了を待っている kmod 側 waiter と同じロックを取らないか」を必ず設計時に確認する**。folio_lock、mmap_sem、i_rwsem あたりが要注意。ロックの blocking 版と trylock 版があるなら upcall path では trylock を優先。詳細 `docs/journal/week4c-implementation-notes.md` (実は Week 4 c の作業ではなく 4 a の fix commit d2d4cdf で導入)。

### 1.4 rwlock は dir_emit を含めない — sleepable な reader が要る (Week 4 (b))

**症状**: `iterate_shared` を rwlock の read_lock 下で回すと、`dir_emit → filldir → copy_to_user` が page fault で sleep する可能性がある → `might_sleep` BUG。RCU も同様に sleep 禁止。

**対処**: mutex に切り替え。読み手が sleep する可能性がある FS 側 API では **mutex (sleepable)** か **rw_semaphore (sleepable)** の 2 択。

**一般化**: kernel の同期プリミティブは sleep 可否で分ける:

| primitive | reader sleep 可 | writer sleep 可 | concurrent reader | 適する用途 |
|---|---|---|---|---|
| spinlock | ✗ | ✗ | ✗ | short critical section, IRQ context |
| rwlock | ✗ | ✗ | ✓ | short read-heavy, no sleep |
| RCU | ✗ (classic) | ✓ | ✓ | pointer swap, lockless read |
| SRCU | ✓ | ✓ | ✓ | sleepable RCU |
| mutex | ✓ | ✓ | ✗ | 大半のケース、まず mutex |
| rw_semaphore | ✓ | ✓ | ✓ | read-heavy + reader sleep 有り |

「sleep する可能性のある API」= copy_to_user / copy_from_user / memory allocation / dir_emit / d_add / filesystem I/O。**とりあえず mutex** → concurrent reader が bottleneck ならば rwsem に上げる。詳細 `docs/journal/week4b-implementation-notes.md §2`。

### 1.5 SYNC_ENTRIES に size を持たせ忘れ、cat が empty で返る (Week 4 (b))

**症状**: `mc put live.txt` → 数秒後に `ls` に出るが、`stat` が size=0、`cat` が empty body。

**原因**: SYNC_ENTRIES で送っているのは `{ino, mode, name}` のみ。kmod の `make_inode` は「admit が無く initial_content も NULL (dynamic entry)」だと `i_size = fd->size = 0` にする。`generic_file_read_iter` は i_size=0 を short-circuit で return → `.read_folio` すら呼ばれない → Path C の upcall も走らない。

**対処**: SYNC_ENTRIES struct に `size` field を追加。kmod の i_size 決定順を `admit → sync.size → fd->size` に。

**一般化**: **UAPI 拡張は「必要になってから」ではなく「その shape で kmod が正しく動くか」から逆算する**。今回の場合、`s3fs_sync_entry` を設計する時点で「daemon が sync してから admit が届くまでの間、kmod は何を i_size として返すべきか」を先に問うべきだった。UAPI 変更は 1 度出したら後方互換のためのコストが乗るので、初期段階に size のような自明メタデータは詰め込む。詳細 `docs/journal/week4b-implementation-notes.md §2`。

### 1.6 `.create` の同期 semantics — POSIX 待ち vs daemon PUT を切り離す (Week 4 (c))

**症状**: `.create` は POSIX 上、成功時に新 inode + dentry を返す必要がある (open(2) が続くので)。同期的に daemon PUT を待つと `touch` が S3 の RTT ぶん (localhost MinIO で ~30ms、実 AWS S3 で 数百 ms) 遅くなる。

**対処**: 「kmod-local ino allocator (1_000_000+)」で先に inode を確保し、`s3fs_local_entries` list に追加、即成功 return。daemon 側は非同期で flush queue を drain → PUT → `registerIno(s3Key, ino)` で ino allocator に固定 → 次 sync round で dyn table に自然 promote (kmod 側 `local_prune_promoted_locked` で local から drop)。

**一般化**: **POSIX fop の即時性と backend 側の RTT を切り離すには「local speculative state + backend confirm 時に merge」の 2-phase pattern**。今回は
- local state: `s3fs_local_entries` list
- confirm 時 merge: SYNC_ENTRIES に同 name+ino が来たら local から drop

同型のパターンは FS layer だけでなく分散システム全般で有効 (Kafka の transactional producer、CQRS の event sourcing 等と発想が同じ)。詳細 `docs/journal/week4c-implementation-notes.md §2`。

### 1.7 kmalloc/krealloc の MAX_ORDER 上限で `__alloc_pages_noprof` WARN (Week 4 (c)/(e))

**症状**: `dd bs=10M count=1` (10 MiB write) で dmesg に:

```
WARNING: CPU: 5 PID: 1048474 at mm/page_alloc.c:4816 __alloc_pages_noprof+0x2d2/0x340
Call Trace:
 __kmalloc_large_node_noprof+0x1f/0xd0
 krealloc_noprof+0x56/0x130
 s3fs_file_data_reserve+0x44/0x90 [s3fs_kfunc]
```

**原因**: `krealloc` は内部で `__kmalloc_large_node` を呼び、x86_64 では `MAX_ORDER` = order 10 = 4 MiB が単一 allocation の上限。10 MiB を要求すると WARN + NULL 返し。

**対処**: `kvmalloc` + `memcpy` に手動置換。`kvmalloc` は size <= 8 KiB なら kmalloc、超えたら vmalloc に fallback してくれる。growth strategy も large size では 2 MiB round-up (vmalloc granularity) に切り替え。

```c
if (needed >= (4UL << 20))
    new_cap = round_up(needed, 2UL << 20);
else
    new_cap = round_up(needed, 4096);
newbuf = kvmalloc(new_cap, GFP_KERNEL);
if (fd->buf) { memcpy(newbuf, fd->buf, fd->size); kvfree(fd->buf); }
```

**一般化**: **カーネル空間で「userspace が任意サイズを書ける buffer」を持つなら kvmalloc/kvfree**。kmalloc は速いが 4 MiB で頭打ち。`kvrealloc` は 6.11+ で使えるので mainline ではそちらでも OK。詳細 `docs/journal/benchmark-results.md §8.2` 末尾 caveat。

### 1.8 bench の parallel-cat wrapper が bash `wait` で hang (Week 4 (e))

**症状**: `scripts/bench-scale.sh` の初期版で、`for n in ...; do cat & done; wait` パターンが numjobs=1 で 15 分以上帰ってこない。`/proc/PID/stack` を見ると `do_wait`。

**原因**: 深追いしなかったが、bash の `wait` が subshell 越しの背景プロセスを捉えきれず、child が抜けたのに wait 側が起きないケースがある (job control 相互作用)。

**対処**: fio psync (rw=read) に置換。**parallel read の scaling を測る目的は同じ、実装は fio が桁違いに堅牢**。

**一般化**: **shell の parallel primitive を bench の測定コンポーネントに組み込まない**。data plane は fio / stress-ng / iperf 等の specialized tool で回す。bash はセットアップと結果整形のみ。詳細 `docs/journal/benchmark-results.md §10.3` 末尾。

---

## 2. 横断的な設計判断のパターン

### 2.1 「まず動く」→「次にコーナーを潰す」の spike-first 開発順序

Week 4 (a) で `A′ (kern_vm 直 deref)` に固執せず 1〜2 日でピボットして B (GUP) を動かしたのが後の全速度を決めた。もし完璧設計 → 実装の順で進めていたら、arena kern_vm の fault handler 問題で 2〜3 週停滞していた可能性がある。

**適用条件**: 
- カーネル / eBPF / 分散 / 新しい API を触る PoC はほぼ全て
- 「ドキュメント通りに動く」保証がない領域

**逆に spike-first が向かない場合**:
- 上位プロトコル (認証、UAPI stable API) の設計 — 後で shape を変えるコストが大きい

### 2.2 replace-all ioctl vs delta ioctl

admit テーブル、entry テーブル、両方 replace-all を採用した。

**replace-all のメリット**:
- kmod 側 semantics が「常に snapshot に置換」で単純
- daemon が source of truth になり、race window が減る
- 「消えた entry」の扱いを daemon 側で吸収

**デメリット**:
- 大量 entry ではペイロード大 (現在 4096 entry 上限)
- 1 個追加でも全 snapshot を送る (実装は Go で `append + copy`)

**判断基準**: **sync 頻度 × 1 sync のペイロード ≤ 1 MB/s** に収まるなら replace-all で問題ない。今回は 30s 間隔 × 数十 entry で余裕。数万〜数十万 entry のスケールが視野に入ったら `S3FS_IOC_APPEND_*` を切って delta 化する。

### 2.3 upcall path の 3 段パターン

Week 4 (a) の read miss、(b) の bucket sync 通知、(c) の write flush — どれも同じ shape:

```
[trigger] kmod fop → enqueue event to shared queue
                     ↓
[transport] daemon polls /dev/s3fs_ebpf (blocking read or ioctl)
                     ↓  
[handler] daemon does the S3 work (Get / Put / List)
                     ↓
[completion] daemon issues ioctl (SIGNAL_ADMIT / SYNC_ENTRIES / addAdmit refresh)
```

**構造上の利点**:
- transport 層 (miscdev + wait_queue) が 1 つ、event 種類ごとに queue を持てば scale
- 完了通知が明示的で kmod 側の pending waiter を確実に起こせる
- daemon 側のロジックは全て Go で書ける (S3 SDK / cache / policy)

**適用時の落とし穴**:
- **completion signaling を忘れる** — .read_folio の pending は必ず signal で起こす。Week 4 (a) 初期は shutdown 時の pending をリークしていた
- **transport が backpressure しない** — 現在の flush queue は無制限 kmalloc。daemon が dead でも溜まり続ける (`s3fs_ebpf_admit_shutdown` で drain）

### 2.4 flat bucket 前提の basename 割当

`daemon/cmd/s3fs-ebpf/sync_entries.go` で `path.Base(S3Key)` を dentry name にしている。**subdir なし、basename 衝突なし** の前提で成立。

**現状の制約**:
- `a/foo.txt` と `b/foo.txt` が同時に存在すると同 name で 2 ino → iterate_shared が同 name を 2 回 emit → readdir 側の挙動未定義
- `/` を含む key は現在 daemon の filter (末尾 `/` は skip、`0` byte も skip) で「dir っぽさ」を除外しているだけ

**将来対応**:
- **dir 対応**: entry table の mode で `S_IFDIR` を持つ entry を admit、`s3fs_ebpf_make_inode` を dir 分岐、`s3fs_ebpf_dir_iops` を各 dir に付ける
- **path 保持**: kmod entry table を tree 構造に、あるいは `s3fs_sync_entry` に「parent_ino + name」を持たせる (今は root の子のみ)

Week 4 (b) 実装ノートで「Week 4+ の polish」と表現している範囲。

---

## 3. デバッグ / 検証のパターン

### 3.1 プライマリ計器

**1. `dmesg | grep -iE 'WARN|BUG|call trace'` を regression gate に**  
実装が「動いてるっぽい」でも WARN が出ていれば race か API 誤用がある。session 開始時と smoke 実行後に必ず比較。

**2. byte-for-byte roundtrip**  
```bash
diff -q /tmp/original <(cat /mnt/s3demo/foo)   # read back
diff -q /tmp/original <(docker exec mc cat local/s3fs-test/foo)  # write path
```
「読めた」「書けた」の主張には必ずこれを添える。bench-write.sh に組み込んでいる。

**3. drop_caches で cache 段の切り分け**
- `drop_caches=1`: pagecache only. read の re-populate path が動くか
- `drop_caches=2`: dentry + inode. `iget_locked` の path、`.lookup` の re-run
- `drop_caches=3`: 全部。cold-cache 測定用だが inode 再構築 dominant で FS 差が埋もれる

### 3.2 hang 原因の特定

```bash
sudo cat /proc/PID/stack   # kernel stack trace
sudo cat /proc/PID/wchan   # 何を待っているか (one word)
ps auxf | grep -B2 -A2 target-cmd  # parent/child 関係
```

`do_wait` = wait syscall (child を待っている)、`folio_wait_bit_common` = folio_lock 待ち、`msleep_interruptible` = timer wait。stack trace の一番上の関数がヒント。

### 3.3 git 復旧の最終防衛線

session 途中に「何を消した/変えた」が分からなくなったら:
```bash
git reflog | head -20        # HEAD の履歴
git log --oneline --all      # 未 checkout の branch も含む
git fsck --lost-found        # dangling commits
```

reflog は 90 日残るので、意図せず reset した commit も HEAD@{N} で拾える。

---

## 4. ベンチマーク方法論

### 4.1 3 層メトリクス (fork/exec 込み → syscall 単発 → 並列 IOPS)

- **hyperfine (wall clock)**: fork+exec+shell の 500 μs〜1 ms が dominant。「ユーザーが `cat` を叩いたときの体感」寄り
- **microbench (Go time.Now)**: fork/exec 排除、tight loop で syscall 単発 latency
- **fio filestat / psync**: 並列度 sweep、aggregate IOPS

**主張ごとに適切な計器を選ぶ**:
- 「fast path が μs で return する」→ microbench p50
- 「実運用で `cat` が速くなる」→ hyperfine
- 「並列読み書きに強い」→ fio numjobs sweep

### 4.2 FUSE 比較の公平ライン

s3fs-fuse は `-o use_cache=<dir> -o kernel_cache` で kernel page cache 直読になり hot read で ebpf に肉薄する (`docs/journal/benchmark-results.md §7`)。これを入れずに「s3fs-ebpf は fuse より 100 倍速い」と主張すると caveat が付いて回る。

**bench-3way.sh のパターンを標準化**:
1. `fuse-raw` (no cache) — 従来主張の baseline
2. `fuse-cache` (`-o use_cache + -o kernel_cache`) — 実運用の best-case  
3. `ebpf` (or `ebpf-A+C`) — 本 PoC

3 者を並べれば、`ebpf ↔ fuse-cache` の差が「FUSE アーキ由来のコスト」を示す。

### 4.3 数字の書き方の作法

- **範囲 (min…max)** も併記 — mean だけだと variance が消える (bench-3way §7.4 の cache-miss/op が良い例)
- **byte-for-byte MATCH** を bench 中に確認する — 「速いが正しくない」を検出する
- **caveat セクション** を必ず末尾に置く — bench 側の bug、測定条件の偏り、mount 設定の差

---

## 5. 現状の制約 (次に踏むであろう地雷)

将来の実装者向け:

### 5.1 大 file 対応の限界

現在の admit table shape は `1 ino → 1 uaddr + size`。100 MiB blob は arena の連続領域として admit しているが、arena 上限 (現 128 MiB) を超える or 部分 read の場合破綻する。

**対処案** (未実装):
- admit を `[(offset, uaddr, size), ...]` の range list に拡張
- `.read_folio` が folio 位置を含む range を pin
- arena は sliding window として使い eviction policy を daemon が持つ

### 5.2 fsync の non-blocking semantics

現在の `.fsync` は flush queue に enqueue するだけで daemon PUT を待たない。fsync 発行直後に電源断されると失われる。Application が「fsync → durability」を仮定していると危険。

**対処案**: pending completion + signal_admit と同じパターンで .fsync を blocking にする。write path 全体の semantics を「eager (現状) / synchronous fsync / synchronous write」の 3 mode 選択にすると FUSE `-o writeback_cache` 相当の柔軟性が出る。

### 5.3 ino allocator が daemon プロセス内 in-memory

daemon 再起動 → S3Key ↔ ino の mapping がリセット。umount + remount でも同じ。application が pid + ino で file lock を識別しているとその瞬間に壊れる。

**対処案**: `/var/lib/s3fs-ebpf/inos.json` に atomic write + lock で永続化。ino 空間の cleanup は S3 side でしか判定できないので gc の頻度も設計する。

### 5.4 admit table の gc

`.write` → flush → daemon `AdmitContent + addAdmit` を繰り返すと、arena 上の古い版はそのまま残る (kmod は「上書き」意味で admit するが arena 側の free 概念が薄い)。長時間動かすと arena が線形に増える。

**対処案**: admit 差し替え時に「置き換えられた古い arena offset」を daemon 側で free list に戻す。あるいは arena reset の periodic 実行 (現行 admit entries を全部再登録)。

### 5.5 daemon exit 時の SYNC clear

現状 daemon が SIGINT で落ちても kmod の dyn entries は残り、rmmod まで shutdown 経由の flush が発火しない。

**対処案**: daemon 側 defer で `syncEntriesIoctl(devPath, nil)` を送って bootstrap に戻す (`clearAdmits` と同じパターン)。

---

## 6. 学んで良かった具体的 API / パターン一覧

将来のカーネル系 PoC で reusable な小ワザ:

| API / パターン | 用途 | 引用 |
|---|---|---|
| `iget_locked + insert_inode_hash + unlock_new_inode` | inode を hash に載せて ilookup 可能にする | Week 4 (a) fix |
| `invalidate_mapping_pages` (vs `invalidate_inode_pages2`) | trylock 版の page cache flush、upcall path 内で安全 | Week 4 (a) fix |
| `kvmalloc + memcpy + kvfree` | vmalloc fallback ありの growable buffer | Week 4 (c)/(e) fix |
| `iterate_shared` に snapshot copy + unlock 後 dir_emit | mutex 下で sleep しない iterate | Week 4 (b) |
| `shrink_dcache_sb` | table replace 時の一括 dentry invalidate | Week 4 (b) |
| `pin_user_pages_remote` + `mmap_read_lock` + `locked` out-param | GUP over user_vm、6.12 の内部要件込み | Week 4 (a) |
| miscdev の `.read + .poll` + `LIST_HEAD + wait_queue` | BPF ringbuf に頼らない upcall stream | Week 4 (a) C |
| `refcount_t` ベースの pending completion 共有 | 同 ino の並行待ちを 1 個の pending に集約 | Week 4 (a) C |
| bpf2go `//go:generate` + `bpf/*.bpf.c` 分離 | userspace Go と BPF C を stable interface で | Week 1 |
| BTF-derived struct redefine (`bpftool btf dump ...`) | 未 export な kernel private struct を kmod から触る (最終手段) | Week 4 (a) |

---

## 7. ここに書かなかったこと

以下は既存ドキュメントで詳しく扱っているので重複を避ける:

- **6.12 の細かい API 変更** (`no_llseek` → `noop_llseek`、`bpf_map_get` の namespace 化 etc.) — `docs/journal/week4a-implementation-notes.md §2`
- **BPF verifier の pointer type check の性質** — `docs/journal/week2-implementation-notes.md` および verifier-demo
- **cilium/ebpf の addr_space_cast marshal パッチ** — `docs/misc/cilium-ebpf-addr-space-cast-marshal.patch`
- **各 bench の数字と解釈** — `docs/journal/benchmark-results.md`
- **clang 20 / kernel 6.12 の toolchain build** — `docs/journal/week1-build-runtime-notes.md`

---

## 8. Meetup / 論文向け 3 行要旨

もし Meetup で「1 分で伝える」なら:

> **1.** FUSE を経由せず、Linux VFS 直下 + bpf_arena + kernel module + eBPF fentry の 4 段構成で S3 を FS として提供できる (Path B: 事前 admit で cat 1 MiB を μs 台、Path C: on-demand upcall で新 object も透過的に read)。
>
> **2.** Week 4 で write-back / dynamic sync / touch まで動き、`ls / stat / cat / echo > / dd / touch` が S3 backend に正しく反映される。overwrite で fuse-cache 比 14〜17× 速い (100 MiB size)。
>
> **3.** 主な難所は (a) arena kern_vm の fault handler 欠如で GUP over user_vm に pivot、(b) VFS inode hash 登録忘れで admit が dead code、(c) upcall path の folio_lock deadlock — どれも「動作する仮定」と「実装の実態」の乖離を smoke で早期に潰したのが決定打だった。
