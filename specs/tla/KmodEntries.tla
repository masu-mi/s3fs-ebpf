----------------------------- MODULE KmodEntries -----------------------------
(*
 * What kmod's lookup shows for one directory while the daemon applies the user's
 * operations to S3 later and a sync replaces the S3-derived table at any time
 * (backlog AC-85). See docs/design/kmod-entry-table-state-machine.md.
 *
 * kmod keeps for a directory:
 *   local  entries kmod made itself (create; the new name of a rename); looked
 *          up first;
 *   dyn    the last S3 listing pushed by the daemon's sync, read-only;
 *   tomb   (fixed design) entries that rename / unlink removed, which lookup
 *          must skip even though dyn may still list them.
 * Every local entry and tombstone remembers `k`: how many queued operations were
 * ahead of (and including) the one that made it. k = 0 means the daemon has
 * applied that operation to S3 — what the daemon reports to a sync as "applied up
 * to seq W". A listing taken at that point reflects the operation.
 *
 * `want` is the truth the user expects (each operation applied at once); the
 * daemon applies the same operations to S3 later, in kmod's order (AC-84).
 *
 * Variants:
 *   Tombstone    rename / unlink hide the old name. FALSE = the code as of AC-84.
 *   DropRule     when a sync drops a tombstone:
 *                  "always"  every sync clears them all;
 *                  "absent"  when the listing no longer has (name, ino);
 *                  "applied" when the listing no longer has (name, ino) AND the
 *                            daemon had applied the operation (k = 0).
 *   PromoteRule  when a sync drops a local entry because the listing "has" it:
 *                  "listed"  when the listing has (name, ino) (the code today);
 *                  "applied" when it has (name, ino) AND k = 0.
 *
 * The code now is the variant `tombstone` (Tombstone, DropRule "applied",
 * PromoteRule "applied"); the others record the designs the model rejected.
 *)
EXTENDS Naturals, Sequences, FiniteSets

CONSTANTS Names, Inodes, NONE, Tombstone, DropRule, PromoteRule, MaxQ

VARIABLES want,   \* name -> ino the user expects
          local,  \* kmod local entries: name -> [ino, k] or NONE
          dyn,    \* last S3 listing: name -> ino or NONE
          tomb,   \* hidden entries: set of [name, ino, k]
          s3,     \* S3: name -> ino whose bytes it holds
          used,   \* inodes ever allocated
          q       \* operations kmod queued for the daemon, in order

vars == <<want, local, dyn, tomb, s3, used, q>>

LocalEntry == [ino : Inodes, k : 0..MaxQ]

TypeOK ==
    /\ want \in [Names -> Inodes \cup {NONE}]
    /\ local \in [Names -> LocalEntry \cup {NONE}]
    /\ dyn \in [Names -> Inodes \cup {NONE}]
    /\ s3 \in [Names -> Inodes \cup {NONE}]
    /\ tomb \subseteq [name : Names, ino : Inodes, k : 0..MaxQ]

Init ==
    /\ want  = [n \in Names |-> IF n = "a" THEN 1 ELSE NONE]
    /\ local = [n \in Names |-> NONE]
    /\ dyn   = [n \in Names |-> IF n = "a" THEN 1 ELSE NONE]
    /\ tomb  = {}
    /\ s3    = [n \in Names |-> IF n = "a" THEN 1 ELSE NONE]
    /\ used  = {1}
    /\ q     = <<>>

Hidden(n, i) == \E p \in tomb : p.name = n /\ p.ino = i

\* resolve_entry_by_name: local first, then dyn unless hidden.
View(n) ==
    IF local[n] # NONE THEN local[n].ino
    ELSE IF dyn[n] # NONE /\ ~Hidden(n, dyn[n]) THEN dyn[n]
    ELSE NONE

\* The operation being queued is at position Len(q) + 1.
Tomb(n, i) == IF Tombstone THEN {[name |-> n, ino |-> i, k |-> Len(q) + 1]} ELSE {}

\* -- the user's operations (kmod applies them at once, queues them for the daemon)
Create(n, i) ==
    /\ Len(q) < MaxQ
    /\ View(n) = NONE /\ i \in Inodes \ used          \* lookup was negative
    /\ want' = [want EXCEPT ![n] = i]
    /\ local' = [local EXCEPT ![n] = [ino |-> i, k |-> Len(q) + 1]]
    /\ used' = used \cup {i}
    /\ q' = Append(q, [op |-> "flush", name |-> n, ino |-> i])
    /\ UNCHANGED <<dyn, tomb, s3>>

Unlink(n) ==
    /\ Len(q) < MaxQ
    /\ View(n) # NONE
    /\ want' = [want EXCEPT ![n] = NONE]
    /\ local' = [local EXCEPT ![n] = NONE]
    /\ tomb' = tomb \cup Tomb(n, View(n))
    /\ q' = Append(q, [op |-> "delete", name |-> n, ino |-> View(n)])
    /\ UNCHANGED <<dyn, s3, used>>

\* Rename f -> t (t does not exist): the inode gets a local entry at t, the old
\* name is hidden. A tombstone (t, i) from an earlier rename away (mv a c; mv c a)
\* must not hide the name that now holds i again.
Rename(f, t) ==
    /\ Len(q) < MaxQ
    /\ View(f) # NONE /\ View(t) = NONE
    /\ LET i == View(f)
       IN /\ want' = [want EXCEPT ![f] = NONE, ![t] = i]
          /\ local' = [local EXCEPT ![f] = NONE, ![t] = [ino |-> i, k |-> Len(q) + 1]]
          /\ tomb' = {p \in tomb : ~(p.name = t /\ p.ino = i)} \cup Tomb(f, i)
          /\ q' = Append(q, [op |-> "rename", name |-> f, to |-> t, ino |-> i])
    /\ UNCHANGED <<dyn, s3, used>>

\* -- the daemon (serialized, in kmod's order): applies the head operation to S3
Dec(k) == IF k > 0 THEN k - 1 ELSE 0

Daemon ==
    /\ q # <<>>
    /\ LET e == Head(q)
       IN CASE e.op = "flush"  -> s3' = [s3 EXCEPT ![e.name] = e.ino]
            [] e.op = "delete" -> s3' = IF s3[e.name] = e.ino THEN [s3 EXCEPT ![e.name] = NONE] ELSE s3
            [] e.op = "rename" -> s3' = IF s3[e.name] = e.ino
                                        THEN [s3 EXCEPT ![e.to] = e.ino, ![e.name] = NONE]
                                        ELSE s3
    /\ q' = Tail(q)
    /\ local' = [n \in Names |-> IF local[n] = NONE THEN NONE
                                 ELSE [ino |-> local[n].ino, k |-> Dec(local[n].k)]]
    /\ tomb' = {[name |-> p.name, ino |-> p.ino, k |-> Dec(p.k)] : p \in tomb}
    /\ UNCHANGED <<want, dyn, used>>

\* syncEntriesOnce -> s3fs_ebpf_entries_replace
Sync ==
    /\ dyn' = s3
    /\ local' = [n \in Names |->
                   IF local[n] # NONE /\ s3[n] = local[n].ino
                      /\ (PromoteRule = "listed" \/ local[n].k = 0)
                   THEN NONE ELSE local[n]]
    /\ tomb' = CASE DropRule = "always"  -> {}
              [] DropRule = "absent"  -> {p \in tomb : s3[p.name] = p.ino}
              [] DropRule = "applied" -> {p \in tomb : ~(p.k = 0 /\ s3[p.name] # p.ino)}
    /\ UNCHANGED <<want, s3, used, q>>

Next ==
    \/ \E n \in Names, i \in Inodes : Create(n, i)
    \/ \E n \in Names : Unlink(n)
    \/ \E f, t \in Names : Rename(f, t)
    \/ Daemon \/ Sync

Spec == Init /\ [][Next]_vars

\* -- properties -----------------------------------------------------------------
\* lookup shows exactly what the user expects
Consistent == \A n \in Names : View(n) = want[n]

\* once the daemon has caught up, S3 holds what the user expects
QuiescentConsistent == (q = <<>>) => \A n \in Names : s3[n] = want[n]
==============================================================================
