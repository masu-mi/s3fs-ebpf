# capacity-pass-20261106

`benchmark-results.md` §18.53 の生データ。`scripts/bench-capacity-pass.sh` が出す `result-<commit>.txt`。
列: `cap_mode ncold inodes_delta n min_ns p50_ns p90_ns max_ns`。

- `3b-r*.txt`: Phase 3b (3d3bd2a、admit hash 版)。`3c-r*.txt`: Phase 3c (3a3a538)。各 2 run。
- `exp-*.txt`: 走査の変種 (使い捨ての worktree、コミットなし)。`V0` = 3c そのまま、`V1` = 走査から inode ごとの `i_lock` を外す、`V2` = 2 回目の走査の中身を空にする、`V4` = 2 回目の走査を丸ごと外す (数えるだけ)。cold 10,000 / PASSES=100。
