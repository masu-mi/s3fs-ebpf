# v2 spec モデル

`../redesign-v2-simplify.md` §6 の規則 (Q-1〜Q-3、D-3、F-1〜F-4) を、全状態を探索して検証する小さなモデル。Python 3 だけで動く (依存なし)。

```
make spec-check                         # 全シナリオ。期待と違う結果があれば失敗
python3 docs/design/v2-spec/run.py -v   # 反例の trace も出す
```

| ファイル | 内容 |
|---|---|
| `modelcheck.py` | BFS の探索器。不変条件違反、deadlock、最終状態の検査、最短の反例 |
| `readmiss_model.py` | read の miss: reader / daemon の handler / evictor。現行と v2 の設定、pin / verify などの変異 |
| `flush_model.py` | flush: writer / close / fsync / daemon。枠 (slot) と `flush_read_seq`、変異 |
| `run.py` | 14 シナリオと期待の結果 (`ok` か `VIOLATION`)。反例が出るべきものが ok になったら、モデルが弱くなったので失敗にする |

`python3 -I` は使わない (`-I` はスクリプトと同じディレクトリの import も外す)。

見ていないこと (lock、複数 inode、partial modify、DELETE / RENAME の順序、daemon の障害) と、規模を縮小していることは、設計 doc の §6.4。
