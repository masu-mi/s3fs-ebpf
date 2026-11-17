# .deb を「公式相当」にするために足りないもの

状態: **計画なし** (バックログ AC-98、条件付き)。この文書は、将来 Debian / Ubuntu の公式アーカイブ、PPA、または社外に配る apt リポジトリへ進むと決めたときに、何から手を付けるかを残すためのもの。

## 今あるもの (`make deb` / `deb-check` / `deb-install`、`scripts/build-deb.sh`)

- 手元・社内に配るための .deb: `s3fs-ebpf` (デーモン・bpf・policies・man)、`s3fs-ebpf-doc`、カーネルごとの `s3fs-ebpf-kmod-<krel>`。
- `dpkg-deb` + `fakeroot` で組み立てる 1 本のシェルスクリプト。`debhelper` も `debian/` も使っていない (選んだ理由は比較検討ではなく、手元で動く最短の方法だったから)。
- 検査は `deb-check` (中身・vermagic・BTF・`modules.dep`・`apt-get install -s`・man の groff 警告) と `kmod-matrix`。**lintian と policy 適合は検査していない**。

以下の「必要作業」は、Debian Policy / Developer's Reference の私の知識に基づく見立てで、**各項目は着手時に最新の文書で確かめること** (この文書を書いた時点で policy 本文は読み直していない)。

## 1. 先に決めること (ブロッカー)

| 項目 | 現状 | 必要なこと |
|---|---|---|
| ライセンス | リポジトリに `LICENSE` が無い。ソースの SPDX は `GPL-2.0`、`MODULE_LICENSE("GPL")` (kmod) | 全体のライセンスを決めて `LICENSE` を置く。kmod と、kfunc / struct_ops を使う bpf プログラムは GPL が必要 (GPL 互換でないと kfunc を呼べない / ロードできない)。デーモン側 (Go) は別ライセンスにもできるが、混在するなら境界を明記 |
| 依存ライブラリのライセンス | `daemon/go.sum` に依存が約 200 行。静的リンクされる | 全依存のライセンスを洗い出し、`debian/copyright` に載せる (`go-licenses` 等で自動化) |
| 対象ディストリ | kmod は 6.12 以上 (`kmod/s3fs_ebpf_dev.c` の `#error`)。Ubuntu 24.04 の標準は 6.8、6.12 は mainline ビルドのみ | 公式アーカイブの対象は、標準が 6.12 以上のもの (Debian trixie など) に限られる。noble 向けは PPA でも、カーネル自体を別に入れさせる前提になる |
| 配布経路 | 無し | (a) Debian / Ubuntu 公式、(b) PPA (Launchpad、ソースパッケージ必須)、(c) 自前の apt リポジトリ (reprepro / aptly + GPG 署名)。(c) なら下の 2 の一部は不要 |

## 2. ソースパッケージとビルド

- `debian/` を作る: `control` / `rules` / `changelog` / `copyright` (DEP-5) / `source/format` (`3.0 (quilt)`) / `debhelper-compat`。今のスクリプトの中身は `dh_install` / `dh_installman` / `dh_installdocs` / `dh_compress` / `dh_md5sums` / `dh_gencontrol` に置き換える。
- **クリーンな環境でのビルド** (sbuild / pbuilder): 今は手元にビルド済みの `bin/` と `bpf/*.o` がある前提。ビルド依存を `Build-Depends` に書く: clang >= 19、`llvm`、`libbpf-dev`、Go >= `daemon/go.mod` の版 (1.25)、`dwarves` (pahole)、`bpftool`。**distro の標準 clang が 19 未満の場合は満たせない** (noble は 18 が標準)。
- **ネットワーク無しでビルド**: 公式のビルダーは外に出られない。Go の依存は `vendor/` を同梱するか、`golang-*-dev` パッケージに依存する (`dh-golang`)。今は `go build` が module を取りに行く。
- **コミット済みの生成物**: bpf2go の生成物 (`daemon/internal/*/*_bpf{el,eb}.o`) がリポジトリに入っている。公式は「修正に適した形式 (ソース) からビルド」を求めるので、`go generate` をビルド中に走らせて作り直す形にする (clang 19 以上が必要。`arena_cast_test.go` が古い clang の生成物を検出する)。
- 再現可能ビルド (`SOURCE_DATE_EPOCH`、`-trimpath`、`-buildid` の固定)。
- 実バージョン管理: 今は `0.0.0+git<日付>.<sha>`。タグ付けと `debian/changelog` が要る。

## 3. kmod の配り方

今は「カーネルごとにビルド済みの `s3fs-ebpf-kmod-<krel>`」。公式・長期運用ではこれが続かない。

- **DKMS** (`s3fs-ebpf-dkms`、`dkms.conf`) にすると、新しいカーネルが入るたびに利用者の機械でビルドする。このとき、利用者の機械に **gcc / pahole / `$KDIR/vmlinux`** が要る (`doctor` の C-3 / C-4 / H-2)。vmlinux が無い headers (Ubuntu mainline など) では module BTF が空になり、insmod が失敗する。`dkms.conf` の PRE_BUILD で検査し、足りなければ明示的に失敗させる設計が要る。
- ビルド済みを続けるなら、カーネルの ABI 更新のたびに全 `s3fs-ebpf-kmod-<krel>` を作り直して配る仕組み (CI のマトリクス) が要る。今の `kmod-matrix` が土台になる。
- `Depends: linux-image-<krel>` が無い (mainline カーネルはパッケージ名が違うため意図的に外した)。公式では対応するカーネルパッケージへの依存を付ける。
- **Secure Boot / lockdown**: 未署名のモジュールは Secure Boot 下でロードできない。MOK への登録、DKMS の署名鍵の扱いを決める。
- 置き場所: 今は `/lib/modules/<krel>/updates/`。公式では `/lib/modules/<krel>/extra` や `updates/dkms` の扱いを確認する。`depmod` の呼び出しは `postinst` / `postrm` にあるが、`modules-load.d`・`/etc/modprobe.d` は未整備。

## 4. パッケージの中身

- `md5sums` (今は作っていない) 、`conffiles`、strip / デバッグシンボル (`-dbgsym`)、ハードニング、`Depends` の自動生成 (`${misc:Depends}`)。Go は静的なので shlibs は少ない。
- bpf オブジェクトの置き場: 今は `/usr/share/s3fs-ebpf/bpf` (arch 非依存扱い)。bpf のバイトコードはエンディアン依存で、アーキテクチャ固有のデータなので `/usr/lib/<triplet>/s3fs-ebpf/` が正しい可能性が高い (policy で確認)。デーモンの探索パス (`resolveShareRoot`) も合わせて変える。
- systemd unit: デーモンは手動起動 (`fastpath`、`mount`)。サービスにするなら unit・`/etc/default`・設定ファイルが要る。`postinst` での自動起動はしない方針を決める。
- `policies/` の例は `/usr/share/doc/<pkg>/examples/` が慣例。
- `s3fs-ebpf-doc`: `copyright` の整備、Markdown のまま (`doc-base` 登録は任意)。`docs/journal` は入れない方針を維持。
- man ページ: 今は `--help` からの生成。公式では、生成物ではなく、ソースに入れた roff か、ビルド中に生成する形にする。各コマンドの説明の質は `--help` 止まり。
- **lintian をクリーンにする** (警告を 0 にするか、override に理由を書く)。piuparts (インストール・削除・アップグレードの検査) も通す。

## 5. 検査 (公式のレベル)

- 今の `deb-check` は、パッケージを展開して見る静的な検査だけ。実際のインストール (`deb-install`) は手元で実行していない。
- autopkgtest: insmod・mount・MinIO・root が要るので、公式の autopkgtest 環境 (コンテナ / qemu) では実行できない可能性が高い。「インストールできてバイナリが動く」程度の `superficial` テストに留めるか、`Restrictions: needs-root, isolation-machine` で VM を要求する。
- マルチアーキテクチャ: 今は amd64 のみ検証。arm64 などは kmod のビルドも bpf の `-mcpu=v3` も未検証。`Architecture:` を絞る。
- CI: コンテナ (対象ディストリ) でのビルド、lintian、piuparts、`kmod-matrix` を GitHub Actions に載せる。runner には複数カーネルが無いので、kmod は headers だけで build できる範囲に限る (実機 insmod の試験は CI に載らない)。

## 6. 着手するときの順序 (案)

1. `LICENSE` と依存ライセンスの洗い出し (1 の表)。
2. 配布経路を決める。自前の apt リポジトリなら 2〜5 のうち必要な分だけ、公式 / PPA なら全部。
3. `debian/` を作り、クリーンビルド (vendor 化、`go generate` のビルド内実行) を通す。
4. kmod を DKMS にするか、ビルド済みのマトリクスで続けるかを決めて実装。
5. lintian / piuparts をクリーンにし、CI に載せる。
6. 署名鍵とリポジトリ (reprepro など) を用意して公開。

関連: `docs/dev-notes/build-env-dependencies.md` (「実行環境向け apt パッケージ」節)、`scripts/build-deb.sh`、`scripts/kmod-matrix.sh`。
