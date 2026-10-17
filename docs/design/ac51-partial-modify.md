# AC-51: 既存の大きいオブジェクトの途中の書き換え — 変更のない部分を運ばない

状態: **承認済み (2026-10-17)**。§9 の 4 点は推奨どおり (412 は取り直して PUT、閾値 16 MiB、ETag は最初の変更で記録、GCS は対象外)。実装の順序 (§10) の 1 から進める。実装するときは同じ変更で `docs/kmod-pcache-invariants.md` の「既存ファイルへの最初の変更 (AC-50)」を更新する。

## 1. 問題

`docs/journal/benchmark-results.md` §18.29: オブジェクトの真ん中に 4 KiB を書いて fsync すると、100 MiB で ebpf 389 ms、fuse-raw 158 ms (0.41×)。

原因は AC-50 の設計にある。

- `open(2)` は中身を読まず、flush は fd の全体を PUT する。
- そのため clean → dirty の最初の変更の前に、変更で残る部分をすべて fd に読み込む (`s3fs_fd_load_for_modify`)。
- 100 MiB の 4 KiB 書き換えで、100 MiB の GET と 100 MiB の PUT が発生する。

## 2. s3fs-fuse は何をしているか (確認済み)

MinIO の API trace (`mc admin trace`) で、s3fs-fuse が 100 MiB の `mid.bin` の 50 MiB の位置に 4 KiB を書いて fsync したときの呼び出しを見た (2026-10-17)。

| 順 | 呼び出し | 範囲 |
|---:|---|---|
| 1 | `GetObject` | `bytes=52432896-57671679` (約 5 MiB。書き換えを含む範囲) |
| 2 | `NewMultipartUpload` | |
| 3 | `PutObjectPart` part 2 | 5 MiB (取得した範囲に 4 KiB を重ねたもの) |
| 4 | `CopyObjectPart` part 1 | `X-Amz-Copy-Source-Range: bytes=0-52428799` |
| 5 | `CopyObjectPart` part 3 | `X-Amz-Copy-Source-Range: bytes=57671680-104857599` |
| 6 | `CompleteMultipartUpload` | |

変更のない 95 MiB は server-side copy で、クライアントを通らない。運ぶのは 5 MiB の GET と 5 MiB の PUT だけ。この方式を kmod + daemon で再現する。

## 3. 方針

「変更のない範囲は、取り込まず、PUT せず、server-side copy にする」。

1. 最初の変更で、**書き込みが一部だけにかかる chunk** (書き込み範囲の両端) だけを読み込む。それ以外の chunk は fd に持たず、「元のオブジェクトのまま」と記録する (**remote** chunk)。
2. flush は multipart upload にする。remote chunk の連続は `UploadPartCopy`、fd が持つ chunk は `UploadPart` で送る。
3. copy 元は、変更を始めた時点のオブジェクトの ETag (`x-amz-copy-source-if-match`) で固定する。別のクライアントが書き換えていたら copy は 412 で失敗する。

## 4. 不変条件の変更

現在 (AC-50): **clean → dirty のとき、変更で残る部分は fd にすべて持つ。**

変更後: **dirty な fd の各 chunk は次のどれか。**

| 状態 | 意味 | メモリ | 読み出し | flush |
|---|---|---|---|---|
| data | fd が内容を持つ (`chunks[ci].p != NULL`、populated または書き込み済み) | あり | fd から | `UploadPart` |
| hole | 元のオブジェクトの終端より先で、書かれていない | なし | 0 | `UploadPart` (0) |
| **remote** | 元のオブジェクトのまま。fd に持たない | なし | upcall で取得 (§6) | `UploadPartCopy` |

- 書き込みが **一部だけにかかる chunk** は、書く前に data でなければならない (今の「最初の変更の前に全部持つ」を chunk 単位に一般化したもの)。全部上書きする chunk は、読まずに data にしてよい (AC-50 と同じ)。
- 縮小 truncate: 新しい終端が入る chunk は data、それより後ろの chunk は捨てる。拡張: 新しい範囲は hole。
- remote は **dirty な fd にだけ** 存在する。flush が成功して clean に戻ると、全 chunk が「元のオブジェクトと同じ内容」になるので、remote は「未取得の clean chunk」と区別がなくなる (消す)。
- 小さいファイル (§7 の閾値より下) は今のまま全部持つ。remote の chunk が 1 つもなければ、今の `PUT` の経路をそのまま使う。

kmod の dirty 側の不変条件 (reset / shed は dirty / flushing の fd に触れない、pin の保持、`fd->lock` の順序) は変えない。remote の chunk はメモリを持たないので、capacity の計算 (`fd->cap`) にも入らない。

## 5. kmod の変更

`struct s3fs_chunk` に remote の印を足す (`bool remote`)。`fd->size` が元のサイズより大きいときの、元の終端より先は hole (`p == NULL && !remote`)。

| 関数 | 変更 |
|---|---|
| `s3fs_fd_load_for_modify` | 必要な chunk を「書き込みが一部だけにかかる chunk」に絞る。それ以外で元のサイズ内にある chunk は `remote = true` にする (閾値以上のときだけ)。読み込みの upcall は今と同じ (`S3FS_MISS_FLAG_FOR_WRITE`) だが、daemon が最初の応答で **元のオブジェクトの ETag を記録する** (§8) |
| `s3fs_fd_modify_ready_locked` | 「全 chunk が populated」ではなく、「書き込みが一部だけにかかる chunk が data」を確かめる。remote の chunk を全部上書きするなら、remote を外して data にする |
| `s3fs_fd_store` | 書き込む chunk の `remote` を外す (書き込み先は data になっている) |
| `s3fs_ebpf_file_setattr` | 縮小では境界の chunk を data にしてから、後ろの chunk を free する (remote の印も消す)。拡張では、元のサイズより先は hole |
| 読み出し (`s3fs_ebpf_read_folio` と pcache の T1/T2 判定) | dirty な fd で remote の chunk に当たったら、今の「hole は 0」ではなく upcall に進む (§6) |
| `s3fs_ebpf_read_dirty` | chunk の範囲を指定して読めるようにする (data の chunk だけ)。新しい ioctl `S3FS_IOC_DIRTY_LAYOUT` が chunk ごとの状態 (data / hole / remote) と元のサイズを返す |
| flush の完了 (`signal_flush`) | 成功したら remote の印を全部消す |
| モジュールパラメータ | `partial_modify` (bool)。daemon の backend が multipart copy を持つときだけ 1 にする。0 のときは今の挙動 (全部持つ) |

UAPI (`include/s3fs_ebpf_uapi.h`) は追加だけ: `S3FS_IOC_DIRTY_LAYOUT`、`read_dirty` の範囲指定。

## 6. dirty な fd の remote chunk を読む

dirty な fd は今、populate を受け付けない (daemon は -EBUSY)。remote の chunk を読むには、その chunk を data にする必要がある。

- 読み出しで remote の chunk に当たったら、`S3FS_MISS_FLAG_FOR_WRITE` と同じ形の miss event を出す (chunk を pin してから pending を作る、daemon が `S3FS_SIGNAL_VERIFY_POPULATED` 付きで signal する、という既存の契約をそのまま使う)。
- daemon は、dirty な fd でも **remote の chunk に限り** populate を許す (kmod が remote の chunk にだけ受け付ける。data の chunk に上書きしない)。取得は ETag を固定した範囲 GET (`If-Match`)。
- populate された chunk は remote を外して data にする。flush では `UploadPart` で送る (元の内容と同じだが、すでに fd にあるので copy にしない。単純さを優先)。

読み出しの頻度は低い想定 (書いた直後に別の場所を読む場合)。性能より正しさを優先する。

## 7. daemon の変更 (multipart flush)

`handleFlush` が `S3FS_IOC_DIRTY_LAYOUT` を呼び、remote の chunk がなければ今の `Put`、あれば multipart にする。

part の組み立て (S3 の制約: 最後以外の part は 5 MiB 以上、最大 10,000 part、1 part は 5 GiB 以下):

1. 連続する remote の chunk を 1 つの run にする。run が 5 MiB 以上 (= 2 chunk 以上) なら `UploadPartCopy` (1 part が 5 GiB を超えるなら分割)。
2. run が 1 chunk (4 MiB) だけのときは、その chunk を GET して、隣の data の part に足す (4 MiB の copy は 5 MiB に満たない)。
3. data / hole の chunk の連続は `UploadPart`。5 MiB に満たないときは、隣の remote の chunk (の先頭または末尾) を範囲 GET して足す。
4. 最後の part だけは 5 MiB 未満でよい。

4 KiB の書き換えは、書いた chunk (4 MiB) + 隣の remote の chunk 1 つ (4 MiB) = 8 MiB の `UploadPart` と、前後の `UploadPartCopy` 2 つになる。fuse-raw の 5 MiB に近い。

`Backend` に任意のインターフェイスを足す。

```go
// MultipartCopier: backend が server-side copy 付きの multipart upload を持つ。
type MultipartCopier interface {
    PutWithCopy(ctx context.Context, key, ifMatchETag string, parts []Part) (newETag string, err error)
}
```

- S3: `minio.Core` の `NewMultipartUpload` / `CopyObjectPart` (metadata に `x-amz-copy-source-if-match` を渡す) / `PutObjectPart` / `CompleteMultipartUpload`。失敗したら `AbortMultipartUpload`。
- GCS: `UploadPartCopy` に当たるものがない。compose で作れるが、今回は対象外。GCS では `partial_modify=0` のまま (今の挙動)。
- 成功したら `Put` と同じく disk cache を invalidate し、`CompleteMultipartUpload` が返す ETag を ino の記録に更新する。

## 8. ETag の固定と 412 のとき

**固定**: kmod は ETag を持たない。最初の変更の読み込み (§5) で、daemon が `Stat` (または最初の範囲 GET の応答) の ETag を ino に記録する。flush の copy はこの ETag に `If-Match` を付ける。書き込みが chunk 境界にそろっていて読み込みが一度も起きないときは、最初の変更の時点で ETag だけを記録する小さな upcall (Count = 0) を出す。**代案**: sync が持つ ETag (最大 sync 間隔の窓がある) を使う。upcall が要らない代わりに、固定の保証が弱い。

**412 (copy 元が変わった)**: 別のクライアントが途中で上書きしていた。選択肢は §9 の判断 1。

## 9. 実装の前に決めること

1. **412 のときの動き**。(a) 現在のオブジェクトから remote の範囲を取り直して、書いた部分を重ねて PUT する (last writer wins。AC-50 以前と同じ)。(b) フラッシュを -EIO で失敗させ、`markDirty` で再試行に任せる (remote の元がないので永久に失敗する)。**推奨は (a)**。計画書の「全体の load にフォールバック」はこれ。s3fs-fuse は `If-Match` を付けないので、(a) でも s3fs-fuse より安全。
2. **閾値**。remote を使うのは元のサイズが何 MiB 以上か。**推奨 16 MiB** (これ未満は multipart の往復 3〜4 回のほうが高い)。実測で決め直す。
3. **ETag の固定方法**。§8 の「最初の変更で記録する」 (推奨) か、sync の ETag か。
4. **GCS**。今回は対象外 (`partial_modify=0`) でよいか。

## 10. 実装の順序

1. daemon: `MultipartCopier` と part の組み立て (純粋関数にして単体テスト)。MinIO に対する統合テスト (`CopyObjectPart` の範囲、5 MiB の境界、412)。kmod に触れない。
2. kmod + UAPI: remote の状態、`DIRTY_LAYOUT`、`load_for_modify` の絞り込み、`modify_ready` の chunk 単位化、setattr。`partial_modify=0` で今と同じ挙動のままにしておく。
3. 読み出し (§6)。
4. invariants メモの更新、テスト、§18.29 の測り直し。

## 11. テスト

- `test-write-existing` (pcache + arena) と `test-write-existing-capacity` が全件 PASS (退行なし)。
- 新規: 100 MiB の真ん中 / 先頭 / 末尾 / chunk 境界をまたぐ書き込み、書いた直後に remote の範囲を読む、縮小 truncate、拡張、追記、flush の前に別クライアントが上書き (412 のフォールバック)、flush の途中で失敗 (abort と再試行)。sha256 を全件比べる。
- multipart の part 数・範囲は MinIO の API trace で確かめる (§2 と同じ手順)。
- 目標: §18.29 の 1 / 10 / 100 MiB で、100 MiB が fuse-raw 以上。

## 12. リスク

- remote の chunk が増えるぶん、「fd に持たない部分が flush までに別の場所で変わる」可能性に ETag 固定だけで耐える。固定が外れる経路 (sync が ETag を更新する、daemon の再起動) を実装中に確認する。
- daemon が flush の途中で落ちると、未完了の multipart upload がバケットに残る。bucket の lifecycle (`AbortIncompleteMultipartUpload`) を `docs/user` に書く。
- 読み出し (§6) が dirty な fd の新しい miss の経路になる。`docs/kmod-pcache-invariants.md` の「成功 signal の契約」と pin の説明を合わせて更新する。
