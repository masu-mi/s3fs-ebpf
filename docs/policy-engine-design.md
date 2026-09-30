# Policy Engine 設計基盤 — language 選定に入る前の rationale + taxonomy

Week 4 (d) の Lua/WASM policy engine 実装に入る前の設計基盤。**なぜ外出しか / 何が policyable か / どのアルゴリズムが表現できるべきか** を先に固めることで、language 選定 (Lua vs WASM vs CEL vs DSL) を **要件ドリブン** にする。**「なんとなく便利なツール」で止まらないため**の下地。

対象読者: Week 4 (d) 実装セッションを回す本人、および 6 か月後に spec を読み返す将来の自分。

---

## 0. 目的と non-goals

**目的**  
- **rationale**: なぜ policy を app や FS 実装から切り離すのか、5 軸で説明可能にする
- **taxonomy**: FS lifecycle の意思決定点を漏れなく列挙、時間予算を明示
- **表現力**: 古典 (LRU/LFU/ARC) 〜 現代 (TinyLFU/S3-FIFO/SIEVE) 〜 学習系 (Learned prefetch) の代表アルゴリズムが engine の shape で表現可能なことを事前検証
- 上の 3 つを埋めた状態で language を選ぶ (§6 の未決定質問リストで管理)

**non-goals**  
- **Lua / WASM の採用** — PoC scope では eBPF + DSL (CEL) + Go native で 100% cover できる方針に決着 (§4.5, §4.6, §5.4)。将来 task D (独自 origin adapter × 非 Go チーム × hot reload 必須) が現実化した時のみ再評価
- benchmark (policy 適用前後の hit-rate)
- CLI (`s3fs-ebpf policy load`) の詳細
- 独自 DSL の設計 (CEL を借用する前提)
- multi-mount / per-tenant policy (Week 5+)
- **kernel 内 policy 評価 の禁止は撤回** — 元々「常に daemon 側」と書いていたが、user eBPF plug-in は kernel 内 (fentry attach) で走らせる形が可能かつ望ましい (§4.5)。super-hot event の kernel side 評価は積極的に採る

---

## 0.5. Meetup 3 分要旨

> 1. **policy engine は "app の外に出したほうが正しい判断がある" ケースを狙う** — 単一アプリの単一 workload なら app に埋め込めば良い。**hot reload / 横断的政策 / 役割分業 / 規制** の 4 条件が 2 つ以上重なる時に外出しが正当化される。
>
> 2. **FS lifecycle の意思決定点は 12 個ある** — 各点に「時間予算」があり (super-hot / hot / warm / cold)、この時間予算が language の下限を決める。**super-hot event (per-read) は language 呼び出しが不可能** — eBPF map の built-in policy に留める。
>
> 3. **プログラマビリティも eBPF に統一する** — 政策を Lua や WASM に載せず、engine 出荷の built-in eBPF program (LRU/W-TinyLFU/S3-FIFO/SIEVE) を DSL (CEL) で選択 + パラメタライズ、novel algorithm は user が独自 eBPF program (`.bpf.o`) を書いて load する。**「eBPF FS + eBPF policy」で runtime を 1 個も追加せず**、verifier が safety を静的保証。§4.5 参照。

---

## 1. なぜ外出しか (rationale)

「app に書けばよいのでは?」に鋭く答える節。政策外出しの justification を 5 軸で立て、各軸に「これがなければ policy engine は不要」の**逆命題**を添える。**逆命題が成立する場合は素直に app 側または FS 側 hardcode で書くべき**。

### 1.1 5 軸の justification

1. **境界の分離 (concern separation)**  
   FS 本体 (kmod + eBPF + daemon) は general-purpose を目指す。特定 workload の知識を FS 実装に焼くのは責務違反。「モデル weights は永続キャッシュ、ログは短命」のような workload 知識は、`daemon/internal/ebpf/entries.go` の Entry struct に `Prewarm bool` を増やして解決する類のものではない。  
   **逆命題**: 1 workload しか mount しないなら FS 側に hardcode で OK。

2. **hot reload**  
   daemon 再起動 (= admit table 全 flush + prewarm やり直し) なしで policy を差し替える。運用チームが夜間に TTL を絞る、事件対応で prefetch を止めるなどの応答速度が変わる。  
   **逆命題**: 日次 deploy 前提なら policy を Go code に埋めて再ビルドで OK。

3. **横断的な政策適用 (governance)**  
   1 mount を複数アプリで共有するとき、単一アプリの視点では最適でも全体では不整合が起きる (アプリ A の prefetch がアプリ B の hot データを evict するなど)。統一 policy が必要。  
   **逆命題**: mount 1 個につきアプリ 1 個なら、そのアプリの内部 cache 層で書けばよい。

4. **役割分業 (organizational)**  
   アプリ開発者 (アクセスパターンを知る) と SRE / infra チーム (storage コスト、失敗モード、キャッシュ予算、regulation) は情報が非対称。app に埋め込むと「SRE が調律したいがコード権限がない」ケースが起きる。policy 層を切ると SRE がそこだけ触れる。  
   **逆命題**: 同一チームなら app に埋め込めば早い。

5. **規制 / コンプライアンス (enforcement)**  
   「PII を edge の arena に prefetch しない」「特定 tenant のデータは専用 arena 領域」など、**app 側の善意に依存できない**制約を FS 層で強制する必要がある。GDPR / HIPAA / PCI-DSS などが典型。  
   **逆命題**: 規制対象データを扱わないなら不要。

### 1.2 scenario × 軸のクロス表

Plan で選んだ 4 domain × 5 軸で外出しの効果を評価:

| 軸 | AI/ML weights | ログ/observability | AWS multi-tenant / PII | 一般 (小規模) |
|---|---|---|---|---|
| 1. 境界分離 | ○ FS は generic のまま | ○ log 特化不可 | ○ tenant 別に差別化 | △ 効果薄 |
| 2. hot reload | △ weights は不変 | ◎ TTL は運用で頻繁変更 | ◎ 事件対応で即時変更 | △ 頻度低 |
| 3. 横断 | ○ 複数 model が共存 | ○ 複数 app が同 mount | ◎ 必須 | ✗ 単独 mount |
| 4. 役割分業 | ◎ ML / SRE 分離明快 | ○ Ops チームが調律 | ◎ Security / Dev 分離 | △ 曖昧 |
| 5. 規制 | △ (社内資産) | ○ 保管期間 | ◎ 本命 (GDPR / PII) | ✗ |

**読み方**: ◎ が 2 個以上ある scenario は policy engine の外出しが強く正当化される。△ ばかりの「一般 (小規模)」では policy engine を作るコストが benefit を上回る (= 素直に FS 側 hardcode か、`Entry.Prewarm bool` の類で十分)。

**Meetup narrative の主軸**: マルチテナント PII (◎ が 3 個) と log/observability (◎ が 2 個) を 2 つの pillar demo に置き、AI/ML は「prewarm on / static policy でも十分だが hot reload が刺さる場面もある」の poster child にする。

### 1.3 外出しの逆コスト (これを明示しないと過大評価になる)

policy engine の導入は only benefit ではない:

- **latency budget**: policy 呼び出しが 10〜100 μs のオーダー。§2 の hot event (per-stat / per-open) の予算を消費する。**super-hot event (per-read) に policy hook を入れると μs 差では済まない** (Nginx OpenResty の Lua per-request でも hot loop に入れると測定可能な差が出る)
- **fault isolation**: policy が panic / infinite loop / OOM した場合、daemon プロセスが道連れになる (kmod まで落ちるわけではないが read-miss upcall が止まる → cat が timeout)。fallback 経路の設計が要る
- **test 複雑度**: FS 単体 test + policy 単体 test + FS × policy 組合せ test の 3 系統。特に **policy の hot reload semantics** (差替え瞬間の in-flight event) が難しい
- **operational surface**: policy が観測点になる (どの policy がどう判断したかのメトリクス)。ここが薄いと debug 不能

これらを **各 scenario の ◎ 数と比較して bespoke に判断する**。

---

## 2. 意思決定点の taxonomy (operation catalog)

FS lifecycle の中で「policy に判断を委ねうる箇所」を漏れなく列挙。各点に {**event**, **input**, **output shape**, **時間予算**, **実装位置**} を対応させる。

### 2.1 12 決定点の表

| # | event | input | output | 予算 | 実装位置 (現状) |
|---|---|---|---|---|---|
| 1 | `on_startup(snapshot)` | bucket 全 entry、S3 meta | prewarm 対象 key set + 優先度 | **cold** | `main.go:418 prewarm()` |
| 2 | `on_sync_tick(added, removed)` | 前回 sync 以降の差分 | 次 tick までの興味範囲、次周期 | **cold** (30s) | `sync_entries.go:202 runSyncLoop` |
| 3 | `on_stat(ino, meta_cache_age)` | ino、cache 内 meta、age | { serve_cached / revalidate / upcall } | **hot** | `.getattr` (現 fentry のみ) |
| 4 | `on_read_miss(ino, off, len, meta)` | ino、S3Key、size、直近 access | { admit_full / admit_range / refuse / defer } | **warm** | `admit_arena.go:186 handleReadMiss` |
| 5 | `on_admit(ino, size, arena_used)` | 対象 size、現使用量 | { admit / evict_first(victims[]) / refuse } | **warm** | `admit_replace` 前段 |
| 6 | `on_pressure(used, cap, threshold)` | 使用量、閾値 | victim 候補 N 個 (順序付き) | **warm** | 未実装 |
| 7 | `on_write(ino, size, dirty_bytes)` | 累積 dirty、write pattern hint | { flush_now / defer(N ms) / coalesce } | **hot** | `.write` inline |
| 8 | `on_release(ino, dirty)` | ino、file size、dirty flag | { flush shape: batch / priority / ignore } | **warm** | `kmod/vfs_ops.c:1163 s3fs_ebpf_file_release` |
| 9 | `on_fsync(ino)` | ino、pending | { nowait / ack_enqueue / durable_block } | **warm** | `kmod/vfs_ops.c:1169 s3fs_ebpf_file_fsync` |
| 10 | `on_periodic(interval)` | 統計、ヒストグラム | prefetch queue、arena gc 指示 | **cold** | 未実装 (別 ticker) |
| 11 | `on_hit(ino)` | ino、slot 情報 | { touch / promote / no-op } | **super-hot** | `.read_folio` fast path (現 count のみ) |
| 12 | `on_evict(ino, reason)` | ino、evict 理由 | { cleanup 追加処理 / staging 保留 } | **warm** | 未実装 |

**参照**:
- Explore が catalog した 8 決定点 + docs から抽出した 4 点 = 12 点
- 現状「未実装」は 3 点 (on_pressure、on_periodic、on_evict) — policy engine 実装と同時に FS 側にも hook を切る必要がある

### 2.2 時間予算 4 分類 と language 選定への影響

各 event を予算グループに分ける。**この分類が language 選定の第 1 制約になる**。

| 予算 | 目標 latency / 呼出 | 発火頻度の目安 | 該当 event | language 候補 |
|---|---|---|---|---|
| **super-hot** | < 1 μs | per-read (数 M/s) | on_hit (#11) | native C / eBPF map の built-in policy のみ、Lua も入らない |
| **hot** | 10〜100 μs | per-stat, per-open (数 k/s) | on_stat (#3), on_write (#7) | native / Lua (gopher-lua) / CEL |
| **warm** | 100 μs 〜数 ms | per-miss, per-release (数百/s) | #4, #5, #6, #8, #9, #12 | Lua / WASM (Wasmtime) |
| **cold** | 数 ms 〜秒 | per-sync, per-startup (数/s〜数/分) | #1, #2, #10 | WASM も余裕、外部 gRPC も可 |

**帰結**: super-hot event #11 (on_hit) を policy hook にすると、per-read で GC or context switch が入る言語は全滅する。**設計選択**: on_hit は「eBPF map の built-in LRU 相当を kmod / eBPF 側で持ち、policy はその上の thresholds を宣言的に決めるのみ」というレイヤ分けにする (§5 の Envoy パターンに従う)。

---

## 3. policy が扱う状態と data structure の分類

どんな judgment も背後に「state × event」がある。state の性質で 6 分類 + 1 補助:

### 3.1 state class 6 + 1

1. **stateless** — path / name / tag / S3 meta ベース。motivation.md §4 の元の例。全 case に効く軽量ルールで、他の class の subroutine としても頻繁に呼ばれる。ここだけなら CEL / DSL で足りる
2. **short-history (recency)** — LRU、2Q、SLRU が要求。「最近 N 回」の順序を保持
3. **frequency** — LFU、TinyLFU、LRFU。per-item access count、または count-min sketch などの近似
4. **ghost history** — ARC、LIRS、CAR。「最近 evict した key の記録」— 現キャッシュに無いが「もし居たら hit したか」を判定するため
5. **cost / value** — GDS (GreedyDual-Size)、GDSF。per-item value (fetch cost、size)、priority-based evict
6. **offline oracle** — Belady optimal。将来 access reference を持つ (トレース replay 専用)
7. **learned** (補助) — LSTM / DL による prefetch hint、access pattern predictor。state というより「別 process の学習結果を feed する」

**判定基準**: class 1 (stateless) だけ書けば済むなら CEL / declarative DSL、class 2-4 を書きたいなら state を保持できる runtime (Lua、WASM)、class 5-7 は別次元の追加要求。

### 3.2 data structure vocabulary (primitive)

algorithm を書くのに必要な primitive:

| primitive | 用途 | engine 提供? policy 側? |
|---|---|---|
| doubly-linked list (ordered) | LRU tail 削除、head promote | **engine** (host lang 実装だと slow) |
| hash map (ino → node) | O(1) key → node | **engine** (共有 admit table と同型) |
| ghost FIFO (fixed-size) | ARC / LIRS の evicted history | **engine**、size 上限は policy 側が指定 |
| count-min sketch | TinyLFU の frequency estimate | **engine** (SIMD 利用可)、policy から increment/estimate |
| doorkeeper (bloom filter) | TinyLFU の 1-hit filter | **engine** |
| segmented list (tag 付き linked list) | SLRU、S3-FIFO の tier | **engine** (linked list の拡張として実装) |
| min-heap / priority queue | GDS の value 順 evict | **policy 側で十分** (event 頻度が warm、GC 圧低い) |
| bitmap / bit-set | 短期 access flag、SIEVE の visited bit | **どちらでも可** (小規模なら policy) |

### 3.3 分担の decision と含意

**decision**: **hot / warm event で使う data structure は engine が提供し、policy 側は「どの primitive にどう query するか」を宣言的に書くだけ**。

**理由**: state を全部 policy に持たせると、Lua の table や WASM の linear memory が実質 state store になる:
- **GC pressure** — 数百万 entry の LRU list を Lua table で持つと、GC pause が hot path に載る
- **hot reload の state 移行** — Lua VM を再作成すると state が消える、SIEVE の visited bit が全リセット = warm-up からやり直し
- **cross-language 移植** — Lua で LRU を書き、WASM で LRU を書き直す必要が出る

engine が primitive を提供し、policy は selector として振る舞うと:
- state は engine 内の Go 構造体 (heap-managed) で永続、hot reload で消えない
- primitive の追加は engine 実装 (Go) で 1 か所、全 policy が使える
- policy の複雑度が下がる (LRU を書き直す必要がない、`policy.lru(key, evict_fn)` を呼ぶだけ)

**trade-off**: engine が primitive を持つ = engine の実装コストが大きくなる。逆に policy 側に state を許すと engine は軽量だが個々の policy が算術部分を書く。**PoC scope では engine exclusive で試す** — policy 側は「どの primitive をどう使うか」の selector に徹する。それでも書けない novel algorithm は §4.5 の user eBPF plug-in で受ける (Lua/WASM のような general-purpose scripting runtime は導入しない)。

---

## 4. アルゴリズム網羅性マトリクス

§2 の event 集合と §3 の primitive で、代表的な cache 置換 / 補完 algorithm が表現できるかを 1 表で確認する。**表現不能な algorithm があれば §2 に event を追加、§3 に primitive を追加**。逆に **一度も現れない primitive は削る**。

### 4.1 13 algorithm × 表現要素

| algorithm | 分類 | 主 event | 必要 state class | 必要 primitive | note |
|---|---|---|---|---|---|
| **LRU** | 古典 | on_hit, on_admit | recency | list + hash | baseline、Meetup demo に最適 |
| **LFU** | 古典 | on_hit, on_admit | frequency | counter + priority | tie-breaker の設計難あり |
| **LRU-K** | 古典 | on_hit, on_admit | K-history | list × K | K=2 が実用最頻 |
| **2Q** | 古典 | on_hit, on_admit | recency × 2 + ghost | A1in + Am + A1out | LIRS の元祖 |
| **ARC** | 現代 | on_hit, on_admit, on_evict | recency + ghost | T1, T2, B1, B2 | 自動チューニング (IBM) |
| **LIRS** | 現代 | on_hit, on_admit | reuse-distance | stack S + queue Q | ghost 領域大、Linux で使用 |
| **W-TinyLFU** | 現代 | on_hit, on_admit | frequency sketch | count-min + doorkeeper + window LRU | Caffeine 実装、高 hit-rate |
| **S3-FIFO** | 2023 | on_hit, on_admit | segmented + freq | small/main/ghost FIFO | Yang et al 2023、シンプルで高性能 |
| **SIEVE** | 2024 | on_hit, on_admit | 1-bit visited | list + visited bitmap | Zhang et al 2024、超シンプル |
| **CacheLib SFIFO** | 産業 | on_hit, on_admit | segmented FIFO | multi-tier FIFO | Meta 実装、production 実績 |
| **GDS / GDSF** | 古典 (web) | on_admit | cost/value | min-heap | HTTP proxy 発、size + cost aware |
| **Belady optimal** | 理論 | on_admit (offline) | future oracle | trace 参照 | 上限比較専用、実運用不可 |
| **Learned prefetch** | ML | on_periodic, on_stat | features + model | 外部 runtime 呼び出し | LSTM / DL、Google MAP-Reduce 系研究 |

**参照**:
- ARC: Megiddo & Modha, FAST 2003
- LIRS: Jiang & Zhang, SIGMETRICS 2002
- TinyLFU: Einziger et al, ACM TOS 2017 / Caffeine (Ben Manes 実装)
- S3-FIFO: Yang et al, SOSP 2023 (arxiv 2301.11291)
- SIEVE: Zhang et al, NSDI 2024
- CacheLib: Berg et al, OSDI 2020

### 4.2 gap 検出

マトリクスを埋めてみて浮かんだ shape 不足:

1. **SIEVE の 1-bit visited** — 汎用の "flag primitive" が要る。§3.2 の bitmap を engine で提供 (SIEVE 専用 API にはせず、汎用 bitmap を expose する)
2. **Learned prefetch の model 推論** — engine 内 runtime ホストする vs 外部 gRPC 呼び出しする 2 案。**設計判定**: 外部呼び出し (`policy.async_hint(features) → key_list`) の hook を用意。engine 内 ML runtime は Week 5+ で `--policy-ml-plugin` として後付け
3. **ARC / LIRS の ghost list サイズ dimension** — policy が「ghost list 上限は cache と同 size」等を指定できる必要。primitive `ghost_fifo(cap)` の cap 引数で対応
4. **GDS の "cost"** — object metadata (size、fetch RTT、fetch bytes 単価) を event に含める必要。§2 の `on_admit` の input に **prev_fetch_latency_us、bytes_transferred、estimated_cost_usd** を追加すべき (現状は size のみ)
5. **Belady optimal は「別ツール」で十分** — trace replay 用の bench tool として engine 外に置く (`s3fs-ebpf trace-replay --policy=belady`)。engine の shape には含めない

### 4.3 マトリクス埋め後の判定

- ✅ **全 algorithm が表現できる = event × state × primitive の shape が過剰でない**
- ✅ **primitive リストは全部使われている** (LRU〜Learned まで各 primitive が最低 1 algorithm で登場) — 削減不要
- △ **on_evict (#12)** は ARC でしか使わないが、future ghost list 系 algo で必要になる可能性が高い、残す
- ✅ **on_hit の super-hot 問題は §2.2 の layering で解決**: on_hit は engine 内の primitive (LRU list) が自動 promote、policy には呼ばない

---

## 4.5 primitives-first architecture — 3 層 + KFunc cross-cut

§3.3 で「engine exclusive、policy は selector」、§4 で「algorithm は engine 提供の primitive で全部表現可能」と決めた帰結として、実装 architecture は 3 層 + cross-cut になる。**Lua / WASM は登場しない**。

```
Layer 1: DSL (CEL)                    ← 宣言的 selection + parameterization
                                          "for *.log use fifo ttl=5s"
Layer 2: 組込 eBPF policy program     ← engine 出荷、production 品質実装
                                          LRU / LFU / W-TinyLFU / S3-FIFO / SIEVE / ARC / GDS
Layer 3: user eBPF policy program     ← novel algorithm / domain-specific 挙動
                                          `s3fs-ebpf policy load-bpf foo.bpf.o`
Layer 0 (cross-cut): KFunc            ← 統計素性 + 汎用 primitive
                                          hit-rate hist / freq estimator / RTT hist /
                                          size dist / sequentiality detector /
                                          LRU list ops / ghost FIFO / count-min sketch
```

### 4.5.1 各 Layer の責務

**Layer 1 (DSL: CEL)**  
`if path.matches("*.log") then use("fifo", ttl=5s) else use("w_tinylfu", window=0.05)` レベルの宣言。expression のみ (state 持てない)、compile time 型検査、~μs / eval で hot event も cover。Layer 2/3 の built-in / user program 名を selector で指名。

**Layer 2 (built-in eBPF)**  
engine が `.bpf.c` で書いて出荷する production-quality 実装群。ソースは repo に含まれ、bpf2go で load 済。policy DSL から名前 (`"lru"`, `"w_tinylfu"`) で参照される。**PoC 初期出荷分**: LRU、W-TinyLFU、S3-FIFO の 3 種で hit-rate 比較 bench の材料にする。

**Layer 3 (user eBPF)**  
user が自分で `.bpf.c` を書き、clang + BTF で compile、`s3fs-ebpf policy load-bpf my_policy.bpf.o` で load。engine 側は verifier check + BTF 検証 + 署名 (allowlist) で受け入れ。SEC 命名で hook 種別を宣言 (`SEC("policy/on_admit")` 等)。**主な用途**: novel algorithm (研究発表の実装)、domain-specific 挙動 (社内 workload 専用ルール)。

**Layer 0 (KFunc)**  
Layer 2 も Layer 3 も同じ KFunc set を呼び出せる、algorithm 実装の共通 building block:
- **統計 accessor**: `get_hit_rate_5min(prefix) → u64`、`get_freq_estimate(key) → u32`、`get_size_class_hist(class) → u64`、`get_fetch_rtt_p99() → u64`
- **primitive 操作**: `lru_touch(list_id, key)`、`lru_evict(list_id) → key`、`ghost_fifo_add(fifo_id, key)`、`count_min_add(sketch_id, key)`、`count_min_est(sketch_id, key) → u32`
- **signal detector**: `sequentiality_score(ino) → u8`、`working_set_est(prefix) → u64`
- **arena 情報**: `arena_used() → bytes`、`arena_free() → bytes`

これらは kmod / daemon で Go / C 実装、eBPF program は `bpf_kfunc_call_*` で呼ぶ。**Layer 2 と Layer 3 に interface 差は無い** = user が engine 品質の実装を書ける (逆に engine 側は user と同じ制約で書くので dogfood が効く)。

### 4.5.2 なぜ user 拡張も eBPF か (Lua / WASM を選ばない理由)

|  | user eBPF | Lua / WASM |
|---|---|---|
| verifier による safety | ✓ 無限ループ・OOB・null 全部静的保証 | sandbox あるが runtime check |
| latency 予測性 | ✓ 命令数 bounded、GC 無し | GC pause、JIT ばらつき |
| hot event 実行 | ✓ kernel 側で fentry attach 可 | daemon 越え、hot event 無理 |
| 既存 infra 再利用 | ✓ bpf2go / arena / KFunc 全部共有 | 新 runtime、embedding、sandbox 追加 |
| debug story | △ bpf_trace_printk / bpftool | ○ VM が stack trace + logger 提供 |
| 開発敷居 | △ clang + BTF、verifier debug | ○ Lua は Ops でも書ける |
| ecosystem | △ cache algo の eBPF 実装は未成熟 | Caffeine (Java)、CacheLib (C++) 参考にはなる |
| narrative coherence | ◎ "eBPF FS + eBPF policy" | △ 3 種類の runtime が混在 |

**core rationale**: この project は既に eBPF native (arena、fentry、KFunc、bpf2go)。user 拡張も eBPF にすれば **runtime を 1 個も追加せず** verifier による safety を継承。開発敷居の高さは Layer 1 (DSL) と Layer 2 (built-in) で吸収し、Layer 3 (user eBPF) は「novel algorithm を書く少数の advanced user」に絞る。

### 4.5.3 S3-FIFO を user eBPF で書ける feasibility

S3-FIFO (Yang et al 2023) を Layer 3 で実装する検証:

- **必要 state**:
  - small FIFO (~10% cap) → `BPF_MAP_TYPE_QUEUE` or LRU_HASH で近似
  - main FIFO → 同
  - ghost FIFO (key のみ、data 無し) → `BPF_MAP_TYPE_HASH` fixed-size
  - per-item 2-bit freq counter → `BPF_MAP_TYPE_HASH` の value に u8
- **決定 logic**:
  - `on_hit`: freq++ (`bpf_map_update_elem`)
  - `on_admit`: small が full なら → main へ demote、ghost 経由 admit 判定
  - `on_evict` (from main): freq が閾値超なら再挿入、else evict
- **verifier friendly か**: unbounded loop なし、bounded 条件分岐のみ、map 操作は verifier 対応済 → **問題なし**

**同様に SIEVE (1-bit visited)、W-TinyLFU (KFunc の count-min を呼ぶ)、GDS (KFunc の min-heap 操作) も表現可能**。unbounded loop が必須の algorithm (LIRS の stack pruning など) は tail call で分割、あるいは book-keeping 部分を KFunc 化して user 側は決定だけに絞る。

### 4.5.4 novel algorithm の adoption path

「SOSP で新 algorithm が発表された」→ engine 再ビルド待ちにしない道:

1. user が論文を読んで `.bpf.c` に実装 (~数百行)
2. clang + BTF で compile → `.bpf.o`
3. `s3fs-ebpf policy load-bpf new_algo.bpf.o` (管理者権限)
4. DSL で `use("new_algo")` に切り替え
5. hit-rate 変化を bench で観測

**deploy 単位が `.bpf.o` 1 個**、engine 側は無変更。**hot reload は eBPF link update で完結**、in-flight event の handling は verifier 保証 + engine の barrier で safety。この shape が **PoC の scope 内で真の "policy extensibility"** を実現する最短経路。

### 4.5.5 primitives-first の risk と歯止め

**risk**: primitive を無制限に追加すると DSL が事実上 Turing-complete になり (「便利ツール」の別形態)、engine 側の maintenance 負担が線形増加する。

**歯止め (§8 の gate として追加予定)**:
- primitive 追加は **「これがなければ書けない scenario が実在するか」** を毎回 verify
- 「あったら便利」は追加しない、「無いと algorithm が表現不能」のみ追加
- Layer 2 の built-in program 数も同様の gate (「LRU + W-TinyLFU + S3-FIFO」の 3 種を default、他は user eBPF で受ける)

---

## 4.6 origin update propagation の task 分解と language 判定

「cache の origin (S3) 更新をどう検知して伝達するか」の path も policy 相当の judgment を含む。ここでも Lua/WASM を採らずに済むかを task 別に検証する。

### 4.6.1 task 分解 (6 種)

| task | 例 | 要求 | best fit |
|---|---|---|---|
| A. event ingestion | S3 SNS/SQS、Kafka、webhook 受信 | network I/O、SDK、retry、deserialize | **Go native** (AWS SDK 一級市民) |
| B. event transform | S3 event JSON → 内部 invalidation command | JSON parse、filter、key mapping | **CEL** or Go |
| C. dispatch scheduling | rate limit、dedup window、thundering herd 回避 | state + timer | **Go native** (goroutine + `x/time/rate`) or eBPF |
| D. custom origin adapter | user が GCS/Azure/独自 HTTP を backing に | 完全な programming + I/O | Go **plug-in** or (稀に) Lua/WASM |
| E. invalidation rule | "*.log は即時、cold/ は batch" | declarative | **CEL** |
| F. app 通知 (inotify 風) | 更新を app 側に push | ringbuffer / socket | **eBPF ringbuf** or Unix socket |

### 4.6.2 各 task の language 判定

- **A (event ingestion)**: AWS SDK for Go を Lua から呼ぶより Go で書く方が短い。Lua で書いても結局 Go 側 host に I/O を委譲する構造。**Go native 一択**
- **B (event transform)**: `records[].s3.object.key 抽出 → prefix match → 内部 command` は CEL の expression + jq 風 path で 100% 表現可能。compile-time 型検査つき。**CEL**
- **C (dispatch scheduling)**: state + timer は daemon 側 goroutine + `golang.org/x/time/rate` で数十行。あるいは 6.11+ の BPF timer で eBPF 側実装も可。**Go native or eBPF**
- **D (custom origin adapter)**: **唯一** Lua/WASM が意味を持ちうる領域。ただし 3 条件同時成立が必要 (次項)
- **E (invalidation rule)**: 宣言的、CEL 最適。**CEL**
- **F (app 通知)**: eBPF ringbuf に流して app が受信。**eBPF**

### 4.6.3 task D で Lua/WASM が意味を持つ条件と判定

user が独自 origin (S3 以外の社内 API 等) の adapter を書きたい場合、以下の 3 条件が同時成立すれば Lua/WASM の余地が出る:

1. user が非 Go 開発者
2. hot reload が必須 (daemon 停止不可)
3. Go SDK が無い origin (社内独自 API 等)

**s3fs-ebpf の想定 audience (BPF-friendly、system-level)** では稀。想定していない要件が現実に来たら再評価する。**PoC scope では**:

- default: **Go plug-in interface** で origin adapter を切る (`type OriginAdapter interface { Fetch / List / Watch ... }`)
- user 実装は Go で書いて engine 再ビルド or `plugin.Open`
- Lua/WASM は §7 non-goals

### 4.6.4 判定合成

origin update propagation の 6 task は **Go native (A/C) + CEL (B/E) + eBPF (F) + Go plug-in (D)** の 4 手段で 100% cover できる。Lua/WASM を導入する ROI はない。

---

## 5. 参照システムの survey (材料)

各 system の「何を optimize / 何を compromise / 何が learning」を 1 パラグラフで。**全 20 個** (Cache 5、汎用 Embed 4、Kernel 2、Sandbox 6、Admission 3)。

### 5.1 Cache 特化

- **Redis maxmemory-policy** — 固定 policy 名 (allkeys-lru / lfu / random、volatile-* variants) を config に書くのみ。DSL なし、設定値のみ。**Optimize**: 実装単純、predictable performance。**Compromise**: workload 適応性ゼロ。**Learning**: 「plugin + 設定値の 2 段」で足りる workload が多い、無理に DSL を持たなくてよいケースもある
- **memcached LRU sublists** — LRU list を hot / warm / cold の 3 sublist に分ける、per-slab-class。**Optimize**: 短命 item と長命 item の混在への robustness。**Compromise**: policy 選択の余地なし (built-in のみ)。**Learning**: **物理ストレージ tier (slab / arena) と policy を分離する** — この設計原則は s3fs-ebpf でも重要 (kmod arena は tier、policy は上乗せ)
- **CacheLib (Meta, C++)** — Segmented FIFO variants (SFIFO、LFU)、**admission policy と eviction policy を独立に設定できる**。C++ builder interface。**Optimize**: production 実績、高 hit-rate、multi-tenant 対応。**Compromise**: C++ interface で敷居高い、embedded scripting なし。**Learning**: **admission と eviction は同格の別関心事** — 分けると設計が clean になる
- **Caffeine (Java)** — W-TinyLFU 実装、builder API で policy 選択、`.expireAfterWrite(...).maximumSize(...)` の compositional API。**Optimize**: 高 hit-rate、Java ecosystem 統合。**Compromise**: JVM 前提。**Learning**: **policy を組合わせる compositional API** — 我々も個別 policy を関数として書き、Composer で組合わせる形が良い
- **Varnish VCL** — HTTP proxy 用 DSL、Actions (deliver/hash/pipe/pass)、cache decision の pattern-match DSL。**Optimize**: HTTP に特化した書きやすい DSL、静的 verify。**Compromise**: DSL の表現力は algo 実装に不足 (LRU/LFU は built-in only、DSL からは触れない)。**Learning**: **workload 特化 DSL は書きやすいが algo 表現力は原理的に低い** — DSL 案を採るなら「algo 選択と設定値」に絞るべき

### 5.2 汎用 policy embed

- **Envoy HTTP filter chain** — Lua / WASM / native の 3 段選択、hot path は native、warm path は Lua、cold path は WASM。**Optimize**: hot / cold で言語を分ける正しさ。**Compromise**: 学習曲線が急、複数言語習得が要る。**Learning**: **hot event と cold event で language を分けることは可 (Envoy が実証)** — s3fs-ebpf でも super-hot は kmod、warm は Lua、cold は WASM の 3 段が候補
- **Nginx OpenResty** — Lua 一択、per-request Lua 呼び出しが hot path に耐える (Lua co-routine + LuaJIT)。**Optimize**: 高性能 Lua (~1 μs / call、LuaJIT)、統一言語で運用簡潔。**Compromise**: LuaJIT 依存、Go daemon には持ち込みにくい (gopher-lua / go-lua は JIT 無しで ~10 μs)。**Learning**: **Lua が hot path に耐えるのは JIT のおかげ** — JIT 無しの pure Go Lua だと hot event は無理、warm 以降が現実的
- **HAProxy Lua binding** — action + fetch method + converter の 3 種 hook 型を明示的に分ける。**Optimize**: hook の意図が API から自明。**Compromise**: 3 種類しか無いので複雑な hook が書けない。**Learning**: **hook 種別を event 予算で分けて API に反映する** — s3fs-ebpf の event 12 個も予算グループごとに interface 分けるべき
- **Squid ACL** — 宣言 DSL、pattern マッチのみで algo 記述不可。**Learning**: **純 DSL は表現力の下限** — algo が書きたいなら DSL は不十分、少なくとも state + loop + arithmetic は要る

### 5.3 Kernel-side / low-level

- **eBPF LRU_HASH map** — kernel 内 built-in LRU、evict は kernel が自動。**Optimize**: syscall なしで per-CPU O(1)、ちょうど super-hot event の 予算。**Compromise**: LRU 固定、policy の選択なし。**Learning**: **「hot path は built-in primitive、policy はその上の thresholds を宣言的に決める」パターンの実例** — s3fs-ebpf でも on_hit (#11) はこの形が唯一の解
- **Linux readahead heuristics** — 過去 access history から adaptive readahead (`readahead_window` の指数伸張)、DL-based も研究段階。**Optimize**: 汎用性、無設定で効く。**Compromise**: workload 特化 tuning が困難。**Learning**: **heuristic ベースの adaptive prefetch は kernel でも既にやっている** — 我々の policy engine はその「adaptive の裁量」を app 特化に開く役割

### 5.4 Sandbox / language runtime

**採用**:

- **CEL (Google common expression language)** — expression のみ、state なし、boolean / scalar 返却、K8s admission で実績 (~μs / eval)。**Optimize**: hot event に耐える、compile time 型検査、sandbox 完璧。**Compromise**: state が持てない (「宣言的判断」に限定)。**Learning**: **s3fs-ebpf の Layer 1 DSL は CEL に決定** (§4.5)。state が要る全ての判断は Layer 2 / Layer 3 の eBPF program 側に押し、CEL は selector + parameterizer に徹する

**採用しない (PoC 外、task D materialize 時に再評価)**:

以下の 6 runtime は §4.5 で eBPF に一本化する方針が決まっているので採らない。将来 §4.6 の task D (独自 origin adapter × 非 Go チーム × hot reload 必須) が現実化した時、adapter plug-in language として再評価する:

- **Wasmtime + WIT** — WASM component model、strong isolation、overhead 数 μs / call。将来 task D の第一候補
- **gopher-lua** — pure Go、Lua 5.1 subset、~10 μs / call。将来 task D の第二候補
- **go-lua** (Shopify) — gopher-lua より高速な代替
- **Rhai** — Rust embedded scripting、Rust daemon なら候補
- **Starlark** — Python subset、determinism 強い、reproducibility が要件なら候補
- **Deno / QuickJS / boa (JS)** — 特別な理由がなければ非採用

**採用しない rationale の中心**: これらは全て「Turing-complete script を daemon 内で走らせる runtime」で、user 拡張の escape hatch として位置付けが被る。escape hatch は **user eBPF plug-in** に一本化するのが coherence 上正しい (§4.5 の "eBPF FS + eBPF policy" narrative)。runtime を 1 個も増やさない選択。

### 5.5 Admission / webhook パターン

- **K8s ValidatingAdmissionWebhook** — HTTP 越しに外部 process に policy 委任、latency 予算大 (数百 ms)。**Optimize**: policy 実装が完全独立 (別言語、別 process、別チーム管理可)。**Compromise**: latency、network 依存、fallback semantics 複雑。**Learning**: **cold event で policy を完全に別 process 化することは可能** — regulated domain の PII 判定を SecOps チーム管理の外部 service に委ねる場合の pattern
- **OPA (Open Policy Agent) + Rego** — 宣言 DSL、logic programming (Datalog 派生)、compile-time 型検査。**Optimize**: policy の宣言性、audit しやすい (all decisions は式で追える)。**Compromise**: 手続き型 algorithm 不可 (LRU など書けない)、学習曲線急。**Learning**: **compliance / access-control には Rego、cache algo には不向き**
- **CloudFront Functions / Lambda@Edge** — 決定を edge で走らせる、latency 制約強 (Functions は 1 ms 予算)。**Learning**: **CDN と同種の要件を持つのが我々の on_stat / on_read_miss**

### 5.6 参照 system 合成 → decision framework

§4.5 / §4.6 の結論を反映した最終形。**Lua / WASM は登場しない**:

| 予算 | 該当 event | 手段 | 理由 |
|---|---|---|---|
| super-hot | on_hit (#11) | **built-in eBPF program** (LRU_HASH map の native evict、or Layer 2 の LRU/W-TinyLFU) | ~1 μs 予算、DSL 呼び出しでも重い |
| hot | on_stat, on_write (#3, #7) | **CEL (Layer 1)** で selector、実処理は Layer 2/3 の eBPF | 10-100 μs、CEL は μs / eval |
| warm | #4-9 の一部 | **CEL (selector) + Layer 2/3 eBPF (state 操作)** | 100 μs〜数 ms、eBPF program の実行時間で cover |
| cold | on_startup, on_sync, on_periodic (#1, #2, #10) | **Go native (daemon)** で bookkeeping、必要なら CEL や eBPF program 呼び出し | 何でも許容、Go の SDK / ecosystem を活用 |

**origin update propagation (§4.6)** も含めた全体:

| task family | 手段 |
|---|---|
| cache 意思決定 (§2 の 12 event) | Layer 1 CEL + Layer 2/3 eBPF (§4.5) |
| origin event ingestion / adapter | Go native (§4.6 task A, C, D) |
| origin event transform / rule | CEL (§4.6 task B, E) |
| app 通知 | eBPF ringbuf (§4.6 task F) |
| 統計 accessor / signal detector | KFunc (Layer 0、§4.5) |

**含意**: policy engine は **CEL + eBPF + Go の 3 手段** で構成、runtime 追加ゼロ。Envoy 型 mixed engine (Lua + WASM + native) を採らない選択が、s3fs-ebpf のように eBPF native な project では逆に正しい (Envoy は eBPF-native ではないので mixed が必要だっただけ)。

---

## 6. 未決定質問リスト (§4.5 / §4.6 反映後)

§4.5 (primitives-first + user eBPF) と §4.6 (origin task 分解) で大枠が決まったので、初版の Q リスト (Lua/WASM を前提とした選択質問) は再構成する。以下は **CEL + eBPF + Go 前提** で残る詳細質問:

- **Q1**: hot event (on_hit) を policy hook にするか? → **No** (§2.2、§5.6)。built-in eBPF (LRU_HASH map の kernel-native evict、or Layer 2 の LRU program) に task を落とし、DSL は cap / threshold を宣言するのみ  
  → **確定**
- **Q2**: state の owner は engine or policy? → **engine exclusive** (§3.3、§4.5)。primitive はすべて engine 出荷、Layer 2/3 の eBPF program が KFunc 経由で state を操作、Layer 1 CEL は selector のみ  
  → **確定**
- **Q3**: 政策の composition API は? → **DSL layer で compose**、Caffeine 型 (builder API を CEL の select + function call で表現)  
  → 確定方向、具体的な CEL syntax は spike 時に固める
- **Q4**: hot reload での state 移行? → engine が state を保持するので policy 差替えでは in-memory state 影響なし。**Layer 3 の user eBPF 差替え**は bpf_link_update で seamless、in-flight event handling は verifier + engine barrier で safety  
  → 確定方向、in-flight event の semantics 詳細は spike で
- **Q5**: policy panic / infinite loop の fallback は? → **verifier が静的に infinite loop を弾く** (eBPF 選択の副産物)。runtime panic は eBPF program 側にはほぼ無い (verifier がメモリ操作を全 check)、CEL 側 panic は engine 側 default policy (built-in LRU) にフォールバック  
  → **確定** (verifier が Q5 の大半を解決)
- **Q6**: observability は engine 提供 vs policy が emit? → **両方**。engine は built-in カウンタ (hit-rate、evict-rate、arena 使用量)、Layer 2/3 eBPF は `bpf_trace_printk` / perf event / ringbuf で metric 追加可、Layer 1 CEL は expression 内で `metric("name", value)` を CEL 拡張関数として提供
- **Q7**: sandboxing の粒度? → **eBPF verifier 完全に一任**。syscall / alloc / time / rand の全制約が verifier + BPF program type の許可 helper set で決まる。追加 sandbox 実装不要  
  → **確定** (eBPF 選択の副産物)
- **Q8**: policy test harness の shape? → trace replay 型を第一候補 (event stream を record して policy に再生、hit-rate 差分計測)。unit test 型 (single event → expected output) を補完
- **Q9 (新設)**: novel algorithm への対応は? → **user eBPF plug-in** (§4.5.4)。`.bpf.o` を管理者 API で load、DSL の `use()` で参照。engine 再ビルド不要
- **Q10 (新設)**: user eBPF の署名 / 権限管理は? → **allowlist ベース**、`policy load-bpf` は root / CAP_BPF、file の SHA256 を engine 内 allowlist と照合、fail-close

**回答順序** (Q1/Q2/Q5/Q7/Q9 は §4.5 で解決済、残るは spike 時に固める):
1. Q3 の CEL syntax spike (単一 hook で end-to-end 通す)
2. Q4 の in-flight semantics 検証 (差替え瞬間の event 保証)
3. Q6 の metric API 決定 (Prometheus export path 含む)
4. Q8 の trace harness 設計 (event stream の record 形式)
5. Q10 の署名検証実装

---

## 7. Non-goals と将来検討

明示的 non-goal (今回の scope 外):

- **Lua / WASM の採用** — §4.5 / §4.6 / §5.4 の rationale で除外。user 拡張は eBPF plug-in、DSL は CEL、I/O は Go native の 3 手段で 100% cover。将来 §4.6 task D (独自 origin adapter × 非 Go チーム × hot reload 必須) の要件が現実化した時のみ再評価
- **分散** — 複数 daemon 間の共有 policy state。1 mount / 1 daemon の PoC scope
- **ML training loop** — features 収集は視野 (on_periodic の output に metrics 積める) が、learning 自体は out of scope。**Learned prefetch は外部 gRPC 呼び出し**で対応 (§4.2 gap #2)
- **multi-tenancy** — per-mount policy (§1 の PII scenario)。Week 5+
- **独自 DSL の設計** — **CEL を borrow**、独自 DSL は書かない
- **write-through / write-around semantics 選択** — 現状 write-back のみ (Week 4 (c) の実装通り)。policy から選択可能にする改造は Week 5+
- **user eBPF program の marketplace / registry** — allowlist ベースの手動 install。community 共有機構は Week 5+

**方針変更点** (初版から):
- kernel-side policy hook を **積極採用** に変更 (§0)。super-hot event は kernel 側 fentry attach で処理、daemon 越え不要

将来検討 (backlog):
- 各 policy の hit-rate を bench で比較 (Week 4 (e) の追加 bench)
- multi-language mixed engine (Envoy 型) の実装コスト評価
- policy hot reload の in-flight event handling semantics
- per-tenant sandbox (WASM instance per tenant)
- policy 内 metric の Prometheus export

---

## 8. 検証セクション (doc が成立しているかの gate)

doc を書き終えたら以下を self-check、埋め残しがあれば戻る:

1. **「なぜ外出しか」を 3 文で言えるか** — §1.2 のクロス表を見て、任意の 1 scenario × 1 軸で説明できる  
   → ✓ 「PII multi-tenant × 規制」で 3 文説明可能
2. **§2 の event 12 個で、時間予算のうち「language 呼び出しコストが超える」ものが特定されている**  
   → ✓ on_hit (#11) を super-hot に分類、language 不可を明記
3. **§4 の matrix で LRU/LFU/ARC/TinyLFU/S3-FIFO/SIEVE すべてに ○ が入る** (Learned は △)  
   → ✓ 全部埋まっている、Learned は「別 process gRPC」で対応
4. **§3 の primitive リストと §4 の algorithm 要求が対応しており、使われない primitive も表現不能な algorithm も無い**  
   → ✓ §4.3 で確認済み
5. **§5 の参照 system 一覧が最低 15 個**  
   → ✓ 20 個掲載 (Cache 5 + Embed 4 + Kernel 2 + Sandbox 6 + Admission 3)
6. **§6 の未決定質問が spike で答える順序に並んでいる**  
   → ✓ Q3/Q4/Q6/Q8/Q10 が spike 順に並ぶ (Q1/Q2/Q5/Q7/Q9 は §4.5 で確定済)
7. **Meetup 3 分要旨** が末尾にある (§0.5 として先頭に置いた)  
   → ✓ §0.5

**追加 gate (§4.5.5 の risk 対応)**:

8. **primitive / built-in eBPF program を追加するとき、"これがなければ書けない scenario が実在するか" を verify したか**  
   → 新 primitive PR で必須 check、「あったら便利」は却下
9. **user eBPF plug-in の受け入れが allowlist + verifier + BTF check の 3 段になっているか**  
   → §6 Q10 の実装時に確認
10. **Lua / WASM / その他 script runtime を導入したいときに §4.6 の task D 3 条件を全部満たしているか**  
    → 満たしていなければ再評価不要、Go plug-in で対応

---

## 9. 次アクション

- 次セッションで **§6 の Q1〜Q8 に答える** 別 plan を起こす
- Q 回答後、language 選定 (Lua / WASM / CEL / mixed) を決定
- 決定後、embedding API の spike (単一 event の hook を 1 個実装) を回す
- API shape が安定したら §2 の 12 event を順次 hook 化 (super-hot は eBPF map 側で built-in)
- §4 matrix の 3 algorithm (LRU、TinyLFU、S3-FIFO) を policy として実装、hit-rate を bench

---

## 参照

- **既存 code の該当箇所**:
  - `daemon/internal/ebpf/entries.go` — Entry.Prewarm bool (現状の "policy" の全て)
  - `daemon/cmd/s3fs-ebpf/main.go:418` prewarm — on_startup 現実装
  - `daemon/cmd/s3fs-ebpf/sync_entries.go:202` runSyncLoop — on_sync_tick
  - `daemon/cmd/s3fs-ebpf/admit_arena.go:186` handleReadMiss — on_read_miss
  - `daemon/cmd/s3fs-ebpf/flush_loop.go:75` runFlushLoop — on_write / on_release
  - `kmod/vfs_ops.c:1163,1169,1292` — release / fsync / create の kmod 側 hook 点
- **既存 docs**:
  - `docs/motivation.md §4` — 初期 Lua スケッチ (この doc の出発点)
  - `docs/design.md §4-§6` — CLI `policy load` 想定
  - `docs/lessons-learned.md §2.3` — upcall 3 段パターン (policy engine もこの pattern に乗る)
  - `docs/read-path-design.md §3` — Path C upcall (on_read_miss の decision point)
- **参照文献**:
  - ARC: Megiddo & Modha, FAST 2003
  - LIRS: Jiang & Zhang, SIGMETRICS 2002
  - TinyLFU: Einziger et al, ACM TOS 2017
  - S3-FIFO: Yang et al, SOSP 2023
  - SIEVE: Zhang et al, NSDI 2024
  - CacheLib: Berg et al, OSDI 2020
  - Belady: Belady 1966
  - GreedyDual-Size: Cao & Irani, USENIX 1997
