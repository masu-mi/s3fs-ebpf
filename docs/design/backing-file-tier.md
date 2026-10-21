# backing file の段 — 中身をローカル FS のファイルに素通しして、メモリの上限を緩める (案)

2026-10-21。状態: **案。未実装・未計測。** fuse-bpf / FUSE passthrough と同じように、hit したファイルの read をローカル FS のファイル (backing file) に素通しする段を kmod に足す案。目的は、hit の速さ (特に open を daemon に送らないこと) を保ったまま、pcache モードのメモリの制約を緩めること。発表の想定問答では `docs/user/meetup-qa.md` の「メモリの上限を緩めるため、ローカル FS へ素通しする段を足せないか」に要約した。

kmod の read / pcache / eviction の不変条件の正本は `docs/kmod-pcache-invariants.md`。実装に入るときは、この案の §5 で変わる不変条件をそちらに書く。

## 1. 背景: pcache モードのメモリの問題

pcache モード (`docs/design/pcache-mode.md`) は、ファイルの中身を kmod の 4 MiB chunk (`fd->chunks[]`) に持ち、`.read_folio` が chunk から page cache に埋める。速さの源は「hit のときに daemon へ行かない」ことで、open+read+close は fuse-cache の 172 倍 (`docs/journal/benchmark-results.md` §18.27)。一方で、メモリについて次の問題がある。

- **中身が 2 か所に載る**: chunk と page cache の両方に残れば最大 2 倍。
- **カーネルが回収できない**: chunk は `kvmalloc(GFP_KERNEL)` で `__GFP_ACCOUNT` なし、shrinker もない。memcg の制限は chunk を抑えず、メモリが足りなくなっても chunk は減らない。減らすのは `capacity_bytes` と policy だけ。
- **容量の既定は無制限**: ホストのメモリを見ない。300 MiB を続けて読むと、ホストのメモリ回収待ちで cold read が fuse-raw の 0.80 倍に落ちた (§18.39、§18.39.1)。
- **揮発**: umount や daemon の再起動で消え、S3 から取り直す。

daemon はすでにディスクキャッシュ (`--cache-dir`、`daemon/internal/objstore/cache.go` の `DiskCache`) を持っている。miss のとき、kmod の chunk になければ daemon がディスクキャッシュを見て、そこにもなければ S3 から取る。ただし、ディスクキャッシュの中身は毎回 daemon が読んで `POPULATE_PAGES` で chunk にコピーするので、メモリの問題は変わらない。

## 2. 案: kmod が backing file に素通しする

```
read(2) / mmap
  └─ kmod の .read_iter
       ├─ backing の段 (この案): backing file の read_iter に素通し
       │     └─ 下位 FS (ext4 / xfs など) の page cache → ディスク
       └─ pcache の段 (今): generic_file_read_iter → .read_folio が chunk から埋める
```

- daemon は、ディスクキャッシュに中身を書き終えたファイルの fd を ioctl で kmod に渡す (FUSE の `FUSE_DEV_IOC_BACKING_OPEN` と同じ形)。パスを渡すと、kmod が任意のファイルを開かされる攻撃 (fd injection に類するもの) の口になるので、fd で渡し、kmod 側で通常ファイルか、同じ ino の登録かを確かめる。
- kmod の open は、その ino に backing file が登録されていれば、それを開いた `struct file` を自分の file に結びつける。open は daemon に行かないので、open+read+close の速さは保てる見込み。
- read は backing file に素通しする。6.8 以降の `fs/backing-file.c` には、overlayfs と FUSE passthrough (6.9) が使う `backing_file_open` / `backing_file_read_iter` / `backing_file_mmap` などの helper がある。module から使えるか (export されているか) と、6.12 での引数は実装前に確かめる。
- 中身は下位 FS の page cache にだけ載る。kmod の inode の page cache と chunk は使わない。

### 2.1 2 段にする

backing の段だけにすると、page cache から落ちた後の読み直しがディスクの read になる (§6)。そこで次の 2 段を提案する。

| 段 | 置き場所 | 容量 | 使う場面 |
|---|---|---|---|
| hot (pcache) | kmod の chunk + page cache | `capacity_bytes` をホストのメモリの一部に絞る (既定を無制限から変える) | よく読む小さいファイル |
| warm (backing) | ディスクキャッシュのファイル + 下位 FS の page cache | ディスクの大きさ (`--disk-cache-capacity`) | hot から溢れたもの、大きいファイル |

- 段はファイル単位で、open のときに決める (FUSE passthrough と同じ)。read の途中で段を変えない。
- hot から追い出すとき、ディスクキャッシュにそのファイルがあれば warm に降ろす (中身は捨て、backing の登録だけ残す)。
- 大きいファイル (例: `capacity_bytes` の一定割合を超えるもの) は最初から warm にする。§18.39 のメモリ回収待ちは大きいファイルの連続読みで起きたので、これが一番効くはず。

## 3. 経路ごとの扱い

- **read**: 上の図のとおり。warm のファイルは `.read_iter` で素通しし、`generic_file_read_iter` を通らない。read の fentry (`fentry/generic_file_read_iter`、R-01) は warm のファイルでは発火しないので、on_hit は kmod の `.read_iter` の中で数える。これは `docs/design/read-iter-hook-overhead.md` §4.6 の「hit の経路のフックを外す」方向とも合う。
- **mmap**: warm のファイルは `backing_file_mmap` で backing file の mapping を使う。読み出しは動く。書き込み可能な共有 mapping は、今の pcache と同じく扱わない (`MAP_SHARED` + `PROT_WRITE` は拒否するか、warm から外す)。
- **write**: dirty になるファイルは warm に置かない。最初の変更のときに pcache の段に移し (AC-50 の読み込みは backing file から行えるので S3 に行かない)、flush は今のまま。PUT が終わったら daemon がディスクキャッシュを新しい中身で置き換える。
- **truncate / unlink / rename**: unlink はディスクキャッシュのファイルも消す (開いている backing file は Linux の通常の動きで close まで残る)。rename は `DiskCache.InvalidateRename` と同じく、登録の付け替えか削除。
- **一部だけ取得したファイル**: 今のディスクキャッシュはオブジェクト全体を 1 ファイルで持つ (`DiskCache.Write`)。range GET で一部だけ取った大きいファイルを warm に置くには、ファイルを sparse にして、取得済みの範囲の印 (4 MiB 単位の bitmap) を kmod か daemon が持つ必要がある。最初の版では「全体を取得済みのファイルだけ warm」に限る。

## 4. daemon 側の変更

- **キャッシュファイルの形式**: 今は先頭に header (`s3fscache1`、ETag、サイズ) があり、中身はその後ろにある (AC-40)。素通しには中身がオフセット 0 から始まる必要があるので、header を xattr (`user.s3fs.etag` など) か別ファイル (sidecar) に移す。古い形式のファイルは読み込み時に捨てる (今の stale の扱いと同じ)。
- **登録の ioctl**: ファイルを書き終え (rename で確定し) てから、fd と ino、ETag、サイズを kmod に渡す。上書きを検知したら (`DiskCache.Observe` の不一致) 登録を取り消す ioctl を送る。開いている file は古い中身を読み続けるので、取り消しは次の open から効く (今の chunk の evict より一貫性は弱くない: 今も開いている fd の page cache は残る)。
- **ディスクの追い出し**: `DiskCache.evictIfNeeded` は mtime 順の watermark (90% → 70%) で消している。kmod が開いているファイルを消しても壊れないが、容量はすぐには空かない。追い出しの前に kmod の登録を取り消す。

## 5. kmod の変更と不変条件

- inode ごとに「段」(hot / warm) と、warm なら backing の `struct file` を持つ。open で段を決め、file の `private_data` に結びつける。
- R1 (PCACHE entry があれば chunk は確保済み) と T2 (populated の確認) は hot の段だけのものになる。warm の段の不変条件は「登録があれば backing file は全体を取得済みで、ETag が登録時のもの」。
- 段の移動 (hot → warm、warm → hot) は `fd->lock` の下で行い、in-flight の read (pin、pending) がない時だけにする。pcache の shed (AC-45 C3) と同じ考え方。
- policy (struct_ops) の役割は 2 つに分かれる。hot の段で何を追い出すか (今のまま)、warm の段でどのファイルを消すか (今は daemon の mtime 順)。後者を BPF に渡すかは決めていない。最初の版は daemon のまま。
- KUnit (`docs/dev-notes/kunit.md`) に、段の移動と read / truncate / evict の並行のテストを足す。

## 6. 性能の見込み (推測。計測していない)

| 場面 | 今 (pcache) | 案 (warm) |
|---|---|---|
| open+read+close (hit) | 3.05 µs | ほぼ同じ見込み。open で backing file を開く分と、1 層増える分だけ重い (overlayfs と同程度、µs 未満と見ている) |
| page cache で hit する read | kmod の page cache | 下位 FS の page cache。差は小さい見込み |
| page cache から落ちた後の read | chunk から memcpy | ディスクの read (SSD で数十 µs) |
| cold read | S3 の GET (+ 非同期のディスク書き込み、AC-52) | S3 の GET + ディスクへの書き込みを待つ (登録の前に書き終える必要がある)。大きいファイルでは遅くなりうる |
| 大きいファイルを続けて読む | ホストのメモリ回収待ち (0.80×、§18.39) | 下位 FS の page cache は普通に回収されるので改善する見込み |
| daemon の再起動後 | 全部 S3 から取り直す | ディスクキャッシュから返る |

cold read で「書き終えるのを待つ」コストは、取得しながら hot の段で返し、書き終えたら次の open から warm にする、で避けられる。

## 7. 将来の検証項目

実装した場合に確かめること。gate の形は AB-07 (`scripts/bench-ab-pcache.sh`) に倣う。

1. **hit の速さを保つ**: microbench の open+read+close (§18.27 と同じ条件) で、warm のファイルが hot の 1.1 倍以内。fuse-cache 比の倍率がどこまで残るか。
2. **並列の read**: 64 jobs の psync read (gate (a) と同じ条件) で、warm がフックなしの hot の 0.98 倍以上。
3. **メモリ**: 300 MiB を 5 個続けて読む §18.39 の手順で、kswapd の回収と direct reclaim の stall が fuse-raw と同程度になる。memcg を絞ったコンテナの中で読んで、中身のメモリが memcg に課金され、上限を超えないこと。
4. **page cache から落ちた後**: `drop_caches=1` の後の warm read の遅延 (hot の chunk からの読み直しとの差)。ディスクの種類 (NVMe / SSD) を記録する。
5. **cold read**: §18.37 の 10 / 100 / 300 MiB で、warm に登録するまでのディスク書き込みが cold read を遅くしないこと。
6. **再起動後**: daemon を再起動した直後の read が S3 に行かないこと (MinIO の GET の数で確かめる)。
7. **正しさ**: `make test-regression` 一式と、段の移動と evict、truncate、上書きの並行で sha256 が一致すること。他のクライアントが上書きしたとき、登録の取り消し後の open で新しい中身が見えること。
8. **ディスクの追い出し**: 開いている warm のファイルを daemon が追い出しても read が失敗せず、close の後に容量が戻ること。

## 8. 手順案

1. `fs/backing-file.c` の helper が 6.12 の module から使えるかを確かめる (export、引数、必要な `CONFIG`)。
2. daemon のキャッシュファイルの形式を変える (header を xattr へ)。今の機能のまま入れられる。
3. kmod に warm の段だけを入れる (全体を取得済みのファイル、read と read-only mmap)。write は今の pcache の段に移す。検証項目 1〜4、7 を測る。
4. `capacity_bytes` の既定をホストのメモリから決め、hot から warm へ降ろす処理を入れる。検証項目 3、5、8。
5. 必要なら、一部だけ取得したファイルを warm に置く (sparse + bitmap)。

## 9. 決めていないこと

- warm の段のファイル選び (どれを消すか) を struct_ops の policy に渡すか。
- 大きいファイルを最初から warm にするしきい値。
- arena モードとの関係。この案は pcache モードの上に足すもので、arena モードには入れない想定。
- GCS backend でも同じに動くか (ディスクキャッシュは両方の backend が使っているので、動くはず)。

## 関連

- `docs/user/meetup-qa.md` — fuse-bpf との違い、pcache とディスクキャッシュの違い、メモリの制限と memcg の問答
- `docs/design/pcache-mode.md` — 今の pcache モード
- `docs/journal/benchmark-results.md` §18.27 / §18.39 — open+read+close と、メモリ回収待ち
