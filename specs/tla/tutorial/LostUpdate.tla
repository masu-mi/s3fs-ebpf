---------------------------- MODULE LostUpdate ----------------------------
(*
 * 入門記事 (docs/dev-notes/tlaplus-introduction.md) の教材。
 * 2 つのプロセスが共有カウンタを +1 する。読んで、書く、の 2 ステップに
 * 分かれていると、片方の更新が消える (lost update)。
 *   Atomic = FALSE : 読み (Read) と書き (Write) が別のステップ。 -> 違反する
 *   Atomic = TRUE  : 読んで書くまでが 1 ステップ。              -> 満たす
 *)
EXTENDS Naturals, FiniteSets

CONSTANTS Procs, Atomic

VARIABLES counter,   \* 共有カウンタ
          pc,        \* 各プロセスの位置: "read" -> "write" -> "done"
          tmp        \* 各プロセスが読んだ値 (手元のコピー)

vars == <<counter, pc, tmp>>

Init ==
    /\ counter = 0
    /\ pc  = [p \in Procs |-> "read"]
    /\ tmp = [p \in Procs |-> 0]

Read(p) ==
    /\ pc[p] = "read"
    /\ tmp' = [tmp EXCEPT ![p] = counter]
    /\ pc'  = [pc EXCEPT ![p] = "write"]
    /\ UNCHANGED counter

Write(p) ==
    /\ pc[p] = "write"
    /\ counter' = tmp[p] + 1
    /\ pc'      = [pc EXCEPT ![p] = "done"]
    /\ UNCHANGED tmp

Inc(p) ==                     \* Atomic 版: 読んで書くまでを 1 ステップで
    /\ pc[p] = "read"
    /\ counter' = counter + 1
    /\ pc'      = [pc EXCEPT ![p] = "done"]
    /\ UNCHANGED tmp

AllDone == \A p \in Procs : pc[p] = "done"

Next ==
    \/ \E p \in Procs : IF Atomic THEN Inc(p) ELSE (Read(p) \/ Write(p))
    \/ (AllDone /\ UNCHANGED vars)      \* 終わったら止まる (deadlock と区別するため)

Spec == Init /\ [][Next]_vars

\* 全員が終わったら、カウンタは Procs の数に等しい。
Correct == AllDone => counter = Cardinality(Procs)
=============================================================================
