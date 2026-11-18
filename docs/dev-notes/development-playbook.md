# 開発の進め方: 技術トピック、典型作業、コツ (2026-11-17)

ここまでの開発で出てきた技術トピックと、繰り返し発生する作業の型をまとめる。個々の詳細は各 doc に任せ、ここは「何を、どの順で、どこを見るか」の地図に徹する。経緯は [`../journal/history.md`](../journal/history.md)、地雷の個別の話は [`lessons-learned.md`](lessons-learned.md)。

## 1. 現在の構成 (これだけ押さえれば読み始められる)

| 層 | 何がいるか | 最初に読む doc |
|---|---|---|
| kmod (`kmod/`) | VFS の実装。中身は chunk 表と page cache。miss は 1 本の `reqq` で daemon に積む (read / flush / delete / rename が同じ FIFO) | [`../kmod-pcache-invariants.md`](../kmod-pcache-invariants.md)、[`../design/daemon-kmod-messaging.md`](../design/daemon-kmod-messaging.md) |
| BPF (`bpf/`) | `struct_ops` の policy (LRU / SIEVE / S3-FIFO / W-TinyLFU、`pick_victim`)、fentry の観測 | [`../design/policy/`](../design/policy/)、[`../user/writing-policies.md`](../user/writing-policies.md) |
| daemon (`daemon/`) | S3 / GCS との往復、CEL policy、flush の順序保証、staging (小さいファイルの先読み) | [`../design/component-communication.md`](../design/component-communication.md) |
| 検証 | KUnit (kmod、UML)、go test (daemon)、実機の回帰 (`scripts/test-*.sh`)、TLA+ / Python モデル (`specs/`) | §3 |

## 2. 技術トピックの要点

- **bpf_arena を中身の置き場にしない**: kmod から arena の kern_vm を直接読む案は 6.12 で動かず (generic context の fault handler がない)、GUP 経由も page cache に負けた。今は arena は検証用のデモ (`make test-arena`) だけ。[`ebpf-kernel/arena-access-paths.md`](ebpf-kernel/arena-access-paths.md)
- **struct_ops の hot swap**: policy の差し替えは `BPF_LINK_UPDATE` 1 回で RCU のポインタを替える。kmod 側の glue (cfi_stubs / verifier_ops / update) が要る。[`ebpf-kernel/struct-ops-kmod-glue.md`](ebpf-kernel/struct-ops-kmod-glue.md)、データメンバは [`ebpf-kernel/struct-ops-data-members.md`](ebpf-kernel/struct-ops-data-members.md)
- **v2 再設計 (2026-11)**: 中身の置き場は chunk 表だけ、queue は `reqq` 1 本、完了は `DONE(ino, err)` 1 種に絞った。規則は [`../design/redesign-v2-simplify.md`](../design/redesign-v2-simplify.md)、実行できるモデルは [`../../specs/v2/`](../../specs/v2/README.md) (`make spec-check`)
- **順序と非同期は TLA+ で**: flush / rename / delete の順序 (AC-84〜86、96)、entry 表の tombstone、staging の鮮度は TLA+ の反例から見つけて実機 (`test-parity` など) で再現した。[`process/design-verification.md`](process/design-verification.md)、[`process/tlaplus-introduction.md`](process/tlaplus-introduction.md)
- **staging (AC-88)**: 小さいファイル 10,000 個の cold cat は、名前順に兄弟を先読みして 4 倍速くなった (8.6 s → 2.2 s)。外部更新は TTL 5 秒の間は古い写しを返しうる (AC-93)。[`../design/object-staging.md`](../design/object-staging.md)
- **先読みは自己クロックにする (AC-89)**: 窓を「miss ごとに満たす」では足りず、完了のたびに補充する形にして、容量 32 MiB の cold read が無制限に対して約 +4% まで縮んだ。[`../journal/benchmark-results.md`](../journal/benchmark-results.md) §18.62〜§18.63
- **計測のゆらぎは環境を疑う**: ある日の遅さの正体は VM の block デバイスの停止だった (§18.64、`scripts/disk-stall-probe.sh`)。MinIO と同居する bench は残った `mc admin trace` でも 25〜30% 遅くなる (AC-72)。
- **open の存在確認 (`open_check`)**: 既定は 1 (S3 に HEAD)。0 にすると entry 表だけで答え、速いが消えたファイルの open も通る。AC-79〜81 に頻度を下げる案。
- **カーネル版**: 最小は 6.12。`make kmod-matrix` で入っている全カーネルに通す。パッケージは `make deb` (公式相当にするには [`process/deb-packaging-official.md`](process/deb-packaging-official.md))。

## 3. 典型作業 (まず skill を呼ぶ。make のターゲットは下)

| やること | skill | 主な make |
|---|---|---|
| 新しい環境、build が通らない | `dev-env-setup` | `make doctor` / `make all` |
| kmod の単体テスト | `kmod-unit-test` | `make test-kunit KSRC=<linux>` / `make check-invariants` |
| daemon の単体テスト | `kmod-unit-test` | `make test-daemon` / `make test-daemon-cov` |
| kmod を変えて実機で | `kmod-change-verify` | `make mount` → `make test-regression` |
| 実機の回帰・検証 | `regression-test` | `make test-regression` / `make test-parity` / `make test-lockdep` |
| 性能試験の実行と記録 | `perf-bench-verify`、`journal-record` | `make bench*` |
| 遅さの原因 | `perf-root-cause` | perf / bpftrace |
| 設計の検証 | `tlc-design-verification` | `make test-spec` / `make spec-check` |
| uapi (構造体・ioctl) の変更 | `uapi-abi-change` | `make all` (sizeof の期待値も更新) |
| policy の開発 | `policy-develop` | `make test-policy-conformance` |

### root・実機なしで回せるもの (CI と同じ)

`make test` (= `test-daemon` + `check-invariants` + `spec-check` + `check-docs` + `test-boot-kernel`)。KUnit と TLC は別に `make test-kunit-cov` / `make test-spec` (CI の job は `.github/workflows/test.yml`)。実機の回帰は root と mount が要る。

## 4. 開発のコツ

1. **読む前に不変条件**: kmod の read / pcache / eviction を触るなら最初に [`../kmod-pcache-invariants.md`](../kmod-pcache-invariants.md) だけを読む。`vfs_ops.c` は全文を読まず `scripts/fn.sh <func>` で関数単位に読む。不変条件を変えたら同じ変更でそのメモも直す。
2. **修正前に FAIL するテストを作る**: 直す前のコミットの worktree で、テストが赤になるのを確かめてから直す (AC-48 / AC-50 / AC-53 / AC-64)。確かめられなかったときはその旨を書く。
3. **性能の変更は原因を計測で分離してから**: 推測で入れた変更は何度も外れた (AA-01 / AA-03 / AB-10 / large folio 仮説)。perf / bpftrace と A/B (交互測定) で見る。
4. **速度比を書く前に、双方の S3 への HEAD / GET と upcall の数を数える**。不自然な倍率は設計漏れを疑う (875 倍や write 17 倍は条件の違いが大半だった)。
5. **計測は条件を残す**: ビルドの commit、回数、`open_check`、キャッシュの有無を数値と一緒に。修正前ビルドやスクリプトのバグで無効になった数字は無効と書く。生データは `docs/journal/data/` に置き、`benchmark-results.md` の節から参照する。
6. **非同期・並行・持ち主が複数のものは、コードより先に状態遷移図と TLA+**。反例は `scripts/test-parity.sh` で実機に当てる (s3fs-fuse との差分テスト、既知の差は `scripts/lib/parity-known-diff.sh` に理由つき)。
7. **oops の後は rmmod できない**: 復旧手順は skill `kmod-change-verify`。bench の前後で `unmount` と MinIO の残りプロセスを確かめる (`minio_require`)。
8. **`pkill -f` を使わない** (自分のシェルも落ちる)。`pkill -x` か保存した PID。
9. **`go build` はルートか `daemon/` から**。`cmd/s3fs-ebpf` の中で打つと 60 MB のバイナリが出てコミットに混ざる。
10. **uapi を変えたら kmod と daemon を同時に**: sizeof の期待値と `docs/design/dev-s3fs-ebpf-protocol.md` も更新する (skill `uapi-abi-change`)。
11. **docs を動かしたら** `make check-docs` (相対リンクと `docs/...` の参照が壊れていないか) を回す。追加・移動は `docs/INDEX.md` にも同時に反映する。
12. **backlog は開いている項目だけを置く**。完了したら `docs/backlog/backlog-archive.md` に移す (ID と本文はそのまま)。

## 5. 次に着手するとき

- 開いている項目: [`../backlog/backlog.md`](../backlog/backlog.md) (AD: FUSE に対する優位性、R: 不足フック、AC-79〜81: open の確認と更新通知、AC-66 / AC-75: 計測待ち)
- 公式パッケージ化の条件付き保留: AC-98。社外に配ると決めたら着手する。
