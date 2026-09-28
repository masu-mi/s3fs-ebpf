# Meetup Demo Scenario — s3fs-ebpf

30-45 分の登壇デモ台本。各節に **run** (実際のコマンド)、
**expected** (成功時の出力)、**say** (話す要点) を並べる。ぶっつけ本番
でも読み上げれば同じデモが再現できる状態で書いてある。

必要なもの:
- kernel 6.12 LTS (BPF arena + struct_ops + bpf_loop)
- docker + docker-compose
- clang-20 / bpftool
- `make all` 済 (kmod + bpf + daemon build 済)
- MinIO container up (`make compose-up`)

---

## Demo 0 — 会場でのウォームアップ

登壇 5 分前、裏で走らせておく。

### run

```bash
# 1. compose stack (MinIO + Prometheus + Grafana) up
make compose-up

# 2. bucket seed — 30 test files for hit-rate bench
BUCKET=s3fs-test N_FILES=30 ./scripts/bench-hitrate.sh --seed-only 2>/dev/null || {
  # bench script does seeding as part of full run; alternative:
  for i in $(seq 0 29); do
    printf 'demo file %02d\n' "$i" |
      docker exec -i s3fs-ebpf-minio mc pipe "local/s3fs-test/hitrate_$i.txt"
  done
}

# 3. Grafana を open (http://localhost:3000, admin/admin)
#    dashboard: "S3FS-eBPF Overview"
```

### say

- 「MinIO は S3 互換のローカルオブジェクトストア。今日はこれをバックエンドに」
- 「Prometheus + Grafana は built-in の観測スタック」

---

## Demo 1 — マウントして cat する

FUSE ではないファイルシステムから S3 のデータが読めることを見せる。

### run

```bash
# insmod + mount + daemon
make mount
LD_LIBRARY_PATH=$HOME/.local/lib sudo -E bin/s3fs-ebpf fastpath --s3 \
    --policy-bpf=builtin:policy_wtinylfu_ops \
    --metrics-addr=:9600 --sample-every=2s --sync-every=3s
```

別ターミナルで:

```bash
mount | grep s3demo
ls /mnt/s3demo/ | head
sudo cat /mnt/s3demo/hitrate_0.txt
```

### expected

```
none on /mnt/s3demo type s3fs_ebpf (rw,relatime)
hitrate_0.txt
hitrate_1.txt
...
demo file 00
```

### say

- **「FUSE ではない」** — mount type が `s3fs_ebpf`、`fuse` じゃない
- 「cat は VFS の read パスで完結。ユーザー空間デーモンには行かない
  (キャッシュ hit のとき) 」
- 「読んだデータは S3 が原本。デーモンが起動時に prewarm 済み」

---

## Demo 2 — 3 レイヤーアーキテクチャの構造

`docs/policy-engine-design.md` の図を持参スライドで、コード側で確認する。

### run

```bash
# Layer 2: built-in policies
ls bpf/policy_*_ops.bpf.o
# Layer 0: KFunc set
grep BTF_ID_FLAGS kmod/tree_traversal.c | head -10
# Layer 1: CEL policy
cat policies/default.cel
```

### expected

```
bpf/policy_freq2bit_ops.bpf.o
bpf/policy_lru_ops.bpf.o
bpf/policy_pressure_ops.bpf.o
bpf/policy_s3fifo_ops.bpf.o
bpf/policy_sieve_ops.bpf.o
bpf/policy_time_decay_ops.bpf.o
bpf/policy_wtinylfu_ops.bpf.o
...
BTF_ID_FLAGS(func, s3fs_kfunc_traverse_stub, KF_TRUSTED_ARGS)
BTF_ID_FLAGS(func, s3fs_kfunc_admit_count)
BTF_ID_FLAGS(func, s3fs_kfunc_softcap)
BTF_ID_FLAGS(func, s3fs_kfunc_evictions_total)
BTF_ID_FLAGS(func, s3fs_kfunc_time_ns)
...
prewarm: entry.size <= 33554432 && !entry.name.endsWith(".log")
policy: "builtin:policy_sieve_ops"
```

### say

- **「Layer 0 = KFunc、Layer 1 = CEL、Layer 2 = built-in eBPF、Layer 3 = user eBPF」**
- 「今動いているのは Layer 2 の W-TinyLFU built-in policy」
- 「CEL は起動時に一度読まれる、YAML でいう cluster config 相当」

---

## Demo 3 — Policy を live で hot swap

**これがハイライト**。走行中の FS の cache algorithm を curl 一発で変える。

### run

```bash
# 現在は W-TinyLFU
curl -s http://localhost:9600/metrics | grep 'policy_hook_invocations'

# S3-FIFO に切替
curl -X POST http://localhost:9600/policy/reload -d 'builtin:policy_s3fifo_ops'

# SIEVE に切替
curl -X POST http://localhost:9600/policy/reload -d 'builtin:policy_sieve_ops'

# LRU に切替
curl -X POST http://localhost:9600/policy/reload -d 'builtin:policy_lru_ops'
```

### expected

```
s3fs_ebpf_policy_hook_invocations_total 12
reloaded to /home/ubuntu/works/s3fs-ebpf/bpf/policy_s3fifo_ops.bpf.o
reloaded to /home/ubuntu/works/s3fs-ebpf/bpf/policy_sieve_ops.bpf.o
reloaded to /home/ubuntu/works/s3fs-ebpf/bpf/policy_lru_ops.bpf.o
```

dmesg で:

```
s3fs_ebpf: policy struct_ops registered (ops=...)
s3fs_ebpf: policy struct_ops unregistered (superseded, keeping ...)
```

### say

- 「struct_ops の RCU で atomic swap。in-flight な .read_folio は
  古い vtable で完走、次の呼出から新 vtable」
- 「デーモン再起動なし、mount 再マウントなし、in-flight IO の
  中断なし。**production で policy を差替える shape**」
- 「curl 一発の裏で、eBPF program の verifier + BTF relocation +
  struct_ops link create + RCU publish が走っている」

---

## Demo 4 — Hit-rate bench: policy 差の数値化

事前実行しておいた bench の結果を見せる (30 秒×7 policy で ~4 分かかるので
Meetup 中には走らせない)。

### run

```bash
# 事前に走らせておく (~4 分)
N_ACCESS=200 N_FILES=30 ZIPF_ALPHA=0.7 ./scripts/bench-hitrate.sh

# 会場では結果ファイルを見せる
cat /tmp/bench-hitrate/summary.txt
```

### expected

```
policy=observe         rate=100.00%  evictions=    0
policy=freq2bit_ops    rate=  6.50%  evictions=11581
policy=lru_ops         rate= 11.50%  evictions=10173
policy=pressure_ops    rate= 54.00%  evictions= 1627
policy=sieve_ops       rate= 62.00%  evictions=  535
policy=s3fifo_ops      rate= 65.50%  evictions=  783
policy=wtinylfu_ops    rate= 80.00%  evictions=  270
```

### say

- **「同じワークロードで、policy を変えるだけで hit 率が 6.5% → 80%」**
- 「観測じゃなくて実測。ワークロード生成は Python の Zipf sampler、
  seed 固定なので全 policy で同じ trace」
- 「observe は baseline (evict しない)、freq2bit は tier 分離のない
  naive 版、W-TinyLFU が CMS + aging で最強」
- 「W-TinyLFU は最初 25% だった。bpf_loop() で aging を復活させただけで
  55pp 跳ねた。**algorithm の勝ちだけじゃなく、BPF verifier との
  戦い方の勝ち**」
- (時間があれば) `docs/hit-rate-bench.md` の caveats section を紹介

---

## Demo 5 — KFunc で kmod-side state を policy が読む

Layer 0 が動く証拠。pressure_ops か time_decay_ops のどちらかを走らせて、
kmod の live state で policy が判断を変えるところを見せる。

### run

```bash
# time_decay_ops に swap
curl -X POST http://localhost:9600/policy/reload -d 'builtin:policy_time_decay_ops'

# 1 回目の eval → 全 ino first-sight、evict なし
curl -X POST http://localhost:9600/policy/eval

# 5 秒 (DECAY_NS) 待つ
sleep 6

# 2 回目の eval → time_ns kfunc で now - last_hit > 5s を検出、全 evict
curl -X POST http://localhost:9600/policy/eval

# dmesg で確認
sudo dmesg -T | grep -E "voted|evicted" | tail
```

### expected

```
s3fs_ebpf: struct_ops voted evict on 30/30 entries
s3fs_ebpf: policy evicted 30 entries (softcap=8)
```

### say

- **「policy program が kmod の内部変数を KFunc 経由で読んでいる」**
- 「`s3fs_kfunc_time_ns()` は kmod の ktime_get_boottime_ns() を wrap。
  同じような shape で `admit_count / softcap / evictions_total` も
  取れる」
- 「BPF program から kernel address を直接触るのは verifier が禁止。
  KFunc を経由することで、**verifier safety を保ちつつ kmod state に
  接続**」

---

## Demo 6 — Allowlist で untrusted user policy を rejct

Safety demo。誰でも file: で任意の .bpf.o を load できてしまうと meltdown。

### run

```bash
# 現在の daemon を停止
sudo pkill -INT -f 's3fs-ebpf fastpath'

# 承認済 .bpf.o のリスト (今回は built-in の SIEVE の hash だけ入れる)
cat > /tmp/allowlist.txt <<EOF
# 承認済 user policy
$(sha256sum bpf/policy_sieve_ops.bpf.o | awk '{print $1}')  demo-approved
EOF

# 承認 + user policy になりすます用の unauthorized .bpf.o
cp bpf/policy_lru_ops.bpf.o /tmp/rogue.bpf.o

# strict allowlist で daemon 起動
LD_LIBRARY_PATH=$HOME/.local/lib sudo -E bin/s3fs-ebpf fastpath --s3 \
    --policy-bpf=builtin:policy_sieve_ops \
    --policy-allowlist=/tmp/allowlist.txt \
    --metrics-addr=:9600 --sample-every=2s --sync-every=3s &

sleep 4

# 未承認 .bpf.o の load 試行
curl -sSf -X POST http://localhost:9600/policy/reload \
     -d "file:/tmp/rogue.bpf.o" || echo "REJECTED (expected)"

# 承認済 .bpf.o は通る
cp bpf/policy_sieve_ops.bpf.o /tmp/approved.bpf.o
curl -sSf -X POST http://localhost:9600/policy/reload \
     -d "file:/tmp/approved.bpf.o"
```

### expected

```
[allowlist] /tmp/allowlist.txt: 1 approved user .bpf.o hashes (builtin bypasses)
...
allowlist: /tmp/rogue.bpf.o (sha256=...) not in /tmp/allowlist.txt
REJECTED (expected)
[allowlist] approved: /tmp/approved.bpf.o (6c9d3d6cf141...) matched "demo-approved"
reloaded to /tmp/approved.bpf.o
```

### say

- **「eBPF の verifier は kernel safety を守る。allowlist は
  supply-chain safety を守る」**
- 「同じ SHA256 の .bpf.o しか load できない → tampering 検出」
- 「built-in はエンジン出荷なので bypass、user 提供は必ず allowlist 経由」

---

## Demo 7 — Grafana panels

観測 stack の見せどころ。

### run

```
http://localhost:3000 → S3FS-eBPF Overview
```

### 見せる panel (上から順)

1. **Fast Path Hit Ratio** (top-left) — fentry から見た hit 率
2. **Admits (arena entries)** — softcap 越えで policy が evict している
3. **Read-miss latency (p50 / p95 / p99)** — upcall service 時間
4. **Read-miss latency distribution (heatmap)** — tail の分布
5. **Flush latency** — write-back の対称 panel
6. **Policy hook rate** — 現 policy がどれくらい呼ばれてるか
7. **Policy evictions** — 累計 / rate/s

### say

- 「hit ratio の急激な下がりは policy 変更したときに evict しまくる
  瞬間。SIEVE → LRU に swap すると即座に見える」
- 「latency heatmap は Zipf の cold tail が p99 に効いてる形」
- 「これが毎回の deploy で見える。**A/B すら不要、hot swap で切替
  観測**」

---

## Demo 8 — teardown

```bash
sudo pkill -INT -f 's3fs-ebpf fastpath'
sleep 2
sudo umount /mnt/s3demo
sudo rmmod s3fs_kfunc
rm -f /tmp/rogue.bpf.o /tmp/approved.bpf.o /tmp/allowlist.txt
make compose-down  # optional
```

---

## 想定タイム配分 (35 分想定)

| # | 節 | 所要 | 累計 |
|---|---|---:|---:|
| 0 | Warmup (楽屋) | - | - |
| 1 | Mount + cat | 3 min | 3 |
| 2 | 3-Layer 構造 | 3 min | 6 |
| 3 | **Hot swap (live)** | 5 min | 11 |
| 4 | **Hit-rate bench 数値** | 5 min | 16 |
| 5 | KFunc live state | 4 min | 20 |
| 6 | Allowlist safety | 4 min | 24 |
| 7 | Grafana panels | 4 min | 28 |
|   | 質疑応答 | 7 min | 35 |

---

## 想定される質問と回答

- **Q: FUSE と比べて何 μs 早い?**
- **A**: `docs/benchmark-results.md` にある通り、warm stat が
  s3fs-fuse で ~50 μs、s3fs-ebpf で ~5 μs。10 倍。cold read は
  S3 側の latency 支配なので差が縮む。

- **Q: 大きなファイルは?**
- **A**: 現状は 1 ino ↔ full-file admit。100 MiB blob もそのまま
  arena に載る (128 MiB arena)。より大きい blob は range-list 化
  (backlog D-01) が要る。

- **Q: 分散環境は?**
- **A**: `docs/policy-engine-design.md §7` の non-goal。Week 5+
  で複数 daemon 間の共有 policy state を検討予定。

- **Q: なぜ Lua じゃないの?**
- **A**: hot path が μs 台、Lua の call overhead (10 μs) が予算を
  食う。CEL は「eval 時間 < 1 μs」の場合だけに使い、実際の判定は
  eBPF に寄せている。`docs/policy-engine-design.md §5-§6`。

- **Q: user が eBPF 書くの現実的?**
- **A**: 現状は unlikely。built-in の 7 policy で 99% の要求は
  cover できるはず。Layer 3 は「novel algorithm を研究発表と同期
  してすぐ試す」用途。

---

## 会場で刺さらなかったら

- **数値の代わりに動画**: Grafana panel の 30 秒 recording を
  用意しておく (hit ratio が hot swap で切替わる瞬間)
- **時間圧迫**: Demo 6 (allowlist) を最後にして、時間切れなら省略
- **技術詳細に食いつかれた**: `docs/lessons-learned.md` の各節に
  戻って個別に答える (arena mm/gup / bpf_loop / struct_ops
  hot reload / etc.)

---

## 参照

- `docs/motivation.md` — 全体設計・比較 (発表スライドの土台)
- `docs/policy-engine-design.md` — Layer 別詳細
- `docs/hit-rate-bench.md` — Demo 4 の数値の出典と caveats
- `docs/policy-attach-comparison.md` — struct_ops vs fmod_ret
- `docs/lessons-learned.md` — 実装ハマりポイント
