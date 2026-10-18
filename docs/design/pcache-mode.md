# pcache モード — ファイルの中身を kmod の chunk と page cache に置く

2026-10-18。`--arena-metadata-only` で有効になる read / write の方式の全体像。2026-10-13 以降の計測と修正はこのモードで行っている (既定は今も arena モード)。ロック順や不変条件の細部は `docs/kmod-pcache-invariants.md` が正本で、ここでは繰り返さない。経緯は `docs/journal/history.md` §2.5〜§2.9。

## 1. なぜ移ったか

arena モードでは、ファイルの中身を `bpf_arena` (daemon と BPF の共有メモリ) に置き、kmod は daemon の arena を GUP で読む (`docs/design/read-path-design.md` Path B)。kmod から見ると arena は「別プロセスのユーザー空間」なので、hot path の read のたびに GUP が要り (1 回約 1.7 µs)、64 並列の read が伸びなかった (AA-03、`br §14`)。arena は BPF とユーザー空間で状態を共有するのには向くが、kmod の hot path から毎回読む場所には向かない (`docs/design/bpf-arena-use-cases.md`)。

そこで中身を kernel 側に置き、2 回目以降の read を普通の page cache で返すことにした (AB-04〜、`1db272e`)。arena には中身を置かない。

## 2. 2 つのモードの違い

| | arena モード (既定) | pcache モード (`--arena-metadata-only`) |
|---|---|---|
| 中身の置き場所 | `bpf_arena` | inode ごとの 4 MiB chunk の表 (`fd->chunks[]`) と page cache |
| admit の ioctl | `ADMIT_ARENA` (uaddr / size / offset の表) | `POPULATE_PAGES` (中身を kmod にコピー) + `S3FS_ADMIT_FLAG_PCACHE` の entry |
| hit の経路 | `.read_iter` が arena から GUP でコピー | `.read_iter` → `generic_file_read_iter`。page cache が空なら `.read_folio` が chunk から folio を埋める |
| prewarm | 中身まで | メタデータだけ。中身は最初の read で取る (AC-44) |
| 容量 | arena の大きさ (128 MiB) | `capacity_bytes` (既定 0 = 無制限) で chunk の確保量を数える |
| 弱み | 毎回の GUP | 不変条件が多い。読んだ page を残すので、大きいファイルを続けて読むとホストのメモリ回収を待つ (`br §18.39`) |

## 3. データ構造

- **chunk の表**: inode の `s3fs_file_data` (fd) が 4 MiB (`S3FS_CHUNK_SIZE`) の chunk の配列を持つ。chunk は触った分だけ、必要な大きさちょうどで確保する (46 B のファイルは 46 B、AC-41 / AC-45 C1)。chunk ごとに `populated` (S3 の中身で埋まった) と `readers` (pin) を持つ。上限は 1 TiB (`S3FS_MAX_CHUNKS`)。
- **PCACHE の admit entry**: kmod の admit hash に `S3FS_ADMIT_FLAG_PCACHE` 付き (uaddr=0) で入る。「この範囲は fd の chunk にある」という印で、R1 (entry があれば chunk は確保済み) で守る。
- **fd ごとの admit cache**: `fc_pcache_check` は `s3fs_admit_gen` が変わっていなければ rwlock を取らずに hit を判定する (AB-10)。
- **pending**: `(ino, chunk_offset)` 単位。chunk ごとに独立して miss の完了を待つ。

## 4. read

1. `.read_iter`: `fc_pcache_check` が PCACHE entry を見つけたら `generic_file_read_iter` に渡す。page cache に folio があればそこで終わり、kmod も daemon も通らない。
2. page cache に folio がないと `.read_folio` が呼ばれる。entry と chunk があり `populated` なら、`s3fs_folio_fill_from_kmalloc` が `fd->lock` 下で chunk から folio にコピーする (S1 の世代確認、T2 の `populated` 確認)。
3. entry がなければ miss。reader は chunk を pin してから pending を作り (この順が成功 signal の契約の前提)、`/dev/s3fs_ebpf` の `read(2)` で daemon に miss を渡して待つ (Path C と同じ upcall)。
4. daemon (`handleReadMiss`) は miss を含む 4 MiB の range GET (4 MiB を超える大きいファイル) か全体 GET をして、`POPULATE_PAGES` で kmod に渡す。kmod は chunk にコピーして `populated` を立て、`fd->lock` を持ったまま PCACHE entry を入れる。全体 admit になる大きいファイルは chunk ごとに並列で取り、取れた chunk から populate して signal する (AC-52、`admit_chunkload.go`)。
5. daemon は `SIGNAL_ADMIT` に `S3FS_SIGNAL_VERIFY_POPULATED` を付けて reader を起こす。chunk がもう evict されていれば kmod は `-EAGAIN` を返し、daemon は populate をやり直す (最大 3 回)。

page cache で返る read も policy に見えるように、`fentry/generic_file_read_iter` が KFunc `s3fs_kfunc_policy_hit` で `on_hit` を呼ぶ (S-03。CPU と ino ごとに jiffy 1 回に間引き、AC-47)。

## 5. 容量と eviction

- 容量は arena の entry のサイズと、pcache の ino ごとの `fd->cap` (確保済み chunk の合計) で数える (AC-41)。
- evict は `admit_replace` の中で行う (`docs/design/admit-and-softcap.md`)。pcache の miss は `admit_replace` を通らないので、`--cache-capacity` があるとき daemon は populate の後、signal の前に `admit_replace` をやり直して上限を守る (AC-45 C2)。
- evict は entry を落としてから page cache を落とし、`s3fs_file_data_reset` で chunk を free する。miss が in-flight の ino は丸ごとは evict せず、pin されていない chunk だけを free する (shed、AC-45 C3)。capacity より大きいファイルを順に読んでも、読み終わった chunk は溜まらない。
- victim の選び方は arena モードと同じ (score 順、または AC-58 の `pick_victim`、`docs/design/ac58-pick-victim.md`)。

## 6. write

- write は fd の chunk に書き、fd を dirty にする。dirty / flushing の fd には PCACHE entry を入れない (AC-57)。close / fsync で flush event を積み、daemon が `READ_DIRTY` で読んで PUT する (`docs/design/component-communication.md` §4)。
- S3 にある既存ファイルへの最初の変更の前に、変更で残る部分を読み込む (AC-50、`s3fs_fd_load_for_modify`)。open は中身を読まないので、これがないと読んでいない範囲が 0 で PUT される。
- `--partial-modify` (既定 off、S3 のみ) のときは、変更のない chunk を読まずに remote として記録し、flush は multipart + `UploadPartCopy` で運ぶ (AC-51、`docs/design/ac51-partial-modify.md`、ioctl `DIRTY_LAYOUT` / `READ_DIRTY_RANGE`)。

## 7. 性能 (ローカル MinIO、`docs/journal/benchmark-results.md`)

- open+read+close は fuse-cache の 172 倍、小さいファイル 10,000 個の warm cat は 93 倍 (§18.26〜18.32)。
- AB-07 の gate (`fd8f772`、§18.43): 64 jobs psync read は fuse-raw 比 0.983 (フックなしの pcache は fuse-raw 以上)、100 MiB の cat は fuse-cache より速い (14.7 ms 対 17.1 ms)、microbench は arena モードより速い。
- 負けている所: 大きいファイルの cold read (300 MiB で fuse-raw の中央値 0.80×、§18.37〜18.39)。既存オブジェクトの途中書き換えは `--partial-modify` で fuse-raw 並み (§18.36)。
- hit rate は §18.43 で 29〜35%。prewarm がメタデータだけになったので、§18.1 の 52〜54% とは条件が違う。並べないこと。

## 8. 正しさ

populate と evict の race による静かなデータ破損 (読み出しが 0 を返す) が続けて見つかり、R1 / S1 / T2、pending の 2 本立ての refcount、成功 signal の契約、chunk の pin を入れて止めた (`docs/journal/history.md` §4)。kmod の read / pcache / eviction を変えるときは、先に `docs/kmod-pcache-invariants.md` を読み、不変条件を変えたら同じ変更でそのメモを更新する。回帰は `make test-regression` (evict-race、capacity、write-existing、pick-victim-stale などを含む)。lockdep での検証はまだしていない (AC-42)。

## 9. 残っていること

- 大きいファイルの cold read (AC-52 の残り): MinIO 側の揺れとホストのメモリ回収。
- 64 jobs の律速の特定 (AC-06)。
- 既定を pcache モードに切り替えるかは決めていない。
