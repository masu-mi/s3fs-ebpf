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
- Lua/WASM の embed 実装、runtime 選定、具体的 API 決定 (別セッション)
- benchmark (policy 適用前後の hit-rate)
- CLI (`s3fs-ebpf policy load`) の詳細
- 独自 DSL の設計 (Lua/WASM/CEL のどれかを borrow する前提)
- multi-mount / per-tenant policy (Week 5+)
- kernel 内 (kmod/eBPF) での policy 評価 (常に daemon 側で評価)

---

## 0.5. Meetup 3 分要旨

> 1. **policy engine は「app の外に出したほうが正しい判断がある」ケースを狙う** — 単一アプリの単一 workload なら app に埋め込めば良い。**hot reload / 横断的政策 / 役割分業 / 規制** の 4 条件が 2 つ以上重なる時に外出しが正当化される。
>
> 2. **FS lifecycle の意思決定点は 12 個ある** — startup / sync / stat / read-miss / admit / pressure / write / release / fsync / periodic / hit / evict。各点に「時間予算」があり、super-hot (per-read) は native、hot は Lua/CEL、warm 以上は WASM も OK — この**時間予算が language 選定の第 1 制約**。
>
> 3. **algorithm 表現力は「event × state × primitive」で検証する** — LRU/LFU/ARC/TinyLFU/S3-FIFO/SIEVE がすべて engine の shape で表現できることを §4 のマトリクスで確認。**表現できない algorithm があれば event か primitive を拡張、使われない primitive があれば削る** — 設計の gate として機能する。

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

**trade-off**: engine が primitive を持つ = engine の実装コストが大きくなる。逆に policy 側に state を許すと engine は軽量だが個々の policy が算術部分を書く。**PoC scope では engine 主導で試す**、複雑化したら policy 側に一部戻す。

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

- **Wasmtime + WIT (Bytecode Alliance)** — WASM component model、strong isolation、bindings 生成自動、AOT / JIT。**Optimize**: strong isolation、ML/CPU intensive にも耐える。**Compromise**: overhead 数 μs / call (hot event には重い)、ecosystem 発展途中。**Learning**: **warm / cold event と ML runtime ホストに最適**
- **gopher-lua** — pure Go、Lua 5.1 subset、コルーチン、JIT なし (~10 μs / call)。**Optimize**: 依存ゼロ (cgo 不要)、embed 簡単。**Compromise**: JIT 無く数十 μs、Lua 5.3+ の一部機能欠落。**Learning**: **Go daemon 統合の第一候補**、warm event に最適
- **go-lua** (Shopify) — gopher-lua より高速な pure Go Lua VM。**Learning**: gopher-lua の代替、選定時に compare
- **CEL (Google common expression language)** — expression のみ、state なし、boolean / scalar 返却、K8s admission で実績 (~μs / eval)。**Optimize**: hot event に耐える、compile time 型検査、sandbox 完璧。**Compromise**: state が持てない (「宣言的判断」に限定)。**Learning**: **stateless class (§3.1 class 1) の判断には CEL が最良** — hot event の on_stat / on_write は CEL、warm 以降で state が要るところは Lua、というレイヤ分けが自然
- **Rhai** — Rust embedded scripting、type-safe、Rust ecosystem 前提。**Learning**: Rust daemon なら候補、Go daemon には不向き
- **Starlark** — Python subset、Bazel 発、determinism 強い (no I/O、no random)。**Optimize**: reproducibility が保証される (CI / test でも同じ結果)。**Compromise**: I/O 不可 = 外部システムと連携できない。**Learning**: **policy の determinism が要件になる scenario では Starlark が第一候補** — s3fs-ebpf の scenario では I/O 不可は厳しい (S3 meta を read できないと不便) が、規制 domain で「決定木の再現性」が要る場合は考慮
- **Deno / QuickJS / boa (JS)** — sandbox 可、web ecosystem 統合。**Compromise**: overhead 大、JS 独特の型システム。**Learning**: 特別な理由がなければ Lua を先に検討

### 5.5 Admission / webhook パターン

- **K8s ValidatingAdmissionWebhook** — HTTP 越しに外部 process に policy 委任、latency 予算大 (数百 ms)。**Optimize**: policy 実装が完全独立 (別言語、別 process、別チーム管理可)。**Compromise**: latency、network 依存、fallback semantics 複雑。**Learning**: **cold event で policy を完全に別 process 化することは可能** — regulated domain の PII 判定を SecOps チーム管理の外部 service に委ねる場合の pattern
- **OPA (Open Policy Agent) + Rego** — 宣言 DSL、logic programming (Datalog 派生)、compile-time 型検査。**Optimize**: policy の宣言性、audit しやすい (all decisions は式で追える)。**Compromise**: 手続き型 algorithm 不可 (LRU など書けない)、学習曲線急。**Learning**: **compliance / access-control には Rego、cache algo には不向き**
- **CloudFront Functions / Lambda@Edge** — 決定を edge で走らせる、latency 制約強 (Functions は 1 ms 予算)。**Learning**: **CDN と同種の要件を持つのが我々の on_stat / on_read_miss**

### 5.6 参照 system 合成 → decision framework

上を合成すると、event 予算ごとに language の候補が絞られる:

| 予算 | 該当 event | language 候補 | 理由 |
|---|---|---|---|
| super-hot | on_hit (#11) | native / eBPF built-in policy | ~1 μs 予算、language 呼び出しは不可能 |
| hot | on_stat, on_write (#3, #7) | CEL / native / (LuaJIT ならぎり) | 10-100 μs、state 少なめ |
| warm | 6 events (#4-9 の一部) | gopher-lua / WASM (Wasmtime) | 100 μs〜数 ms 予算、state OK |
| cold | on_startup, on_sync, on_periodic (#1, #2, #10) | WASM / gopher-lua / 外部 gRPC | 何でも許容 |

**policy 単位で language を選ばせる (mixed engine)** が Envoy 型で最強だが、実装コスト大。**PoC scope では単一 language (gopher-lua 濃厚) + hot event は eBPF built-in で妥協** が現実的。

---

## 6. language 選択の観点 (未決定質問リスト)

§0〜§5 が固まった後の別セッションで答えるべき questions。**§2〜§4 の taxonomy がある種類の質問には既に答えを含む**、その旨を書いておく。

- **Q1**: hot event (on_hit) を policy hook にするか? → **No 濃厚** (§2.2、§5.6 の結論)。super-hot は eBPF LRU_HASH built-in、policy は cap や thresholds を宣言的に決めるのみ
- **Q2**: state の owner は engine (query API 提供) か policy (自前で持つ) か? → **engine 主導** (§3.3 の decision)。primitive を engine で実装、policy は selector
- **Q3**: policy の compositional API か single policy か? → **compositional 濃厚** (Caffeine 型、§5.1)。ただし PoC 初期は single policy から
- **Q4**: hot reload での state 移行保証は必要か? → engine が state を持つ (Q2) なら policy 差替えで state は消えない。**必要**
- **Q5**: policy 内 panic / infinite loop の fallback は何? → default LRU 相当 (engine built-in)。timeout budget を event 予算に応じて設定 (hot: 100 μs で timeout、warm: 5 ms で timeout)
- **Q6**: observability (metrics / traces / hit-rate) は engine が提供 vs policy が output? → 両方。engine は built-in カウンタ、policy は `policy.emit_metric("name", val)` で追加可能
- **Q7**: sandboxing の粒度 (syscall / alloc / time / rand)? → language 側の機能に依存。Lua / WASM とも syscall 遮断は engine 側でホスト API を限定
- **Q8**: policy test harness の shape? → trace replay 型 (event stream を record して policy に再生) を第一候補、unit test 型 (single event → expected output) を補完

**回答順序**:
1. Q1 (hot event 扱い) — language の下限を決める、最初に答える
2. Q2, Q3 (state owner + composition) — API shape を決める
3. Q4, Q5 (reload, fallback) — 運用要件
4. Q6, Q7, Q8 (observability, sandbox, test) — 実装詳細

---

## 7. Non-goals と将来検討

明示的 non-goal (今回の scope 外):

- **分散** — 複数 daemon 間の共有 policy state。1 mount / 1 daemon の PoC scope
- **ML training loop** — features 収集は視野 (on_periodic の output に metrics 積める) が、learning 自体は out of scope。**Learned prefetch は外部 gRPC 呼び出し**で対応 (§4.2 gap #2)
- **multi-tenancy** — per-mount policy (§1 の PII scenario)。Week 5+
- **policy DSL の自作** — Lua / WASM / CEL のどれかを borrow、独自 DSL は書かない
- **kernel-side policy hook** — hot event の kernel 内評価は eBPF map の built-in policy (LRU_HASH) に留める、policy engine は daemon 側のみ
- **write-through / write-around semantics 選択** — 現状 write-back のみ (Week 4 (c) の実装通り)。policy から選択可能にする改造は Week 5+

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
6. **§6 の未決定質問が 5〜10 件、次セッションで答えるべき順序に並んでいる**  
   → ✓ 8 件、4 段階に順序付け
7. **Meetup 3 分要旨** が末尾にある (§0.5 として先頭に置いた)  
   → ✓ §0.5

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
  - `docs/deisign.md §4-§6` — CLI `policy load` 想定
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
