# eBPF-based FS framework の hook 分離設計 — s3fs-ebpf を素材にした将来設計 note

s3fs-ebpf は「S3 を FS として見せる特定用途 PoC」だが、**FUSE のような汎用 eBPF FS framework** の下地としても解釈できる。この doc は:

1. 現在の s3fs-ebpf で S3 機能がどの layer に置かれているか (kmod / eBPF / daemon) を確認
2. **framework 化** した時に hook をどう分離すべきか (4 role × 4 軸)
3. Cache policy 開発者と backend 開発者と end user が別々の場合の **security model**

を 1 か所にまとめる。**将来 F-04 相当の task を切るとき** の設計 note。

**関連 doc**:
- [`design.md`](design.md) — 現行 3-layer 統合設計
- [`policy-engine-design.md`](policy-engine-design.md) — Layer 1-4 の policy dispatch 設計 (§4-§6 が本 doc の下敷き)
- [`ebpf-ecosystem-gaps.md`](ebpf-ecosystem-gaps.md) — upstream に足りない機能
- [`component-communication.md`](component-communication.md) — 現行 11 通信路の mermaid 図

---

## 1. 現在の s3fs-ebpf: S3 機能はどこにあるか

**結論**: **S3 プロトコル / HTTP / bucket-key の知識は 100 % daemon (userspace Go) 側**。kmod と eBPF は完全に backend-agnostic。

### 1.1 実測 (`grep` で確認)

| layer | S3 プロトコルコード? | 検証コマンド | 実際に持つのは |
|---|---|---|---|
| **kmod (`kmod/*.c`)** | ❌ **なし** | `grep -rn 's3://\|minio\|s3\.Client' kmod/*.c` → 0 hit | VFS 適応、admit table、pending waiter、flush/rename/delete queue |
| **eBPF (`bpf/*.c`)** | ❌ **なし** (algorithm 名としての "S3-FIFO" 文字列のみ) | 同上 → policy_*.bpf.c 内の comment のみ | cache 判定 (evict/admit/hit/pressure)、fentry stats |
| **daemon (`daemon/**/*.go`)** | ✅ **全部** | `minio-go` / `cloud.google.com/go/storage` import は daemon 側のみ | `daemon/internal/{s3,gcs}/client.go` (HTTP client) + `objstore.Backend` interface |

### 1.2 抽象化の境界

`daemon/internal/objstore/objstore.go` の `Backend interface` が backend 抽象化の source of truth:

```go
type Backend interface {
    Bucket() string
    Stat(ctx, key) (ObjectMeta, error)
    Get(ctx, key) ([]byte, error)
    GetRange(ctx, key, offset, size) ([]byte, error)
    Put(ctx, key, data) error
    Delete(ctx, key) error
    Rename(ctx, oldKey, newKey) error
    List(ctx, prefix) ([]ObjectMeta, error)
}
```

- S3 (`internal/s3`) と GCS (`internal/gcs`) が同 interface を実装
- kmod 側は「ino → uaddr/size の tuple」しか知らず、その uaddr の bytes を誰が置いたかは全く関知しない
- BPF policy 側は「ino / size / count / time_ns」しか触れず、これも backend 無関係

### 1.3 kmod に "倒している" 機能

daemon ではなく kmod に置いた機能を明示的に列挙 (これらは backend が S3 でなくても要る):

| 機能 | 実装場所 | なぜ kmod か |
|---|---|---|
| VFS fop dispatch (.lookup / .read_folio / .write / ...) | kmod | VFS callback は kernel space から呼ばれる |
| admit table (ino → uaddr map) | kmod | .read_folio が sync に参照する必要 |
| pending waiter (folio_lock 中の block) | kmod | `wait_for_completion_interruptible_timeout` は kernel side でしか組めない |
| flush / delete / rename queue | kmod | fop event の origin は kernel、userspace が消費 |
| entry table (dir listing の source) | kmod | `.lookup` / `.iterate_shared` が sync 参照 |
| attr overrides (chmod/chown 記憶) | kmod (G-02) | icache eviction を跨いで permission を保持 |
| KFunc set (`admit_count` / `softcap` / ...) | kmod | BPF verifier から呼ばれる helper |
| policy hook anchors (`s3fs_policy_on_*`) | kmod | fmod_ret / struct_ops attach 対象 |
| arena kern_vm mapping (BTF hack) | kmod | `bpf_arena_get_kern_vm_start` が非 export のため |

**要点**: kmod は「FS のトランスポート層」に徹していて、"どの backend か" の判断は 1 行もない。daemon がその decision を独占する。

---

## 2. FUSE との対比

| 観点 | FUSE | s3fs-ebpf (現行) |
|---|---|---|
| kernel-side driver | `fuse.ko` (generic) | `s3fs_kfunc.ko` (this project 専用、汎用化未) |
| user daemon が実装する hook | `fuse_operations` 40+ 個 | `objstore.Backend` 8 個 (VFS 全部ではなく "backend I/O" だけ) |
| VFS ↔ user daemon transport | `/dev/fuse` character device + custom protocol | miscdev (`/dev/s3fs_ebpf`) + 12 ioctl + ringbuf |
| cache policy | daemon 内 (writeback_cache 等の mount option 経由) | BPF program (verifier-safe、hot swap 可) |
| per-request BPF override | ❌ なし | ✅ struct_ops vtable (Layer 3) |
| policy sandboxing | daemon 実装依存 (孤立度は daemon 次第) | BPF verifier + KFunc set で kernel が保証 |

**主要な差**:

- FUSE は "VFS → daemon" の全てを generic protocol にしたが、**cache policy を差し込む場所がない** (daemon 内部の実装マター)
- s3fs-ebpf は "VFS → backend I/O" を daemon に、"cache policy" を BPF に分けている。**policy と backend が独立**
- 一方 s3fs-ebpf の kmod は 1 backend 前提で hard-coded (generic ではない) — framework 化には kmod の generic 化が必要

---

## 3. 想定する 4 role

**framework 化** した時、これら 4 role が独立して仕事できるべき。

| role | 主に触るもの | 権限 | 必要な知識 |
|---|---|---|---|
| **framework 開発者** | kmod (generic driver) + KFunc set + verifier hook | root、kernel patch を write | VFS internals、eBPF verifier |
| **backend 開発者** (S3 / HDFS / IPFS / ...) | userspace daemon | 自身の backend への credential のみ、mount 権限は端末側 | backend protocol、framework の ioctl/ringbuf ABI |
| **cache-policy 開発者** | BPF program (`.bpf.c` + BTF、struct_ops vtable) | BPF program 書ける権限、attach は sysadmin 経由 | eBPF verifier、KFunc contract |
| **end user** | mount コマンド + `--backend s3 --policy sieve` の CLI | mount(2) 可能な UID | なし (backend と policy は allowlist から選ぶだけ) |

**現状の s3fs-ebpf は 4 role が unify されていて**、開発者 = end user = 全 role を 1 人がやる想定。framework 化では **役割ごとに独立 deployable** にしたい。

---

## 4. Hook 分離の 4 軸

汎用 eBPF FS framework で hook をどう切るか。s3fs-ebpf の設計を一般化する方向で検討。

### 4.1 軸 A: adapter kind (何の adapter か)

| Hook 種 | 実装場所 | 例 (現行) | framework 化の姿 |
|---|---|---|---|
| **VFS-adapt** | kmod (generic) | `.read_folio` → admit table lookup → GUP | 全 backend で共有、framework が持つ |
| **Backend-adapt** | daemon (per-backend) | `s3.Client.Get` / `gcs.Client.Get` | backend developer が `Backend` interface を実装 |
| **Cache-policy** | BPF struct_ops | `on_admit` / `on_evict` / `on_hit` | policy developer が vtable を書く、KFunc で bounded |
| **Metadata** | kmod (attr overrides) or daemon | G-02 の per-ino chmod override | 「session-only metadata」は kmod、「durable metadata」は backend hook |

**設計 principle**: layer をまたぐ hook は避ける。1 hook = 1 layer が触るだけ。

### 4.2 軸 B: data-plane vs control-plane

data-plane (per-request、hot) と control-plane (rare、cold) を明確に分離。

| plane | 頻度 | 現行 transport | framework 化の推奨 |
|---|---|---|---|
| **data-plane hot** | request/秒 | fentry BPF (getattr) | BPF struct_ops のまま、KFunc に latency-safe な helper だけ用意 |
| **data-plane cold** (miss) | miss/秒 | miscdev read + ringbuf + ADMIT_ARENA + SIGNAL_ADMIT | 現状の 3-段 upcall で OK。KF_SLEEPABLE で BPF 側にする道は S3 のような blocking I/O では非現実的 |
| **control-plane** | ~30s | SYNC_ENTRIES ioctl / DRAIN_EVICTIONS ioctl | 引き続き ioctl or netlink。生 syscall で低コスト |
| **event stream** (metric / trace) | k/秒 (将来) | 未実装 | **USER_RINGBUF** が本命 (`ebpf-ecosystem-gaps.md §4` 参照) |

**設計 principle**: hot path に blocking upcall を入れない。BPF 内で決めきれない事象 (S3 Get) は 3-段 upcall、それ以外は BPF 内で。

### 4.3 軸 C: trust boundary

各 role の権限境界。

```
┌──────────────────────────────────────────────────────────────┐
│  end user (mount 権限のみ)                                    │
│  - mount コマンド発行                                          │
│  - --backend / --policy を allowlist から選択                  │
│  - S3 credential は end user か backend developer が持つ      │
└──────────────────────────────────────────────────────────────┘
          │ mount(2)、mount option
          ▼
┌──────────────────────────────────────────────────────────────┐
│  backend daemon (per-mount、userspace)                        │
│  - 自 backend の credential 保持                              │
│  - framework の ioctl/ringbuf ABI に沿って動く                 │
│  - **BPF program を load できない**                           │
│  - **cache policy に干渉できない**                            │
└──────────────────────────────────────────────────────────────┘
          │ ioctl / ringbuf (framework-defined ABI)
          ▼
┌──────────────────────────────────────────────────────────────┐
│  framework kmod (kernel, GPL、root only)                     │
│  - generic VFS adapter                                        │
│  - admit table / pending waiter / queue                       │
│  - KFunc set 定義 (policy dev への contract)                  │
│  - struct_ops vtable の register                              │
└──────────────────────────────────────────────────────────────┘
          │ struct_ops attach、KFunc dispatch
          ▼
┌──────────────────────────────────────────────────────────────┐
│  cache-policy BPF (verifier-bounded、attach は sysadmin)     │
│  - struct_ops vtable の method 実装                           │
│  - KFunc 経由でしか kmod を触れない                            │
│  - 自 policy 用の map のみアクセス可                          │
│  - **backend の credential にアクセスできない**               │
│  - **他 policy の map を触れない**                            │
└──────────────────────────────────────────────────────────────┘
```

**Key invariants**:

1. **backend developer は BPF に触れない** — cache decisions を hijack できない
2. **policy developer は backend credential に触れない** — S3 秘密鍵を policy から読めない
3. **end user は allowlist から選ぶだけ** — kernel/BPF/daemon の実装詳細を知らなくてよい
4. **framework kmod だけが全 layer を橋渡し** — kernel-side sanity check の中央集権

### 4.4 軸 D: per-mount isolation

現在の s3fs-ebpf は **single-mount PoC** (1 kmod、1 daemon、1 policy)。framework 化では以下が要る:

| 資源 | 現状 | framework 化 |
|---|---|---|
| kmod module instance | 1 グローバル | 1 module、複数 super_block |
| admit table | グローバル 1 個 | per-super_block or per-mount ID |
| daemon process | 1 個 | mount ごとに 1 個 (fuse と同じ) |
| BPF policy program | グローバル 1 vtable | per-mount attach (struct_ops の複数 instance) |
| BPF map (policy 内部 state) | グローバル 1 個 | per-mount instance か mount ID を key に含める |
| KFunc call context | mount 判別なし | current context から mount ID を取得できる KFunc `s3fs_kfunc_current_mount_id()` を追加 |

**mount-multiplexing の主な困難**:

- struct_ops vtable の複数 attach: kernel 6.9+ で `attach_target` を struct_ops に追加すれば可能
- KFunc の context: `bpf_ktime_get_ns` のように global な helper と、mount context 依存の helper を明確に区別
- daemon process 起動: end user が mount 時に daemon を fork する model (FUSE と同じ)、あるいは systemd unit template で

---

## 5. Security model

上の 4 軸を踏まえた **信頼モデル** の具体化。

### 5.1 各 role が実行できないこと (禁則)

| Role | 禁じられる操作 | 保証機構 |
|---|---|---|
| end user | policy program の直接 load、backend credential の read、他 user の mount 越しアクセス | UNIX mount 権限 + policy allowlist + mount namespace |
| backend developer | policy BPF program の書き換え、他 backend の credential 参照、kernel/kmod 変更 | daemon が unprivileged 相当 (mount 権限だけ separate)、BPF load は sysadmin 経由 |
| policy developer | backend credential 参照、他 policy の map 参照、kernel の任意関数呼び出し | BPF verifier + KFunc set + per-policy map namespace |
| framework developer | (制約なし。code review + upstream reviewer が唯一のガード) | code review + kernel upstream process |

### 5.2 攻撃 surface と対応

| 攻撃 | 現状 | framework 化での防御 |
|---|---|---|
| 悪意ある policy BPF が admit table を破壊 | KFunc は read-only helper のみ、write は kmod 経由でしか触れない | 現状のまま、KFunc に write helper を安易に追加しない |
| 悪意ある backend daemon が偽データを arena に書く | arena mmap は daemon の mm、byte-for-byte diff は user 側で検証可 | daemon の mm を trusted に、byte diff で検出。改ざんは **backend developer が悪意ある場合の user リスク** で、mount 選択の trust に帰着 |
| policy 間の side-channel (timing 経由の情報漏洩) | 未対策 | future: per-policy map の isolation + KFunc の timing helper を制限 |
| unprivileged user が任意 policy を load | 現在は sysadmin 権限が必要 (BPF load) | policy allowlist (`--policy-allowlist=<hash-list>`) を strict enforce (P-05 実装済) |

### 5.3 policy signing / allowlist

現行 P-05 で SHA256 allowlist + `Verify()` を実装済 (`daemon/internal/policyprog/allowlist.go`)。framework 化では:

- allowlist を **kernel-side** に持って、`bpf(BPF_PROG_LOAD)` で verifier がハッシュチェック → unlisted は EPERM
- あるいは `bpf_prog_load` の付加 metadata に署名付けて BPF filesystem で管理 (LSM の bpf hook 経由)

現状は userspace 側の "loader が check して load 前に reject" だが、これは userspace loader を bypass すれば ineffective。**kernel side での enforcement** が framework 完成には必要。

---

## 6. Hook 分離の recommendation まとめ

上を総合しての **framework 化での hook 分離案**:

### 6.1 kernel side (framework kmod)

- **generic VFS adapter**: fop dispatch は 1 か所、admit/pending/queue の machinery も generic
- **KFunc contract**: cache policy が呼べる helper の set。write 系は最小限、read/observe が中心
- **struct_ops vtable schema**: `on_admit / on_evict / on_hit / on_pressure` の 4 hook を framework が定義、policy はこれだけ実装
- **generic upcall API**: `/dev/<fs>_ebpf` の ioctl set は backend-neutral (ADMIT_ARENA / SIGNAL_* / POP_* / SYNC_ENTRIES)

### 6.2 backend daemon (per-backend implementation)

- **Backend interface** (現行 `objstore.Backend` の一般化) を実装
- framework SDK が ioctl/ringbuf の wrapping を提供し、backend developer は `Backend` interface だけ書けば済むように
- BPF program の load 権限は不要 (mount 権限で足りる)

### 6.3 cache-policy program (per-policy .bpf.o)

- struct_ops vtable を実装、KFunc set 経由でのみ kmod を触る
- 自 policy 内部の map (state, counters) 定義自由、但し他 policy と分離
- SHA256 signing + kernel-side allowlist で strict enforcement

### 6.4 end user (mount コマンド)

```
mount -t ebpffs \
    -o backend=s3,creds=/etc/aws-credentials.json,bucket=mybucket \
    -o policy=sieve,policy-allowlist=/etc/ebpffs/policies.sha256 \
    none /mnt/mydata
```

的な CLI 1 発。daemon の fork も kernel/systemd が担当。

---

## 7. Open questions

現時点で決着していない設計論点:

1. **mount time の daemon 起動**: FUSE モデル (mount コマンドが daemon を fork)、あるいは systemd generator + template unit — どちらが linux distribution 側に受け入れられやすいか
2. **policy hot swap の semantics**: struct_ops の atomic swap は明確だが、"swap 中に in-flight な hit event はどう扱う?" の GC 保証を明文化必要 (`docs/policy-attach-comparison.md §1.5` の未対応項目)
3. **backend の failure semantics**: S3 の 4xx/5xx を FS 層でどう見せる (EIO / ENOSPC / EACCES ?) — backend developer に任せると挙動が backend ごとに散らばる。framework 側で強制 mapping を持つべきか
4. **partial-admit と policy**: D-01 の range-list admit を policy vtable がどう見るか。`on_admit(ino, offset, size)` の hook signature を採るなら backward compat break
5. **sleepable KFunc の位置付け**: cache policy が sleepable struct_ops で書けるなら、BPF 側で S3 Get を直接呼ぶ道が開くが、`ebpf-ecosystem-gaps.md §3` の制約が厳しい。framework SDK が sleep 可能な helper を wrap するか

これらは **後続 PoC で 1 つずつ答える**。s3fs-ebpf は答え合わせできる素材を提供する立場。

---

## 8. TL;DR (3 行)

> - s3fs-ebpf の S3 プロトコル知識は 100% daemon 側。kmod と BPF は完全に backend-agnostic で、framework 化の下地としては good starting point
> - hook 分離は **A (adapter kind) × B (data/control plane) × C (trust boundary) × D (per-mount isolation)** の 4 軸で切る。1 hook = 1 layer / 1 role の原則
> - security は「backend dev は BPF 触れない」「policy dev は credential 触れない」「end user は allowlist から選ぶだけ」の 3 invariant を kernel-side enforcement で保証する
