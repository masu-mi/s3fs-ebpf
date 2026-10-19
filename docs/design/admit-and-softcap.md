# admit と softcap — 読み込みキャッシュの層と eviction の実際

2026-10-05

コード (`kmod/vfs_ops.c`, `daemon/cmd/s3fs-ebpf/admit_arena.go`, `main.go`) の実際の動きに合わせた説明。設計意図ではなく「今どう動いているか」を書き、キャッシュアルゴリズムとしての問題点と改善案を末尾にまとめる。

> **2026-10-05 更新**: §4 の P7 (S-01)、P1〜P3 (S-02, capacity mode)、P6 (S-04) は実装済み (§8)。§2〜§3 は既定の legacy mode の説明として有効。

---

## 1. 読み込みの 4 層

```
read(2)
  │
  ├─ C: page cache ........ kernel 管理。hit なら kmod には来ない
  │     (.read_iter = generic_file_read_iter)
  │
  ├─ B: admit + arena ...... kmod の admit 表 → daemon の arena (共有メモリ)
  │     (.read_folio: admit_snapshot → s3fs_folio_fill_from_arena)
  │
  ├─ A: daemon disk cache .. --cache-dir 指定時のみ (s3.Client.Get がローカルを先に見る)
  │
  └─ S3 / MinIO ............ upcall → daemon が GET
```

| 層 | 誰が中身を決めるか | 追い出し | hit のコスト |
|---|---|---|---|
| C page cache | kernel (読んだら載る) | kernel の LRU / MGLRU、メモリ圧迫時 | 数百 ns〜μs |
| B admit + arena | daemon (prewarm / upcall) + BPF policy (evict 投票) | policy の投票、sync 時の prune | μs (GUP + kmap + memcpy) |
| A disk cache | daemon | (なし。上限管理は未実装) | ローカル I/O |
| S3 | — | — | ms (HTTP) |

---

## 2. admit とは

### 2.1 データ置き場と住所録は別物

- **arena** (`bpf_arena`): daemon と BPF が共有するメモリ。S3 から取ってきたファイルの中身そのものがここに入る
- **admit エントリ**: kmod 内のハッシュ表 `s3fs_admits` の 1 行。「ino X の [offset, offset+size) は arena の uaddr にある」という住所

```c
struct s3fs_admit {          /* kmod/vfs_ops.c */
	u64 ino;     /* どのファイルか */
	u64 uaddr;   /* arena 上の位置 (daemon の user_vm アドレス) */
	u64 size;    /* 長さ */
	u64 offset;  /* ファイル内の開始位置 (大きいファイルは範囲ごとに複数行) */
};
```

「admit されている」= kmod がその範囲を arena から直接読める状態。

### 2.2 `.read_folio` の流れ

```
admit にある (hit)  → arena からコピー → on_hit policy 呼出
admit に無い (miss) → miss queue に積んで daemon を待つ (upcall)
                       daemon: S3 GET → arena に書く → admit を登録 → kmod を起こす
                       kmod  : admit を引き直して arena からコピー
```

### 2.3 admit の正本は daemon

kmod の表は写し。正本は daemon の Go スライス `admits` で、`ioctl(S3FS_IOC_ADMIT_ARENA)` で**毎回全件まるごと**送られ、kmod の `s3fs_ebpf_admit_replace` が表を全消去 → 再登録する。

全置換が起きる契機:

| 契機 | コード |
|---|---|
| 起動時 prewarm | `main.go` (initialAdmits) |
| read miss upcall 完了 | `handleReadMiss` → `replaceAdmit` |
| 定期 sync で S3 から消えたキーを除去 | `pruneAdmitsTo` |
| `POST /policy/eval` (同じ一覧を再送するだけ。ベンチ用) | `refreshAdmits` |

全置換のたびに kmod は次を行う:

1. 一覧の全 ino について `i_size` を更新し、**page cache を破棄** (`invalidate_mapping_pages`)
2. `on_admit(count)` を呼ぶ
3. count > 32 (ハッシュ 64 バケットの半分) なら `on_pressure` を呼ぶ (通知のみ)
4. **count > softcap (8) なら一覧の全件に `on_evict` を聞き、evict 票の付いたものを削除**

---

## 3. softcap とは

```c
#define S3FS_EVICT_SOFTCAP 8U        /* kmod/vfs_ops.c */

for (i = 0; i < count; i++) {        /* count = 今回送られてきた件数 */
	so_vote = s3fs_policy_ops_eval_evict(ino);
	if (count <= softcap) continue;   /* 8 件以下なら票は無視 */
	if (so_vote) { admit から削除; evict_log に記録; page cache 破棄; }
}
```

- **発動条件であって容量ではない**: 「送られてきた一覧が 8 件を超えたら、policy の evict 票を実行する」
- **8 件まで減らしたら止まる処理は無い**: 票が 1 件なら 29 件残り、28 件なら 2 件しか残らない
- **毎回全件に聞く**: 今入ったばかりの ino にも `on_evict` が来る。各 policy が「初見は猶予」(grace) を持つのはこのため
- **後始末**: 削除された ino は `evict_log` に積まれ、daemon が 2 秒ごとに回収して (`drainEvictionsLoop` → `dropAdmits`) 自分の一覧から外し、arena 領域を free-list に戻す
- **復活**: 追い出されたファイルを次に読むと miss → upcall → 再 admit → 全置換 → 件数が 8 を超えていればまた投票

| 似た数字 | 値 | 役割 |
|---|---|---|
| `S3FS_EVICT_SOFTCAP` | 8 | これを超えたら evict 票を実行 |
| pressure 閾値 | 32 | これを超えたら `on_pressure` (通知のみ) |
| ハッシュのバケット数 | 64 | 表の作り。件数上限ではない (チェーン) |

BPF policy からは `s3fs_kfunc_softcap()` / `s3fs_kfunc_admit_count()` で読める。

### 3.1 例: hit-rate ベンチ (30 ファイル)

```
prewarm: 30 件 admit → 30 > 8 なので全件に on_evict
  SIEVE: 初見は猶予 → 0 件削除 → 30 件のまま
eval 2: 同じ 30 件 → visited を落とすだけ → 30 件
eval 3: visited=0 を evict → 大量削除 (例 22 件) → 8 件前後
workload: 消えたファイルを読む → miss → 再 admit → 9 件以上 → また投票
```

W-TinyLFU は evict 票が少ない (161 回) ので 30 件近くが残ったまま走り、88.5% になる。同じ trace で容量 8 の Belady 上限は 61〜65% なので、これは容量 8 の hit 率ではない (`docs/journal/hit-rate-bench.md` Validity review)。

---

## 4. キャッシュアルゴリズムとして問題ないか

**結論: 「policy の判断を差し込める仕組み」としては動くが、「容量の決まったキャッシュ」としては成立していない。** 具体的な問題:

| # | 問題 | 影響 |
|---|---|---|
| P1 | **容量の上限が無い**。softcap は投票の発動条件で、残る件数は policy 次第 (0〜全件) | メモリ使用量が保証できない。policy 同士の hit 率比較が容量で揃わない |
| P2 | **件数ベース**。44 B のファイルも 100 MiB のファイルも 1 件 | 本当の制約は arena のバイト数。件数 8 では arena 128 MiB の使い方を制御できず、溢れると `AdmitContent` が ENOMEM |
| P3 | **「victim を選ぶ」ではなく「全件に yes/no を聞く」API** | 必要数だけ追い出す、が書けない。過剰に追い出すと再取得が連鎖する (ベンチの LRU は 200 アクセスで 9884 回 evict) |
| P4 | **判定タイミングがアクセスではなく全置換**。miss / sync / eval のたびに全件評価 | LRU や SIEVE は近似にしかならない。新規 admit が同じラウンドで即 evict されうる |
| P5 | **page cache hit が policy に見えない**。`on_hit` は `.read_folio` の admit hit でしか呼ばれない | よく読まれるファイルほど page cache で返り、policy からは「冷えている」ように見える。頻度系 (LFU / W-TinyLFU) の統計が逆向きに歪む |
| P6 | **evict の daemon 反映が最大 2 秒遅れる**。その間に別の miss で全置換が起きると、daemon は evict 済み ino を送り直す | 追い出したものが復活し、もう一度投票される |
| P7 | **全置換のたびに一覧全体の page cache を破棄する** (§2.3 の 1) | 1 ファイルの miss で、admit 済みの全ファイルの page cache が消える。hot なファイルも次の read で `.read_folio` からやり直し |

P7 は性能上いちばん効く。中身 (uaddr/size/offset) が変わっていない ino は破棄不要なので、前回と差分のある ino だけ破棄すれば消える。

---

## 5. 8 は変更できるべきか

**できるべき。ただし数字を可変にするより、意味を変えるほうが先。**

1. **容量はバイトで持つ**: `cache_capacity_bytes` (arena サイズ以下) を正にする。件数上限は補助として残すなら任意で
2. **上限を守る主体を決める**: 全置換後、まだ上限を超えていれば、policy の順位で上限以下まで削る。順位の得方は次のどちらか
   - `on_evict` の戻り値を yes/no ではなくスコアにして、kmod がスコア順に削る
   - vtable に `select_victim()` を追加する (sched_ext 型。policy が「次はこれ」を返す)
3. **設定経路**: daemon の `--cache-capacity` → ioctl で kmod へ (module param でもよいが、arena サイズを知っているのは daemon)。`s3fs_kfunc_softcap()` は定数ではなく実行時の値を返す
4. **8 の由来**: PoC のデモ値 (小さな working set で eviction を起こすため)。実運用値としての根拠は無い

可変にするだけ (module param `softcap`) なら小さな変更で済むが、P1〜P3 が残るので hit 率の公平な比較にはならない。

---

## 6. page cache と、その後ろの層の役割分担

### 6.1 page cache だけで良くないのか

page cache は最速 (hit なら kmod にも来ない) で、kernel が全体のメモリ状況を見て自動で出し入れしてくれる。単一ノードの「最近読んだものをもう一度読む」だけならこれで十分。後ろに B (admit + arena) を置く意味は、page cache ができないことにある:

| B (admit + arena) の利点 | page cache ではできない理由 |
|---|---|
| **先読み (prewarm) を daemon が非同期に入れられる** | page cache を埋めるには FS を通して read する必要がある。arena なら daemon が S3 から直接書き込める。モデルロードのように「初回から速く」が要る用途向け |
| **明示的に制御できる (policy / CEL / BPF)** | page cache の追い出しは kernel の LRU 任せで、アプリの知識 (このファイルは次も使う、このジョブ終了後は不要) を入れられない |
| **一貫性の制御点** | daemon が S3 の変更を検知したら admit を差し替え、kmod が該当 page cache を破棄する。page cache だけだと、いつ古い内容を捨てるかの手がかりが無い |
| **page cache が落ちても S3 まで行かない** | メモリ圧迫や drop_caches で page cache が消えても、再充填は arena からの μs。B が無いと S3 の ms になる |
| **policy が観測できる** | `.read_folio` は kmod を通るので on_hit が取れる。page cache hit は kmod を通らない |

### 6.2 その代わりのコスト

- **二重保持**: 同じ内容が arena と page cache の両方に載る。小さい working set なら無視できるが、大きいとメモリ効率が半分になる
- **arena は kernel のメモリ回収の対象外** (BPF map として確保され、LRU に乗らない)。だから B 側で容量を明示的に管理する必要がある → §5 の容量上限が必須になる理由
- **policy の視界が欠ける** (§4 P5)

### 6.3 役割分担の提案

| 層 | 担当 | 方針 |
|---|---|---|
| C page cache | 最短経路の hot set | kernel に任せる。B 側から壊さない (§4 P7 の差分破棄) |
| B admit + arena | **アプリが決める working set**: prewarm、容量 (バイト) 上限つきの policy 管理、一貫性 | 容量を明示的に持ち (§5)、page cache hit も policy に見せる |
| A disk cache | arena に載らない分の第二層 | 上限管理を追加 (今は無制限) |
| S3 | 正本 | — |

page cache hit を policy に見せる方法は既にある材料で作れる。`bpf/s3fs_fastpath.bpf.c` の `fentry/generic_file_read_iter` は page cache hit を含む全 read で発火している (今は read pattern 計測にだけ使用)。ここから policy の `on_hit` 相当 (例えば per-ino のアクセスカウンタ map を policy と共有) を出せば、P5 が解消する。

### 6.4 結局どこが速いのか (ベンチとの対応)

- **hot な 2 回目以降の read**: page cache 同士なので s3fs-fuse (`kernel_cache`) とほぼ同じ。差が出るのは FUSE の open/getattr upcall の有無 (`benchmark-results.md` §7: cat 2.05〜2.45×、open+read+close 245×)
- **初回 read / page cache が消えた後**: B が効く。FUSE は userspace → disk/S3、s3fs-ebpf は arena から μs
- **キャッシュ無し同士の構造比較**: `ebpf-nocache` (kmod `nocache=1`) vs fuse-raw (`scripts/bench-3way.sh`)

---

## 7. 改善項目 (優先度順)

1. **P7 差分 invalidate**: 全置換で中身が変わった ino だけ page cache を破棄する。小さく、効果が大きい
2. **容量上限 (バイト) + victim 選択**: §5。hit 率比較を成立させる前提
3. **page cache hit を policy に供給**: §6.3。頻度系 policy の前提
4. **evict 反映の遅れ (P6)**: kmod 側で「evict 済み ino」を覚えておき、daemon の再送を無視 (または ioctl の戻りで即時に返す)
5. **disk cache (A) の上限管理**

---

## 8. 実装状況 (2026-10-05)

| 項目 | 状態 | 内容 |
|---|---|---|
| S-01 (P7) | 実装済 | `admit_replace` は旧テーブルに同一の (ino, offset, uaddr, size) があり、daemon mm も同じ range の page cache を破棄しない。新しい内容は必ず新しい arena chunk (新 uaddr) に入るので、これで十分。`s3fs_ebpf_admit_invalidations_total` で観測できる |
| S-02 (P1〜P3) | 実装済 | module param `capacity_bytes` (>0 で capacity mode) と `softcap` (実行時に可変)。daemon `--cache-capacity` / `--softcap` で設定する。capacity mode では `on_evict` の戻り値をスコアとして扱う (高い順 → 0 は古い順) |
| S-04 (P6) | 実装済 | daemon は push の直前に evict log を drain する (`reconcileEvictionsLocked`)。kmod が evict するのは daemon の push 中だけなので、復活の窓が無くなる |
| 追加 | 実装済 | miss upcall で reader が待っている ino はどちらのモードでも evict しない (`pending_exists`)。以前は、自分の push で入れた ino をその場で追い出し、reader が placeholder buffer に落ちる可能性があった |
| S-03 (P5) | 実装済 | KFunc `s3fs_kfunc_policy_hit` を `fentry/generic_file_read_iter` の hit パスから呼び、page cache で返る read でも `on_hit` が dispatch される |
| AC-58 (P3、P4 の一部) | 実装済 (2026-10-17) | vtable に `on_insert` / `on_remove` / `pick_victim` を追加。これを持つ policy (SIEVE / LRU / S3-FIFO の `*_pick_ops`) は容量超過のとき「次に捨てる ino」を自分で選び、kmod は必要な分だけ evict する。持たない policy は従来の score 順 (`docs/design/ac58-pick-victim.md`) |

capacity mode でも残る点:

- **件数ではなくバイト**: bench はファイルが等サイズなので `SOFTCAP × 44 B` で slot 数を再現している
- **判定は全置換ラウンド単位 (P4)**: admit が増えるのは必ず daemon の push (= `admit_replace`) 経由なので、上限は push のたびに守られる。ただし policy は push ごとに全件を再評価するので、LRU / SIEVE などの「アクセス単位」のアルゴリズムは引き続き近似になる。AC-58 の `pick_victim` 版は順序を BPF 側で正しく持つが、evict するタイミングは push ごとのまま
- **in-flight の ino は上限計算から外せない**: 待ち reader がいる ino は evict しないので、その分は一時的に上限を超えることがある (`pr_info_ratelimited` で記録)
- ~~**実機未検証** (S-06)~~ 実機検証済み (2026-10-07、S-06)
- **pcache モード (`--arena-metadata-only`)**: 容量は fd の chunk の確保量 (`fd->cap`) で数える (AC-41、ino ごとに 1 回)。miss が in-flight の ino は丸ごとは evict せず、reader が pin していない chunk だけを free する (shed、AC-45 C3)。ロック順と不変条件は `docs/kmod-pcache-invariants.md`
