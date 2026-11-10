---
name: policy-develop
description: キャッシュ policy (Layer 1 CEL / Layer 2 built-in eBPF pick_victim・on_hit / Layer 3 独自 eBPF) を書く・変える・検証する手順 — hit-rate 比較 (bench-hitrate)、simulator との一致 (test-policy-conformance)、verifier を通した Go model との照合、hot reload。「policy を書いて」「policy を変えた」「SIEVE / S3-FIFO / W-TinyLFU / LRU」「hit rate を比べて」「pick_victim」「policy を reload して」と言われたとき、または policies/*.cel / bpf/ の policy を触るときに使う。
---

# policy の開発と検証

書き方は `docs/user/writing-policies.md` (3 層の選び方、allowlist)、設計は `docs/design/policy-engine-design.md` / `cache-algorithms.md` / `ac58-pick-victim.md` / `wtinylfu-pick-plan.md`、struct_ops の glue は `docs/dev-notes/struct-ops-kmod-glue.md` と `struct-ops-data-members.md`。

## 層の選び方

| 変えたいもの | 層 | 反映 |
|---|---|---|
| built-in policy の選択、prewarm の対象 | 1 (CEL、`policies/*.cel`) | `go test ./internal/policy/` で全 `.cel` が読めることを確かめる |
| built-in の eBPF policy の調整 | 2 (`bpf/` の `.bpf.c` を編集) | `make bpf` → `POST /policy/reload` (BPF_LINK_UPDATE 1 回。読み取りは失敗しない) |
| 独自 policy | 3 (`--policy-bpf=file:<path>`) | allowlist の SHA256 承認 (`policies/allowlist.example.txt`) |

## 検証の順序

1. **単体**: daemon 側は `make test-daemon`。policy を verifier に通して Go model と照合: `make test-wtinylfu-harness` (root、kmod / mount 不要)。kmod 側の struct_ops dispatch は `kmod-unit-test` の `s3fs_ebpf_policy_ops`。
2. **実機の振る舞い**: `make test-policy-reload` (hot swap、非 struct_ops は 400)、`make test-policy-data-members` (flags / name)、`make test-pick-victim-stale` (admit set を出た ino が policy の list に残らない)、`make test-pattern-aware-admit`、`make test-pcache-prewarm*`。`regression-test` skill。
3. **simulator との一致**: `make test-policy-conformance` (live の hit 数が `dev trace-replay` の simulator と 3.5 pt 以内、差分アクセスも上限内)。タイミングに敏感。ホストが遅いと外れるので、外れたら先に環境 (残った `mc admin trace`、負荷) を疑う (`perf-bench-verify` §0)。
4. **質の比較 (H)**: `scripts/bench-hitrate.sh` (`make bench-hitrate`)。Zipf の負荷で policy ごとの hit rate (= 1 − upcall / access) を、`dev trace-replay` の LRU / SIEVE / OPT と並べる。

```bash
N_FILES=30 N_ACCESS=2000 ZIPF_ALPHA=0.7 SOFTCAP=8 SEED=42 PCACHE_DROP=1 DAEMON_EXTRA_FLAGS=--arena-metadata-only \
  POLICIES="observe_ops lru_pick_ops sieve_pick_ops s3fifo_pick_ops wtinylfu_pick_ops" OUT=/tmp/r/H scripts/bench-hitrate.sh
```

- 複数 seed / α: `make bench-hitrate-multi` (`ALPHAS=`)、大小混在: `make bench-hitrate-mixed` (D-01)、policy の呼び出しコスト: `scripts/bench-policy-cost.sh`、アクセスパターン別: `scripts/bench-read-pattern-policy.sh`。
- **page cache があると policy に hit の信号が届かず差が出ない。** policy の質は `PCACHE_DROP=1` (ebpf-pcachedrop) で見る (`docs/dev-notes/bench-suite-overview.md` の H)。
- `bench-hitrate.sh` は `s3fs-test` を作り直して中身を消す。他の bench の object (`hello.txt` …) が消えるので H は最後に回し、後で seed し直す。
- 比較の前提 (capacity mode、各行が SOFTCAP 個の slot の hit rate、OPT との比較) は `scripts/bench-hitrate.sh` の冒頭コメント。値の記録・検証は `perf-bench-verify` §3〜4。

## 注意

- policy の状態は kmod の状態とずれうる (in-flight の miss を on_insert する、ghost queue の上限が緩い、など。issue #22 で 7 pt の差になった)。ずれは (3) の conformance が拾う。
- policy が kmod の不変条件 (ロック順、pin) に触れるなら `docs/kmod-pcache-invariants.md`。変えたら同じ変更で更新する。
