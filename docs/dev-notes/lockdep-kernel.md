# lockdep 付きカーネルで回帰テストを通す (AC-42)

kmod のロック順 (`fd->lock` → `s3fs_admits_lock`、evict の mutex、spinlock 群。`docs/kmod-pcache-invariants.md`) は、これまで lockdep で確かめていない。Ubuntu の標準カーネルも、`make setup-kernel` が入れる kernel.ubuntu.com の mainline ビルドも `CONFIG_PROVE_LOCKING` が無効で、`/proc/lockdep_stats` が無い。そのため `test-pcache-evict-race` の lockdep 判定は SKIP のままだった。

issue #22 で、`hit_accum` が既定になり、`pick_victim` を持たない policy でも `admit_replace` が evict の mutex (`s3fs_policy_evict_lock`) を取るようになった。evict の mutex は `fd->lock` / `s3fs_admits_lock` より外側にある前提 (`kmod-pcache-invariants.md` の「ロック順」) で、`s3fs_hit_collect` と `s3fs_hit_forget` は mutex の中で `ilookup` / `iput` を呼ぶ。この順序を lockdep に見てもらう。

## 手順

```bash
# 0. docker / MinIO を起動し、s3fs_ebpf を一度 mount しておく (そのとき読み込まれている module だけを build する。LOCALMOD=1)
make compose-up && make all && make test-write-readback    # どれか 1 つ通れば足りる

# 1. lockdep 付きカーネルを build して入れる (LOCALMOD=1 で 20〜40 分、10〜15 GB。SRC_DIR に空きが要る)
sudo apt install build-essential flex bison bc libssl-dev libelf-dev rsync fakeroot dpkg-dev debhelper
make setup-kernel-lockdep          # = scripts/setup-kernel-lockdep.sh
#   PREPARE_ONLY=1 で config までで止めて .config を確認できる
#   DRY_RUN=1 で手順だけ表示

# 2. 再起動して新しいカーネルを選ぶ
scripts/boot-kernel.sh lockdep --once                # 一度だけ。GRUB_DEFAULT=saved が要る (下の「起動するカーネルを固定する」)
sudo reboot
uname -r                            # ...-s3fs-lockdep
ls /proc/lockdep_stats              # あれば lockdep 有効

# 3. 新しいカーネル用の headers / vmlinux / pahole (カーネルを替えるたびに必要)
make setup-toolchain
make doctor

# 4. 回帰テスト
make test-lockdep                   # = scripts/test-lockdep.sh
```

`make test-lockdep` は `make test-regression` を `hit_accum=1` (既定) と `0` で回し、各回の前に `dmesg -C` して、終わったら dmesg を見る。次のどれかがあれば FAIL。

- `make` が失敗した (回帰テスト自体の失敗)
- dmesg に lockdep の報告 (`possible circular locking`、`inconsistent lock state`、`possible recursive locking`、`bad unlock balance` ほか)、`BUG: sleeping function called from invalid context`、`suspicious RCU usage`、oops
- 終了時に `/proc/lockdep_stats` が `debug_locks: 0` (lockdep が自分で止まった)

結果は `/tmp/test-lockdep/` に残る (`run-<mode>.log`、`dmesg-<mode>.log`、報告の前後 30 行を画面にも出す)。最後の 1 行は `RESULT name=test-lockdep status=PASS|FAIL ...`。

主な環境変数:

| 変数 | 意味 |
|---|---|
| `MODES="1 0"` | `hit_accum` の値。既定は両方 |
| `TARGETS="test-regression"` | 回す make target。個別に絞れる (`TARGETS="test-pcache-evict-race test-pick-victim-stale"`) |
| `KUNIT=1` | 先に KUnit 付きの kmod (`KUNIT=1 make kmod`) をロードして、suite を lockdep の下で動かす。このカーネルは `CONFIG_KUNIT=y` にしてある。**この経路は書いただけで、まだ試していない** |
| `OUT=/tmp/test-lockdep` | 出力先 |

## 結果 (2026-10-28、6.12.0-s3fs-lockdep)

`make test-lockdep` が PASS した。

| 回 | 対象 | 結果 |
|---|---|---|
| `hit_accum=1` (既定) | `make test-regression` 20 件 (19 PASS + conformance は SKIP) | lockdep の報告 0 件、`debug_locks: 1` のまま終了 |
| `hit_accum=0` | 同上 | 同上 |
| 両方 | `make test-pcache-evict-race` (120 秒、32 reader + `/policy/eval` の連打) | `lockdep=PASS`、sha256_fail=0、read_err=0、evict 9,179 回 / 8,916 回 |

lock class は起動時 1,691 個から、kmod を載せて動かすと 1,842 個まで増えた (kmod の `fd->lock`、`s3fs_admits_lock`、evict の mutex、`s3fs_entries_mtx` などが登録された)。`kmod-pcache-invariants.md` のロック順 (evict の mutex → `fd->lock` → `s3fs_admits_lock`) に反する取り方は、この負荷の範囲では出なかった。

この結果の範囲: `make test-regression` と evict-race の 120 秒。長時間 (数時間) の soak、複数マウント、daemon の再起動中の mount/umount は回していない。

## 実際に踏んだこと

- `/proc/lockdep_stats` は **mode 0400 (root 専用)**。一般ユーザーの `[[ -r ]]` は「無い」と答える。`test-lockdep.sh` は `sudo test -e` / `sudo cat` で見る (最初の版は誤って「PROVE_LOCKING が無い」と言った)。
- 6.12 の `bindeb-pkg` が作る headers には `tools/bpf/resolve_btfids/resolve_btfids` が入らず、kmod のビルドが `resolve_btfids: not found` (BTF の段階で Error 127) になる。`setup-kernel-lockdep.sh` はインストール後に build tree からコピーし、`test-lockdep.sh` も無ければコピーする。
- `make test-policy-conformance` (live の hit 数を simulator と比べる) は、lockdep カーネルでは hit 数が 3.8〜5.7 pt ずれて FAIL する (通常のカーネルでは 0.2〜2.2 pt)。タイミングの検査なので、lockdep カーネルでは SKIP にした (`CONFORMANCE_ON_LOCKDEP=1` で実行できる)。

## 読み方

- lockdep は最初の報告のあとで自分を止める (`debug_locks: 0`)。1 回の起動で見つかるのは 1 件。直して再起動し、もう一度回す。
- 報告の `Possible unsafe locking scenario` と、スタックの `s3fs_ebpf_admit_replace` / `s3fs_policy_*` / `s3fs_hit_*` / `s3fs_ebpf_populate_pages` のどこが `fd->lock`、`s3fs_admits_lock`、`s3fs_policy_evict_mutex` を取っているかを見る。確認したい順序は「evict の mutex (外) → `fd->lock` → `s3fs_admits_lock` (内)」。逆順で取るパスが見つかったら、`kmod-pcache-invariants.md` のロック順を直すか、コードを直す。
- `ilookup` / `iput` を evict の mutex の中で呼ぶパス (`s3fs_hit_collect`、`s3fs_hit_forget`、`s3fs_ino_buf_bytes`) で、`iput` が `evict_inode` まで走って `s3fs_ebpf_evict_inode` が `s3fs_entries_mtx` を取る経路は、lockdep が一番疑う所。
- `blocked for more than`: T3 のデッドロック (folio lock と daemon の upcall の待ち合い) の症状。

## このカーネルで性能を測らない

lockdep (`PROVE_LOCKING`、`LOCK_STAT`、`DEBUG_*`) は mutex / spinlock を何倍も遅くする。`bench-*` の数字は通常のカーネルで取る。このカーネルで見るのは、報告が出ないことと、回帰テストの合否だけ。

## カーネルの config (`setup-kernel-lockdep.sh` が足すもの)

動いているカーネルの `/boot/config-$(uname -r)` を土台にして、次を足す。

| 設定 | 理由 |
|---|---|
| `PROVE_LOCKING`、`DEBUG_LOCKDEP`、`DEBUG_LOCK_ALLOC`、`LOCK_STAT` | lock 順序の検証 |
| `DEBUG_MUTEXES`、`DEBUG_SPINLOCK`、`DEBUG_RWSEMS`、`DEBUG_ATOMIC_SLEEP`、`PROVE_RCU`、`DEBUG_LIST`、`DEBUG_PREEMPT` | sleeping-in-atomic、RCU の誤用、リスト破壊 |
| `BOOTPARAM_HUNG_TASK_PANIC` 無効、`PANIC_ON_OOPS` 無効 | 報告が出てもマシンを止めない |
| `SYSTEM_TRUSTED_KEYS=""`、`SYSTEM_REVOCATION_KEYS=""` | 自前ビルドのツリーに distro の署名鍵は無い |
| `DEBUG_INFO_BTF`、`DEBUG_INFO_BTF_MODULES`、`BPF*`、`FUSE_FS`、`KPROBES`、`FPROBE` | kmod の kfunc と daemon、bpftrace |
| `KUNIT`、`KUNIT_DEFAULT_ENABLED` | `KUNIT=1` の kmod をその場で動かす |
| `LOCALVERSION=-s3fs-lockdep` | `uname -r` で見分ける。標準カーネルは消えない |

`olddefconfig` のあと `PROVE_LOCKING` / `DEBUG_LOCK_ALLOC` / `DEBUG_INFO_BTF` / `BPF_JIT` / `BPF_SYSCALL` / `KUNIT` が `y` でなければ、スクリプトは止まる (依存が外れている)。

## 起動するカーネルを固定する (GRUB_DEFAULT=saved)

Ubuntu の既定は `GRUB_DEFAULT=0` で、menu の先頭、つまり**一番新しいカーネル**で起動する。lockdep カーネルを入れると、それが以後の既定になってしまう (性能の測定を取り違える)。`GRUB_DEFAULT=saved` にして、起動するカーネルを明示的に選んで保存する。

```bash
# 一度だけ (開発機では 2026-10-28 に実施済み。元のファイルは /etc/default/grub.bak-before-saved-default)
sudo sed -i 's/^GRUB_DEFAULT=.*/GRUB_DEFAULT=saved/' /etc/default/grub && sudo update-grub

scripts/boot-kernel.sh                  # 一覧、保存されている既定、いま動いているカーネル
scripts/boot-kernel.sh generic          # 6.12.0-061200-generic を既定にする (性能の測定用)
scripts/boot-kernel.sh lockdep --once   # 次の 1 回だけ lockdep カーネルで起動 (既定は変わらない)
scripts/boot-kernel.sh lockdep          # lockdep カーネルを既定にする
```

- 新しいカーネルのパッケージを入れても、保存した既定は変わらない。
- `--once` は `grub-reboot` で、起動に使われたら消える。lockdep の検証は `--once` で足りる。再起動すれば標準のカーネルに戻る。
- grub の menu は `GRUB_TIMEOUT_STYLE=hidden` / `GRUB_TIMEOUT=0` で表示されない。対話で選びたいときは `/etc/default/grub` を `GRUB_TIMEOUT_STYLE=menu`、`GRUB_TIMEOUT=5` にして `sudo update-grub`。
- 保存された値は `sudo grub-editenv list` で見る (`saved_entry=gnulinux-advanced-<uuid>>gnulinux-<version>-advanced-<uuid>`)。

## つまずいたとき

| 症状 | 対処 |
|---|---|
| `pahole: error while loading shared libraries` で Kconfig が失敗する | Kconfig 自体が pahole を呼ぶ。スクリプトは `detect_pahole` で動くものを選び、`LD_LIBRARY_PATH` と `PAHOLE=` を付けて make する。`scripts/setup-toolchain.sh kbuild` で `$HOME/.local` に pahole を入れてから |
| BTF 生成で `Skipping BTF generation` / insmod で kfunc 登録に失敗 | pahole が古い。`make doctor` の C-4。再起動後に `make setup-toolchain` を忘れていないか (`$KDIR/vmlinux` が要る、H-2) |
| 起動しない / 署名エラー | Secure Boot が有効だと、署名のない自前カーネルは起動しない。BIOS で Secure Boot を切るか、MOK で署名する |
| `No space left on device` (`objcopy` / `btf_encoder__write_elf` / `Failed to encode BTF`) | 全 module (distro の config は約 6,400 個) を debug info 付きで build すると 40 GB 以上になる。スクリプトの既定は `LOCALMOD=1` (今 `lsmod` にある module だけ、手元の確認では 6,391 → 95 個)、debug info の zstd 圧縮、`-dbg` パッケージなし。それでも足りなければ `SRC_DIR=` を広い所に。途中まで進んだ build は、次の実行が `mrproper` と `rm -rf debian` から始めるのでそのまま再実行できる |
| `LOCALMOD=1` にしたら起動後に docker や network が動かない | build した時に読み込まれていなかった module が落ちている。docker と MinIO を起動し、s3fs_ebpf を一度 mount してから (`lsmod` に出てから) 再実行する。足りない module を足すなら `LMC_KEEP="drivers/net:net/netfilter"` のようにパスを渡す。全部入れるなら `LOCALMOD=0` |
| `make kmod` が `warning: the compiler differs` | 自前ビルドと同じ gcc を使う限り無害 (C-3) |
| 元のカーネルに戻したい | grub で標準カーネルを選ぶ。不要なら `sudo apt remove linux-image-<ver>-s3fs-lockdep linux-headers-<ver>-s3fs-lockdep`。`grub-reboot` は一度きりなので、次の再起動で標準に戻る |

## 結果の記録先

通ったときも、見つかったときも、`docs/design/hit-signal-percpu-accumulation.md` §9 の lockdep の項と、`docs/journal/issue22-hit-accum-investigation.md` §8 を更新する。見つかったロック順の修正は `docs/kmod-pcache-invariants.md` の「ロック順」に反映する。

## 関連

- `scripts/setup-kernel-lockdep.sh`、`scripts/test-lockdep.sh`、`scripts/test-pcache-evict-race.sh` (lockdep の判定をこのテストも持つ)
- `docs/dev-notes/build-env-dependencies.md` (カーネル入替後の H / C / T の手当て)
- `docs/dev-notes/kunit.md` (UML での KUnit。KASAN 付きは通常の `make test-kunit`)
