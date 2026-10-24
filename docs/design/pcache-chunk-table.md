# pcache の chunk 表のレイアウト

pcache モードで、ファイルの中身を kmod が持つ形 (`fd->chunks[]`) の説明。全体の流れは [`pcache-mode.md`](pcache-mode.md)、ロック順と不変条件は [`../kmod-pcache-invariants.md`](../kmod-pcache-invariants.md)、ioctl は [`dev-s3fs-ebpf-protocol.md`](dev-s3fs-ebpf-protocol.md)。定義は `kmod/vfs_ops.c` の `struct s3fs_chunk` / `struct s3fs_file_data` (`scripts/fn.sh -l s3fs_fd_` で関連関数を引ける)。

---

## 1. 3 つの層

pcache モードの 1 つのファイル (ino) の中身には、置き場所が 3 つある。

| 層 | 何か | 誰が持つ | 単位 |
|---|---|---|---|
| admit entry | 「ino のこの範囲は chunk にある」という印 (`S3FS_ADMIT_FLAG_PCACHE`、`uaddr = 0`) | kmod 全体の admit hash (`s3fs_admits`、key は ino) | populate 1 回の範囲 (`offset`、`size`) |
| **chunk 表** | S3 から取った中身そのもの | inode の `i_private` にある `struct s3fs_file_data` (fd) | 4 MiB の chunk |
| page cache | アプリの read が実際に読むところ | inode の `address_space` (カーネル標準) | folio (今は 4 KiB) |

read の hit は page cache だけで返る。page cache に folio がないときに `.read_folio` が chunk 表から folio を埋め、chunk もなければ miss として daemon に取りに行く。chunk 表は「page cache が落とされても、S3 に行かずに埋め直せる、kmod の中の 2 段目のキャッシュ」と考えるとよい。

## 2. 構造体

```c
#define S3FS_PCACHE_CHUNK_SHIFT 22u                 /* 4 MiB */
#define S3FS_CHUNK_SIZE         (1UL << 22)
#define S3FS_MAX_CHUNKS         (1u << 18)          /* 表の上限 = 1 TiB */

struct s3fs_chunk {
	u8   *p;          /* 中身 (kvmalloc)。NULL = 未確保 */
	u32   cap;        /* 確保したバイト数 (<= 4 MiB) */
	bool  populated;  /* S3 の中身で埋まっている */
	bool  remote;     /* dirty な fd だけ: S3 と同じで、ここには持たない (AC-51) */
	u32   readers;    /* この chunk を待っている reader の pin 数 */
};

struct s3fs_file_data {          /* inode->i_private */
	struct mutex lock;           /* fd->lock: chunk 表はすべてこの下で触る */
	struct s3fs_chunk *chunks;   /* 表 (kvcalloc) */
	u32   nr_chunks;             /* 表の長さ。要素は空でもよい */
	size_t size;                 /* fd が持つ論理サイズ */
	size_t cap;                  /* 確保済み chunk の cap の合計 */
	bool  dirty, flushing;
	u32   nr_remote;
	/* flush の pin など (省略) */
};
```

## 3. 番地の対応

- chunk `ci` はファイルの `[ci << 22, (ci + 1) << 22)` を受け持つ。ファイル上の位置 `pos` は `ci = pos >> 22`、chunk の中の位置は `pos & (4 MiB - 1)`。
- 表は配列で、穴があってもよい。読んでいない chunk は `p == NULL` のまま。
- 表の長さは、必要な `ci` が入らなくなったときに `max(ci + 1, nr_chunks * 2)` に伸ばす (最小 8、最大 `S3FS_MAX_CHUNKS`)。伸ばすときにコピーするのは表 (要素 24 B ずつ) だけで、中身は動かさない。
- chunk の中身の大きさは、populate なら `min(4 MiB, max(終端, i_size) - chunk の先頭)` ちょうど。46 B のファイルは 46 B、最後の chunk はファイルの残りの分だけになる (AC-41)。write は chunk の中で倍々 (最小 4 KiB) に伸ばす。

### 例: 9 MiB のファイルの末尾だけを読む

```
ファイル   0          4 MiB        8 MiB   9 MiB
           |-- ci 0 ---|--- ci 1 ---|- ci 2 -|
chunks[]   [0] p=NULL   [1] p=NULL   [2] p=1 MiB, populated   [3..7] 空
admit      (ino, offset=8 MiB, size=1 MiB, PCACHE)
fd->cap    1 MiB
```

8.5 MiB からの read が miss すると、daemon は 8 MiB から始まる chunk 1 つ分 (ファイルの残りの 1 MiB) を range GET して `POPULATE_PAGES` を呼ぶ。kmod は chunk 2 だけを 1 MiB 確保してコピーし、entry を入れる。ほかの chunk は確保しない (AC-45 の前は、ファイル全体の 1 本の buffer を確保していた)。

## 4. chunk の状態

| `p` | `populated` | `remote` | 意味 | read の扱い |
|---|---|---|---|---|
| NULL | — | false | 未確保 | clean なら miss (daemon へ)。dirty なら穴として 0 |
| あり | false | false | 確保はしたが S3 の中身で埋まっていない (write で作った、populate が chunk を覆っていない) | clean なら miss。dirty なら fd の中身 |
| あり | true | false | S3 の中身で埋まっている | chunk から folio を埋める |
| どちらでも | — | true | dirty な fd の、変更していない chunk (`--partial-modify`) | `FOR_WRITE` の miss で chunk 全体を読み直す |

- `populated` が立つのは、populate の範囲が chunk の先頭から始まり、chunk の終わりか EOF まで届くときだけ (T5)。
- `readers > 0` の chunk は reset や shed で free しない。miss を待つ reader は、pending を作る前に pos の chunk を pin する (U8)。

## 5. admit entry との関係

- populate は、chunk へのコピーと `populated` を立てるのに続けて、**`fd->lock` を持ったまま** PCACHE の entry を入れる。だから entry があれば、その範囲の chunk は確保済み (R1)。逆は成り立たない (entry を先に落とした chunk が残っていることはある)。
- dirty / flushing の fd には entry を入れない (容量の対象外、AC-57)。
- evict は entry を落としてから page cache を落とし、`s3fs_file_data_reset` が pin されていない chunk を free する。pin された chunk が残れば表と `fd->size` は残す。

## 6. 容量の数え方

- `fd->cap` は確保済み chunk の `cap` の合計。容量の判定 (`capacity_bytes`) は ino ごとにこれを足す (`s3fs_ino_buf_bytes`)。entry の範囲の合計ではない。
- `fd->cap` とグローバルの `s3fs_fd_buf_bytes` を変えるのは `s3fs_fd_chunk_ensure` と `s3fs_fd_chunk_free` の 2 つだけ (M1)。`STATS` の slot 12 と 13 で、全 inode を走査した合計と突き合わせられる。

## 7. 主な関数

| 関数 | すること |
|---|---|
| `s3fs_fd_chunks_grow` | 表の長さを伸ばす |
| `s3fs_fd_chunk_ensure` | chunk を必要な大きさで確保する (中身は残し、範囲外は 0) |
| `s3fs_fd_store` | 範囲を chunk に分けてコピーする (populate は `copy_from_user`、write も同じ経路) |
| `s3fs_ebpf_populate_pages` | `POPULATE_PAGES` の本体。store、`populated`、page cache の該当範囲の invalidate、entry の挿入 |
| `s3fs_folio_fill_from_kmalloc` | `.read_folio` で chunk から folio にコピーする |
| `s3fs_fd_chunk_pin` / `unpin` | `readers` の増減 |
| `s3fs_file_data_reset` / `s3fs_ino_shed_unpinned` | evict と shed で chunk を free する |

1 回の `POPULATE_PAGES` は最大 64 MiB (`S3FS_POPULATE_MAX_SIZE`)。daemon は 4 MiB の chunk 境界から populate する。
