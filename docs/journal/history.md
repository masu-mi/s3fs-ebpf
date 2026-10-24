# 開発史: s3fs-ebpf の経緯・課題・トラブル

この文書は、git 履歴 (`git log`、2026-10-01〜2026-10-18 の 138 commit)、`docs/journal/backlog.md`、`docs/journal/benchmark-results.md` (以下 `br`)、週次ノート (`docs/journal/week*-notes.md`)、`docs/dev-notes/lessons-learned.md` (以下 `ll`) をもとに、プロジェクトの流れをまとめたものです。数値には出典 (`br §N`、backlog ID、commit) を付けています。出典に書かれていないことは書いていません。

注意: Week 1〜4 の作業 (2026-09) はこのリポジトリの git 履歴に含まれていません。最初の commit は 2026-10-01 で、backlog が参照する古い commit (`55dbc7d`、`d7efccc` など) はこの履歴にはありません。

---

## 1. 概要

s3fs-ebpf は、S3 (と GCS) のバケットをローカルのファイルシステムとして見せる仕組みです。カーネルモジュール (kmod) が VFS を実装し、BPF プログラムが fast path (`fentry/vfs_getattr_nosec`、`fentry/generic_file_read_iter`) とキャッシュ policy (struct_ops の vtable と KFunc) を担い、Go の daemon が S3 との通信、bucket の同期、prewarm、write-back を行います。

当初は「bpf_arena にファイルの中身を置き、kmod から GUP で読む」設計で、FUSE 比の大きな倍率 (open+read+close で fuse-cache の約 169 倍、`br §7.5`) を出しました。その後、64 並列 read の劣後を解消しようとした AA 系の改修が失敗し (2026-10-13)、arena をメタデータだけに縮退して中身を Linux の page cache に戻す pcache モード (AB 系) に移りました。pcache モードでは populate と eviction の race によるデータ破損が続けて見つかり、AC 系 (AC-01〜AC-65) で正しさを固め、最後に write の部分変更 (AC-50/AC-51) と policy の `pick_victim` (AC-58) まで進んでいます。

---

## 2. 年表

### 2.1 Week 1〜4 (2026-09、git 履歴より前)

- **Week 1 (ビルド疎通)**: Ubuntu 24.04 + mainline 6.12 で `make test-arena` を通すまでに 5 つの環境課題を踏みました。壊れた apt パッケージ、kernel header が `gcc-15` を要求、noble の pahole 1.25 が module BTF を空で出力、linux-headers に vmlinux ELF がない、6.11 以降の `register_btf_kfunc_id_set` が `BTF_KFUNCS_START` を要求 (-22)、の 5 つです (`week1-build-runtime-notes.md`)。
- **Week 2 (verifier とツールチェーン)**: cilium/ebpf の `Marshal` が `bpf_addr_space_cast` の Offset を 0 に潰すバグを vendor + パッチで回避、verifier が arena map の関連付けと `user_vm_start` の先設定を要求する点、`fentry/vfs_getattr` が発火せず `vfs_getattr_nosec` に変えた点、clang 18 では addr_space_cast が使えず clang-20 が必要な点などを記録しています (`week2-implementation-notes.md`)。
- **Week 3 (VFS の materialise)**: kmod の lookup / iterate_shared / read / write / truncate を実装し、daemon が起動時に全 ino を prewarm する形にしました。S3 への write-back と arena による中身の保持はまだでした (`week3-implementation-notes.md §0`)。
- **Week 4 (a) read path**: kmod から arena の kern_vm を直接 deref すると全て EFAULT になり、Path A′ を断念しました (`week4a §3`、`ll §1.1`)。代わりに daemon の arena user_vm を `pin_user_pages_remote` で pin して読む Path B (GUP) に切り替え、read miss を daemon に upcall する Path C も実装しました。prewarm 済みファイルの cat は約 1 ms、未 admit の初回は約 30 ms (ほぼ S3 の RTT) でした (`week4a §10.6`)。
- **Week 4 (b) bucket sync**: `SYNC_ENTRIES` ioctl による動的な bucket 同期。`--sync-every=3s` で put / rm が約 3 秒以内に反映されました (`week4b §3`)。
- **Week 4 (c) write-back**: close / fsync で daemon が PUT する経路、`touch`、unlink / rename、flush の coalescing (E-01〜E-03) を入れました。
- **この時期の代表値** (`br §0`、2026-09-22〜25): cat 752 µs 対 s3fs-fuse 6.12 ms (8.1 倍)、read syscall p50 5.20 µs 対 4.55 ms、1 MiB cat で約 38 倍、write は 100 MiB で 17 倍。ただし後のレビュー (`br §0.1`、2026-10-05) で、875 倍は「キャッシュ有り ebpf 対キャッシュ無し fuse」であり、write 17 倍は durability がそろっていないと注記されました。

### 2.2 ビルド環境の整理 (2026-10-03〜10-04)

- `make doctor` と toolchain 検出の集約 (`97f8c07`)、bench の lifecycle 共通化 (`7db5c16`)、BTF のない .ko の扱い (`ed24511`、`0ab2ae3`)、go.mod を満たす go の選択 (`29a3219`、`2cbc43d`)、clang-20 で BPF object を再生成 (`53649c6`)、rmmod の retry (`632cc95`) など。6.12 での検証記録は `10f0ab0`。

### 2.3 arena モードの正しさと計測の整備 (2026-10-05〜10-12)

- **S 系 (キャッシュと eviction の正しさ、10-05〜10-07)**: softcap (8) は件数による投票の発動条件にすぎず容量ではなかったため、hit-rate bench の W-TinyLFU 88.5% が容量 8 の Belady 上限 61〜65% を超えていたことが発端です (backlog S 節)。差分 invalidate (S-01、`f33af1f`)、evict log の drain (S-04、`66c932d`)、バイト容量による eviction (S-02、`96145db`)、page cache hit の policy への供給と disk cache 上限 (S-03 / S-07、`e7a0124`)、fsync の約 90 ms の固定遅延の解消 (S-08、`cbcc66e`)、nocache の invalidate (S-09、`804e12c`)。S-06 で実機検証 (`37fad35`)。
- **T 系 (コードレビュー、10-06〜10-07)**: arena チャンクのリーク (T-01、T-04 `611c023`)、arena の offset を uint64 に (T-05 `d861941`) など。
- **Q / V / W 系 (2026-10-10)**: エンドポイント認証、`builtin:` による allowlist バイパスの塞ぎ込み、ファイルのパーミッション、goroutine 上限などを `3c0975d` でまとめて入れました。
- **U / X 系 (2026-10-11)**: bench の妥当性 (U-01〜U-08) と policy の意味論 (X-01〜X-07) を `3313952` で修正。同日の同一 run の再計測で、ebpf-nocache 対 fuse-raw が 3.2 倍、ebpf-A+C 対 fuse-cache が約 169 倍 (519 µs / 3.075 µs、`br §7.5`、`6cdc886`)。S-08 後の write は、durable な ebpf-fsync が fuse-raw より 1.28 倍 (1 MiB)〜1.72 倍 (100 MiB) 速くなりました (`br §8.3`、`6a8a209`)。
- **Y / Z 系 (2026-10-12)**: hit-rate bench の指標の壊れ (Y-01 など) とポリシーの時定数のハードコード (Z-01〜Z-05) を `28a716f` で修正。

### 2.4 AA 系: cold-path 改善の失敗 (2026-10-12〜10-13)

- 狙い: 64 並列 read で ebpf が fuse-raw に 1.1〜1.2 倍劣後している点 (AA-01) と、microbench p50 を 2 µs 以下にする点 (AA-03) の解消。
- 実装: admit hash の spinlock を rwlock に (AA-01)、arena から iov_iter への直接コピーをする `.read_iter` を kmod に実装 (AA-03)、pattern-aware admit の既定 on (AA-02)、seq prefetch (AA-04)、disk cache の watermark (AA-05)、mount-s3 を bench に追加 (AA-06)。commit は `28a716f`、`3b9bd79`。
- 結果 (`br §12`): 64 jobs の read は ebpf 4045k 対 fuse-raw 5289k で 76% に留まり、commit `563c53b` の記述では 87% から 76% に悪化しました。microbench p50 は 3.2 µs で目標の 2 µs に届きませんでした (`br §12.1`)。backlog は「GUP + arena サイズ (128 MB) の構造的 bottleneck」と判断しています。
- これを受けて Meetup のアウトラインを「bpf_arena / struct_ops の使い方 + 失敗談」に組み替えました (`563c53b`)。

### 2.5 AB 系: page cache への回帰 (2026-10-13)

- **Phase 1 (AB-01/02)**: GUP 1 回あたり 1728.5 ns (閾値 500 ns 超え)、1 MB ファイルでは page cache 経路 5.341 ms が arena 経路 5.559 ms より速い、という結果で Gate PASS (`br §13.3`)。
- **Phase 2 (AB-04〜06)**: `--arena-metadata-only` (pcache モード) を実装。arena はメタデータだけを持ち、中身は `fd->buf` から page cache に populate します (`ecf3990`)。
- **Phase 3 (AB-07〜10)**: gate (b) 大ファイル (pcache 8.047 ms 対 fuse-cache 8.489 ms) と (c) microbench p50 (3.324 µs 対 3.368 µs) は PASS、(a) 64 jobs IOPS は 3879k 対 6061k (63.9%) で FAIL (`br §14.4`)。rwlock が原因と見て per-file admit cache (AB-10) を入れましたが、64 jobs は 3800〜3970k のまま改善しませんでした (`br §15`)。
- AB-08 では pcache モードの hit rate が 94.35〜94.60% と出ましたが、後に Q1 の修正前ビルドでの計測として無効になりました (4.4 節)。AB-03 (mount-s3 を含む 4-way 比較) は未実施のまま残っています。

### 2.6 AC 系前半: pcache モードの正しさ (2026-10-13〜10-15)

- AB の実装レビューから P1〜P6、Q1〜Q4、R1〜R3、S1〜S11、T1〜T6、U1〜U7 と段階的に問題が見つかり、AC-01〜AC-34 として追跡しました。
- 主な修正: pcache の範囲検査とオーバーフロー防止 (AC-01/02、`98a85bf`)、重複と容量 (AC-03、`a53d229`)、メモリ順序 (AC-04、`c1aafc2`)、reset の `i_size_write(0)` と evict race (AC-07/08、`002a039`)、4 GiB 上限 (AC-09、`1e1b85b`)、populate/evict race (AC-11 / R1、`070ff01`)、check と fill の間の TOCTOU (AC-14 / S1、`315b182`)、multi-chunk と eviction (AC-17〜21、`2417907`)、retry_exhausted カウンタと bitmap (AC-25/26、`22ed44b`)、256 MiB 超の読み出し (AC-23 / T2、`babaa5e`、`e56f6c7`)。
- テスト側: 全テストスクリプトに機械可読の RESULT 行 (`b63b2b5`)、`make test-regression` (`95b8e28`)。
- 作業コストの対策として、`scripts/fn.sh` と `docs/kmod-pcache-invariants.md` を追加しました (`10b5122`)。

### 2.7 AC 系中盤: 書き込み消失、メモリ、フック (2026-10-16)

- 計測準備中に見つかった W1 (flush PUT 前の inode evict で書き込み消失、`694d2a1`)、P1 (pending の hash key、`5bf11a3`)、W2 (create したファイルが evict 後に 0 byte、`7ed6abe`) を修正し、`7ed6abe` で §18.1〜18.8 を計測しました (`3088d55`)。正しさの gate は全 PASS、性能の AB-07 gate は (a) 0.66 倍、(b) 1.20 倍、(c) 0.977 倍で FAIL でした (`br §18.9`)。
- メモリ: `fd->buf` が capacity の 10 倍 (40 MiB 対 4 MiB) まで膨らむことを確認し (`br §18.10`)、容量判定を fd->cap で行う修正 (AC-41、`9c5cfdb`)、populate の再確保を倍々にする修正 (AC-46、`e76e660`。300 MiB の cold read 2.33 s から 0.78 s、`br §18.14`)、fd を 4 MiB chunk の表にして chunk 単位で pin / shed する改修 (AC-45 C1〜C3、`fc7fa45`、`78e60ba`、`4ab4103`。40 MiB の順次読みでピーク 40 MiB から 12 MiB、`br §18.21`) を入れました。
- フック: 64 jobs の差の正体が `fentry/generic_file_read_iter` (perf で約 19%) だと分かり (`br §18.16`)、直接参照 (`3eb6f7b`)、per-CPU の pattern map と slot 配列 (AC-47、`07e3bc2`、`8e6bfee`) で比を 0.81 から 0.94〜0.96 まで上げました。フックの固定コストで 0.974 が上限のため、gate (a) を「フックなしの pcache ≥ 0.98 × fuse-raw」と `hook_ratio` に変更しました (`br §18.24`、`9442716`)。
- その他: prewarm が空のとき upcall が飛ばない (AC-43、`cd0dea3`)、pcache モードの prewarm をメタデータだけに (AC-44、`364aa0c`)、arena の chunk admit で i_size が縮む (AC-48、`c6fd92c`)、chunk admit が他の chunk を落とす (AC-49、`a41c910`)。

### 2.8 AC-50〜65: 既存オブジェクトへの write と部分変更 (2026-10-16〜10-17)

- AC-50: 一度も読まずに既存ファイルへ write / truncate すると、読んでいない範囲が 0 のまま PUT される問題を修正しました (`52c720e`、`a14204c`)。最初の変更の前に残る部分を読み込みます。
- AC-50 の代償として、既存の 100 MiB の途中に 4 KiB 書く操作が fuse-raw の 2.46 倍遅くなりました (`br §18.29`)。AC-51 で変更のない chunk を ETag 付きの remote として記録し、flush を multipart + `UploadPartCopy` にしました (`36884f7`、`34b233e`、`b48b217`、`1ee2eed`)。`--partial-modify` (既定 off、S3 のみ) で 100 MiB は 179.2 ms 対 fuse-raw 175.4 ms とほぼ同じになりました (`br §18.36`)。
- AC-52: 大きいファイルの cold read (300 MiB で fuse-raw の 0.34 倍、`br §18.30`) を、cache-dir の write-behind (`e6cbcb6`)、chunk 単位の並列 load (`4dd473c`)、worker-local の get+populate と 16 MiB range GET (`8ef0511`) で改善し、300 MiB は中央値 0.80 倍になりました (`br §18.37`)。残りの差はホストのメモリ回収で、毎回 drop_caches をそろえると 300 MiB で 1.26 倍速いことが分かりました (`br §18.39`)。
- AC-55: SYNC_ENTRIES に hash と整列配列を入れ、50k 件の `ls -l` cold が 15,075 ms から 212 ms になりました (`br §18.40`、`7af6d9f`)。
- AC-56/57 (`e0c55e7`)、レビュー指摘の AC-59〜65 (`e53779c`、`f3d05f3`、`4329711`)、disk cache の ETag 検証 (AC-40、`4329711`)。

### 2.9 AC-58: pick_victim (2026-10-17)

- policy の vtable に `pick_victim` / `on_insert` / `on_remove` を足し、victim の選択を BPF に渡しました (`2204b81`、`b9b9d86`)。
- 結果 (`br §18.41`、`§18.42`): `sieve_pick_ops` は 3 seed とも LRU より 4.3〜4.9 ポイント、旧 sieve_ops より 5.0〜5.7 ポイント高く、`s3fifo_pick_ops` は 36.7〜40.0% でした。read 経路のコストは 64 jobs で policy なしの −4.6% (旧 SIEVE は −14.8%)。trace-replay の値 (SIEVE 43.8〜48.0%) には届いていません。

### 2.10 §18 の再計測の流れ (2026-10-16〜10-18)

| 時点 | ビルド | 主な結果 |
|---|---|---|
| §18.1〜18.9 (10-16) | `7ed6abe` | 正しさは全 PASS、AB-07 gate は a 0.66 / b 1.20 / c 0.977 で FAIL |
| §18.16〜18.24 (10-16) | フック改修 | フックなしで fuse-raw 比 1.000〜1.024、`hook_ratio` 0.95〜0.965 |
| §18.26〜18.32 (10-17) | `a14204c` + `1c0a0d5` ほか | pcache モードで open+read+close が fuse-cache 比 172 倍、小ファイル 10,000 個の warm cat が 93 倍。大ファイルの cold read と途中書き換えは負け |
| §18.43 (10-17) | `4329711` | AB-07 gate (a)(b)(c) すべて PASS (a は 5759k 対 5857k、`hook_ratio` 0.983)、hit rate は opt 以下、test-regression 18 件 PASS |

AC-42 (lockdep) は、`CONFIG_PROVE_LOCKING` 付きのカーネルがないため実行せず、静的なロック順の確認だけで 2026-10-18 に閉じました (`5a34cfb`)。

---

## 3. 課題 (backlog)

backlog の ID 系列ごとの内容と件数です。件数は backlog のチェック項目 (AC は表の行) を数えたもので、サブ項目を別に数えている系列があります。

| 系列 | 内容 | 完了 | 未完了 |
|---|---|---:|---:|
| A | FS 機能の小修正 (exit 時の SYNC clear、flush 失敗時の dirty 復帰など) | 4 | 0 |
| B | bench / 環境の使い勝手 | 5 | 0 |
| C | Grafana / metric | 4 | 0 |
| D | 大ファイル / 部分 read (range admit、partial GET、arena free-list) | 3 | 0 |
| E | write path (blocking fsync、unlink / rename、coalescing) | 3 | 0 |
| F | dynamic sync の強化 | 3 | 0 |
| G | VFS 拡張 (subdir、setattr) | 2 | 0 |
| H | lifecycle / 信頼性 (ino の永続化、arena gc、shutdown race) | 6 | 0 |
| P | policy engine (CEL、struct_ops、KFunc、各 policy、hit-rate bench) | 24 | 0 |
| I | bench の追試 (mount-s3、drop_caches=1 など) | 5 | 0 |
| J | metric / observability | 6 | 0 |
| R | R-01 read fentry の評価 | 2 | 0 |
| K | upstream 貢献 (arena の export、fault handler) | 0 | 2 |
| L | narrative / docs | 3 | 0 |
| S | キャッシュ / eviction の正しさ (2026-10-05 レビュー) | 9 | 0 |
| T | コードレビュー起票 (2026-10-06) | 5 | 0 |
| Q | セキュリティ / コード品質 (2026-10-10) | 9 | 0 |
| V / V2 | 性能改善 (2026-10-10) | 7 | 0 |
| W | リファクタ (2026-10-10) | 3 | 0 |
| U | bench 計測の妥当性 (2026-10-11) | 8 | 0 |
| X | policy 実装の意味論 (2026-10-11) | 7 | 0 |
| Y | bench 計測の妥当性 Phase 2 (2026-10-12) | 7 | 0 |
| Z | policy の時定数のハードコード (2026-10-12) | 5 | 0 |
| AA | FUSE 比の cold-path 改善 (2026-10-12) | 6 | 0 |
| AB | arena の metadata-only 化 (2026-10-13) | 9 | 1 |
| AC | pcache モードの修正と以降の改修 (2026-10-13〜) | 64 | 1 |

注: AA 系は全項目に完了の印が付いていますが、受け入れ条件 (64 jobs で ebpf ≥ fuse-raw、p50 ≤ 2 µs) は満たしていません (2.4 節)。

### 3.1 まだ開いている項目

- **AB-03**: mount-s3 を含む 4-way の cold read 比較。Phase 1 の Gate を通ったため「省略可、必要に応じて実施」とされています。
- **K-01**: `bpf_arena_get_kern_vm_start` の EXPORT_SYMBOL_GPL の提案。
- **K-02**: arena の kern_vm 範囲に generic context の fault handler を足す提案。入れば Path A′ が復活し GUP の負担が消える見込みです。
- **AC-06**: 64 jobs の律速の特定 (perf c2c / lockstat)。`br §18.16` で fentry が原因と分かり、`br §18.43` で gate (a) は PASS していますが、表の状態は「未着手」のままです。

### 3.2 完了扱いだが残りが書かれている項目

- AC-41: 将来案 (chunk 単位の確保) は未着手と記載。
- AC-42: lockdep は未実行 (静的確認で閉じた)。
- AC-51: 10 MiB の途中書き換えはまだ 1.6 倍遅い。しきい値 (`partial_modify_min` 16 MiB) を下げた計測と、実 S3 での計測は未実施 (`br §18.36`)。
- AC-52: 300 MiB の cold read は中央値 0.80 倍で目標 0.9 倍に未達 (`br §18.37`)。メモリ回収の内訳と ext4 との比較は未確認 (`br §18.39.1`)。
- AC-58: 候補が数千のときの `pick_victim` の時間、W-TinyLFU の移行、アプリのヒントが残り。

---

## 4. トラブルの歴史

### 4.1 データ破損

**AC-50: 既存ファイルの未読部分が 0 で PUT される**
- 症状: S3 にあるファイルを一度も読まずに途中へ write / truncate すると、上書きは書いた範囲まで切り詰められ、truncate による縮小・拡張は 0 になり、`: > f` は S3 に届きませんでした。新しい `test-write-existing` は修正前に 12 シナリオ中 10 件 FAIL でした (`br §18.22`)。
- 原因: open が中身を読み込まず、flush は fd の全体を PUT していたためです。
- 修正: `52c720e` で、clean から dirty への最初の変更の前に残る部分を FOR_WRITE の upcall で読み込むようにしました。続いて、load と `fd->lock` の間に shed / reset が入ると読み込んだ chunk が解放され 0 が flush される race が見つかり、`a14204c` で dirty になるまで pin を保持するようにしました。この修正のテストが修正前のコードで FAIL することは AC-53 で確認しています。
- 教訓: 「open で読み込まない」という read 側の最適化が、write 側の前提 (fd が全体を持つ) を崩していました。

**F1: 新規ファイルの fsync 後に S3 のオブジェクトが 0 byte になる**
- 症状: 新規ファイルに write + fsync すると、あとで S3 のオブジェクトが 0 byte になりました (`br §18.26` の 1、HIGH)。
- 原因: `.create` が空の flush を通常の queue に積み、fsync の WAIT event は即座に処理されるため、古い 0 byte の PUT が後から着地していました。
- 修正: `1c0a0d5` で、WAIT の enqueue がその ino の通常 queue の項目を捨てるようにし、`test-fsync-new-file` を追加しました。
- 発見の経緯: 計測スクリプトが daemon に `--bucket` を渡し忘れていたことから見つかりました (4.4 節)。最初の durable write の数字は 0 byte の PUT を含んでおり無効です。

**AC-48: arena の chunk admit でファイルサイズが縮む**
- 症状: arena モードで 10 MiB のオブジェクトを 4 MiB chunk で admit すると、最初の 4 KiB が 0 byte で返り、全体読みは exit 0 のまま 4 MiB で終わり、`stat` も 4 MiB でした (`br §18.18`)。
- 原因: `admit_replace` が entry の `size` (range の長さ) で `i_size_write` していました。全体 admit を前提とした古いコードが、D-01 の range 化で崩れていたものです。W2 の修正で、誤ったサイズが evict 時に entry へ書き戻される状態でもありました。
- 修正: `c6fd92c`。i_size は変わった range の終端まで伸ばすだけで縮めません。同時の truncate を巻き戻す窓は AC-56 (`e0c55e7`) で閉じました。
- 教訓: §18.17 で BAD と出ていた結果の正体はこれで、`10m.bin` の中身が 0 だったためデータではなくサイズで不一致になっていました。

**Q1 / AC-07: evict 後に stat と read が 0 になる**
- 症状: evict されたファイルの `stat` が 0 を返し、read が EOF になりました (arena / pcache の両モード)。
- 原因: `s3fs_file_data_reset` が `i_size_write(inode, 0)` を呼んでいました。
- 修正: `002a039`。この修正前のビルドで取った §16 / §17 の数値は無効になりました (4.4 節)。

**W1 / W2: 書き込み消失と空ファイル**
- W1 (AC-35): close 後、flush PUT の前に drop_caches や reclaim で inode が evict されると、未 PUT の `fd->buf` が捨てられ書き込みが消えていました。`694d2a1` で flush 完了まで `ihold` で pin します。
- W2 (AC-37): `.create` したファイルの inode が flush 後に evict されると、再 lookup で i_size が 0 になり空ファイルとして読めていました。`7ed6abe` で evict 時に i_size を entry へ書き戻します。

**AC-40: disk cache が古い body を返す**
- daemon の disk cache が size / ETag を検証せず、上書きされたオブジェクトの古い body (10 MiB 版) を返して -EIO になっていました。`4329711` で cache ファイルに ETag と size の header を持たせ、食い違えば miss にしました。

### 4.2 race と解放後の参照

**R1 / Q2 / S1: populate と evict の race (pcache モード)**
- 症状: pcache 経路で読み出しが 0 を返す、静かなデータ破損です (AC-08、AC-11、AC-14)。
- 原因 (R1、`070ff01` の説明): populate が `fd->lock` を離してから PCACHE entry を入れるため、その間に evict が `fd->buf` を解放すると、NULL の buf を指す entry が残りました。S1 (`315b182`) は、lockless な `fc_pcache_check` と `fill_from_kmalloc` の間に evict と別範囲の populate が入り、memset された 0 を読む TOCTOU でした。
- 修正: entry の挿入を `fd->lock` の内側に移し、「PCACHE entry がある ⇔ fd->buf が NULL でない」を不変条件にしました。ロック順は `fd->lock` (外) → `s3fs_admits_lock` (内) です。S1 は `s3fs_admit_gen` の世代を渡して lock 下で再確認します。
- 教訓: これらの不変条件は `docs/kmod-pcache-invariants.md` にまとめ、CLAUDE.md で kmod を触る前に読むことにしています。

**T2 の修正による回帰 (evict race での sha256 不一致)**
- `babaa5e` は 256 MiB 超 (chunk index ≥ 64) が読めない T2 を直すため bitmap の検査を外しましたが、ci < 64 の検査まで外したため、evict race で sha256 が合わなくなりました。-EIO にならず、3 回の retry がすべて buf_valid=true を返す静かな破損です。`e56f6c7` で ci < 64 の検査を戻しました。

**AC-45 C3: shed の最初の版**
- capacity の pass で pending の ino を丸ごと evict する最初の版は、entry のない cached folio を残し、bench-hitrate で hit_rate 84% という異常値を出しました。commit 前に直しています (`4ab4103` の説明、`br §18.21`)。C3 の結果、evict-race の `retry_exhausted` は 124〜160 から 0 になりました。

**その他**
- AC-36 (P1): `pending_exists` が chunk_offset ≠ 0 の pending を見落とし、upcall 待ちの ino を evict し得ました (`5bf11a3`)。
- AC-20 (S10): `admit_snapshot` が PCACHE entry (uaddr=0) を arena hit として返し、GUP で near-NULL にアクセスし得ました。
- AC-24 (T3): evict ioctl が `invalidate_inode_pages2` で folio lock を待ちデッドロックしました。Week 4 (a) にも `.read_folio` と `invalidate_inode_pages2` のデッドロックがあり (`ll §1.3`)、同じ形の問題が二度出ています。

### 4.3 oops とカーネルの警告

- **kvfree 漏れによる oops (AC-55)**: `s3fs_ebpf_entries_replace` が kvcalloc で確保した表を `kfree` で解放していました。表が約 4 MB を超えると vmalloc になり oops します (50k 件で約 15 MB)。`7af6d9f` で `kvfree` に直しました (`br §18.40`)。
- **krealloc の MAX_ORDER 警告 (Week 4 (c)/(e))**: 10 MiB の write で `__alloc_pages_noprof` の WARN が出ました。krealloc は 4 MiB が上限のため、kvmalloc + memcpy に置き換えました (`ll §1.7`)。
- **SYNC_ENTRIES の件数上限**: オブジェクトが 4096 個を超えると startup の sync が `-E2BIG` で失敗し、ブートストラップの 6 件しか見えませんでした (`br §18.26` の 3)。上限を 65536 に上げ (`2ea5473`)、AC-55 で索引を入れて 262144 にしました。

### 4.4 計測の無効化

- **hit-rate 88.5% の無効化 (S 系)**: softcap が容量ではなかったため、W-TinyLFU 88.5% が Belady 上限を超えていました。S-01〜S-02 の修正後は全 policy が 33〜40% に集まりました (`br §0.1`)。
- **bench の集計バグ (U 系、Y 系)**: 失敗した `cat` を hit に数える (U-01)、`bench-write.sh` が最後の書き込みしか S3 で検証しない (U-03)、clat の単位違いで静かに失敗 (U-04)、backend 間で同じ S3 key に書く (U-07)、`rate_total` が 100% を超える (Y-01) など。
- **Q1 修正前ビルドの §16 / §17 (AC-13)**: `a53d229`〜`6443015` のビルドでは evict 後の再 read が EOF になっていた可能性があり、AB-08 の hit rate 94% を含む §16 / §17 は無効と判定されました (`br §18` 冒頭)。pcache の hit rate はその後 §18.1 で 52〜54%、AC-44 後の §18.43 で 29〜35% になっています。
- **timed_reads の問題 (AC-10 / AC-12 / AC-15)**: awk が gawk 専用で µs のパースが誤っていた (Q4)、fallback が静かで計測方法が残らない (R2)、subshell で method の記録が消える (S2)。
- **fio の IOPS 文字列 (`br §18.24`)**: `6293k` を awk に直接渡すと数値として壊れ、それ以前の gate (a) の比較が正しく動いていなかった可能性があります。`hook_ratio` の計算も 1〜3 回目は壊れていました。
- **`/metrics` の取得 (`a9b996b`)**: SIGPIPE で値が 2 行になり、delta=0 の誤 FAIL を出していました。
- **bench が別の bucket に書いていた (`br §18.26`)**: 計測スクリプトが daemon に `--bucket` を渡し忘れ、ebpf の書き込みが `s3fs-test` に入っていました。これで F1 が見つかりました。
- **cold read のメモリ回収 (`br §18.39`)**: ebpf の cold read が bench の中でだけ遅く、読む順番に従って遅くなりました。遅い read だけ kswapd が約 900 MiB を回収しており、daemon の経路ではなくホストの page cache の回収待ちでした。毎回 drop_caches をすると 300 MiB で fuse-raw の 1.26 倍速くなります。

### 4.5 外れた仮説

- **AA-03 / AB-10: GUP と rwlock**: AA-01 は 64 jobs の劣後を admit の spinlock と見て rwlock にしましたが、76% に悪化しました (`br §12.2`)。AB-07 では GUP を除いても 63.9% で、rwlock を除いた AB-10 でも変わりませんでした (`br §15`)。最終的に、差の正体は `fentry/generic_file_read_iter` の負担 (perf で約 19%) で、フックを外すと fuse-raw と同じ IOPS になりました (`br §18.16`)。§15 の時点での「VFS ラッパーと folio lock の競合」という結論も外れていたことになります。
- **large folio 仮説**: 「FUSE は large folio を使うので速い」という仮説は、100 MiB 読みで fuse-cache も ebpf も order 0 が 25,600 件ずつで、6.12 では成り立ちませんでした (`br §18.16`、`docs/design/read-iter-hook-overhead.md`)。gate (a) の fio は 18 B のファイルを読むため、folio はどの FS でも 4 KiB 1 枚です。
- **U10 (buf_readers) 仮説**: §18.2 の性能低下と容量超過の原因を pcache hit 経路の `buf_readers` 増減と見ましたが、A/B では差がノイズの範囲で、容量超過の本当の原因は buf の確保サイズでした (`br §18.10`)。U10 は不要として削除しました (`892e1be`)。
- **arena kern_vm の直接参照 (Week 4 (a))**: kmod から kern_vm を deref できる前提で設計しましたが、generic context の fault handler がなく全て EFAULT でした (`ll §1.1`)。`ll` は、先に完璧な設計を立てていたらこの案で 2 週停滞していたと振り返っています。

---

## 5. 教訓のまとめ

- 性能改善の前に原因を計測で分離する。AA-01/AA-03 と AB-10 は原因を推測して実装し外れ、perf と A/B (`br §18.16`) で初めてフックが原因と分かりました。
- 正しさの不変条件は文書にしてロック順と一緒に守る。R1 / S1 / T2 回帰のような静かな 0 返しは、「PCACHE entry ⇔ fd->buf」の不変条件を明文化してから止まりました。
- 読み込みを省く最適化は write 側の前提を崩しやすい。AC-50 と F1 は、どちらも「何かを後回しにする」設計が flush の中身を壊した例です。
- 修正前のコードで FAIL するテストを作る。AC-48、AC-50、AC-53、AC-64 では修正前 FAIL / 修正後 PASS を確かめており、確かめられなかった AC-63 はその旨を記録しています。
- 計測値は、修正前ビルドやスクリプトのバグで簡単に無効になる。ビルドの commit、計測方法、回数を数値と一緒に残し、無効になったら理由を書いて置き換える (§16 / §17、hit-rate 88.5%)。
- 倍率は比べる相手とキャッシュの条件をそろえて書く。875 倍や write 17 倍は条件の違いが大半で、fuse-cache や ebpf-nocache を対照群にして初めて構造の差が見えました (`br §0.1`)。
- bench の環境はホストの状態 (page cache、メモリ回収、MinIO の同居) に左右される。cold read の遅さはホストのメモリ回収でした (`br §18.39`)。
- 道具は堅牢なものを使う。bash の `wait` で hang した並列 cat は fio に替え (`ll §1.8`)、大きな kmod ファイルは `scripts/fn.sh` で関数単位に読むようにしました。
