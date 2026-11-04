# daemon の disk cache (`--cache-dir`) の対象サイズと chunk 単位の cache (AC-75)

## 仕様

| object のサイズ | 取り方 | disk cache の単位 |
|---|---|---|
| 4 MiB 以下 (`partialAdmitThreshold`) | `Get` (全体) | object 全体。key は `bucket/key` |
| 4 MiB 超 | chunk ごとの `GetRange` | chunk。key は `bucket/key NUL range:<offset>:<size>` |

どちらも cache file の先頭に header (`s3fscache1`、ETag、本体のバイト数) を持つ。

以前は `GetRange` が disk cache を読みも書きもしなかったため、4 MiB を超える object は kmod の chunk 表から落ちると `--cache-dir` があっても毎回 S3 に戻っていた (64 MiB: upcall 288 回に対して S3 GET 1136 回)。

## 古い bytes を返さない条件

- chunk を読むのは、その object の ETag を `List` / `Stat` で観測済みのときだけ。未観測なら cache を使わず、従来どおり S3 から取る (全体 `Get` が未観測の key を信頼するのとは違う。chunk は ETag なしでは由来を確かめられない)。
- chunk は `If-Match: <ETag>` で取る。cache に入る bytes は、header の ETag の object のものに限る。
- 読むときは header の ETag が観測中の ETag と一致し、サイズが要求と一致することを確かめる。違えば file を消して S3 に戻る。
- `If-Match` が 412 (別の人が上書き済み) なら、その読みは cache せず新しい内容を返す。
- `Put` / `Delete` / `Rename` は、その key の観測 ETag を忘れる。次の `List` / `Stat` が新しい ETag を教えるまで chunk は cache を使わない。
- 古い ETag の chunk file は列挙できないので、読まれて捨てられるか、mtime 順の eviction (`--max-cache-bytes`) で消える。

## 書き込み

write-behind (`maxAsyncWrites` = 4)。全体 `Get` の `WriteAsync` は枠が埋まっていれば書き込みを捨てるが、chunk の `WriteRangeAsync` は枠が空くまで待つ。64 MiB は 16 chunk が同時に来るので、捨てると次の読みが S3 GET になる。待つ間は呼び出し側 (chunk の取得) が遅れる。

## 対象外

- GCS の `GetRange` は未対応 (cache は全体 `Get` のみ)。
- chunk のサイズを変えた (`partialAdmitChunk` を変えた) あとの古い chunk は、読まれず eviction で消えるのを待つ。

## テスト

- `TestDiskCache_Range` (`daemon/internal/objstore`): ETag の不一致、別 chunk、Invalidate。
- `TestGetRange_DiskCache` (MinIO 付き): object を消しても cache から返る、上書き後は新しい内容。
- `TestGetRange_LargeObjectSecondReadNoGET` (MinIO 付き): 64 MiB を 4 MiB ずつ 2 回読み、2 回目の S3 GET が 0 回。
